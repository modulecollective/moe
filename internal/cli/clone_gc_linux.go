//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func cloneInventory(root string) ([]string, error) { return clonereclaim.Inventory(root) }

func collectClone(root, p, id string, apply, retry bool, now time.Time, stdout, stderr io.Writer) cloneResult {
	result := cloneResult{Project: p, Run: id, Path: sandbox.Path(root, p, id), Outcome: "held", Reason: "owner_busy"}
	h, err := runowner.Acquire(root, p, id)
	if err != nil {
		result.Detail = err.Error()
		if !runowner.IsBusy(err) {
			result.Outcome = "failed"
			result.Reason = "owner_probe_failed"
		}
		return result
	}
	defer h.Release()
	return disposeClone(h, apply, retry, "terminal", now, stdout, stderr)
}

// disposeClone is the shared terminal release/collector/rebind transaction.
// Hooks and scans run outside repolock; each phase retains the explicit owner.
func disposeClone(h *runowner.Handle, apply, retry bool, purpose string, now time.Time, stdout, stderr io.Writer) cloneResult {
	result := cloneResult{Project: h.Project, Run: h.Run, Path: sandbox.Path(h.Root, h.Project, h.Run), Outcome: "held", Reason: "recovery_required"}
	fail := func(reason string, err error) cloneResult {
		result.Outcome = "failed"
		result.Reason = reason
		result.Detail = err.Error()
		return result
	}
	hold := func(reason string, err error) cloneResult {
		result.Reason = reason
		if err != nil {
			result.Detail = err.Error()
		}
		return result
	}
	md, err := cloneRunState(h)
	if err != nil {
		return hold("lifecycle_evidence_missing", err)
	}
	if md.Workspace != "" {
		return hold("named_workspace", nil)
	}
	basis := clonereclaim.DisposalBasis{Purpose: purpose}
	if purpose == "terminal" {
		if !run.IsTerminal(md.Status) {
			return hold("nonterminal", nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		evidence, err := run.LatestTerminalTransition(ctx, h.Root, md, now)
		cancel()
		if err != nil {
			return hold("terminal_history_unproven", err)
		}
		result.Terminal = &evidence
		basis.Terminal = evidence
		basis.Expired = evidence.RetentionExpired(now)
	} else if md.Status != run.StatusInProgress {
		return hold("not_rebindable", nil)
	}
	registries, err := clonereclaim.Registries(h)
	if err != nil {
		return hold("generation_evidence_invalid", err)
	}
	defer func() {
		for _, r := range registries {
			r.Close()
		}
	}()
	for _, r := range registries {
		pending, yes, err := r.PendingRemoval()
		if err != nil {
			return fail("receipt_probe_failed", err)
		}
		if !yes {
			continue
		}
		result.Identity = r.Identity
		result.Receipt = r.Receipt()
		if purpose != pending.Basis.Purpose {
			return hold("pending_removal", nil)
		}
		if err := r.InspectRemoval(h, pending.ID, basis); err != nil {
			return hold("pending_removal_unproven", err)
		}
		result.Outcome = "eligible"
		result.Reason = "pending_removal"
		if !apply {
			return result
		}
		if err := r.FinishRemoval(h, pending.ID, basis); err != nil {
			return fail("removal_failed", err)
		}
		result.Outcome = "removed"
		return result
	}
	if _, err := os.Lstat(result.Path); errors.Is(err, os.ErrNotExist) {
		if err := clonereclaim.CheckRunRecovery(h); err != nil {
			return hold("missing_clone_unproven", err)
		}
		return hold("already_absent", nil)
	} else if err != nil {
		return fail("path_probe_failed", err)
	}
	r, err := clonereclaim.LoadRegistry(h)
	if err != nil {
		return hold("generation_evidence_invalid", err)
	}
	defer r.Close()
	result.Identity = r.Identity
	result.Receipt = r.Receipt()
	scripts, err := ownedDevEnvScripts(h.Root, h.Project, devEnvTeardownDirRel)
	if err != nil {
		return fail("hook_probe_failed", err)
	}
	plan, err := r.PlanCleanup(h, purpose, scripts, retry)
	if err != nil {
		if _, ok := errors.AsType[*clonereclaim.HoldError](err); !ok {
			return fail("cleanup_probe_failed", err)
		}
		return hold("cleanup_recovery_required", err)
	}
	if _, err := r.CheckTree(h, !basis.Expired); err != nil {
		if _, ok := errors.AsType[*clonereclaim.UnsafeTreeError](err); !ok {
			return fail("tree_probe_failed", err)
		}
		return hold("unsafe_tree", err)
	}
	preserve := func() error {
		if basis.Expired {
			return nil
		}
		artifacts, err := r.Artifacts(h)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		proof, err := clonereclaim.InspectPreservation(ctx, r.Identity.Path, filepath.Join(h.Root, "projects", h.Project, "src"), artifacts)
		if err != nil {
			return err
		}
		if proof.Reason != "" {
			return &clonereclaim.HoldError{Reason: proof.Reason, Receipt: r.Receipt()}
		}
		return nil
	}
	if err := preserve(); err != nil {
		if _, ok := errors.AsType[*clonereclaim.HoldError](err); !ok {
			return fail("preservation_probe_failed", err)
		}
		return hold("local_work_retained", err)
	}
	basis.Preservation = "independently_preserved"
	result.Reason = "independently_preserved"
	if basis.Expired {
		basis.Preservation = ""
		result.Reason = "retention_expired"
	}
	result.Outcome = "eligible"
	if !apply {
		return result
	}
	disposal, err := r.BeginDisposal(h, basis, plan)
	if err != nil {
		return fail("intent_publication_failed", err)
	}
	if err := devEnvTeardownOwned(context.Background(), h, md, purpose, retry, stdout, stderr); err != nil {
		return fail("teardown_failed", err)
	}
	// Re-read lifecycle and original content after hooks, including newly emitted
	// output. An early preservation hold prevents removal after successful teardown.
	fresh, err := cloneRunState(h)
	if err != nil {
		return fail("lifecycle_recheck_failed", err)
	}
	if fresh.Status != md.Status || fresh.Workspace != md.Workspace {
		return fail("lifecycle_changed", errors.New("run changed during cleanup"))
	}
	if purpose == "terminal" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		e, err := run.LatestTerminalTransition(ctx, h.Root, fresh, now)
		cancel()
		if err != nil {
			return fail("terminal_recheck_failed", err)
		}
		if e != basis.Terminal {
			return fail("terminal_changed", errors.New("terminal authority changed"))
		}
	}
	if err := preserve(); err != nil {
		result.Outcome = "held"
		return hold("post_teardown_work_retained", err)
	}
	plan, err = r.PlanCleanup(h, purpose, scripts, true)
	if err != nil {
		return fail("teardown_receipt_failed", err)
	}
	if err := r.AuthorizeRemoval(h, disposal, plan); err != nil {
		return fail("authorization_failed", err)
	}
	if err := r.FinishRemoval(h, disposal.ID, basis); err != nil {
		return fail("removal_failed", err)
	}
	result.Outcome = "removed"
	return result
}

func releasePerRunClone(h *runowner.Handle, md *run.Metadata) error {
	if h == nil || !h.Owns(h.Root, md.Project, md.ID) {
		return errors.New("workspace release requires held run ownership")
	}
	if _, err := os.Lstat(sandbox.Path(h.Root, md.Project, md.ID)); errors.Is(err, os.ErrNotExist) {
		return clonereclaim.CheckRunRecovery(h)
	} else if err != nil {
		return err
	}
	result := disposeClone(h, true, false, "terminal", time.Now().UTC(), os.Stdout, os.Stderr)
	if result.Outcome == "failed" {
		return fmt.Errorf("%s: %s; evidence: %s; retry `moe clone gc --apply %s/%s`", result.Reason, result.Detail, result.Receipt, md.Project, md.ID)
	}
	if result.Outcome == "held" && result.Reason != "already_absent" {
		if result.Reason == "local_work_retained" || result.Reason == "post_teardown_work_retained" || result.Reason == "terminal_history_unproven" {
			fmt.Fprintf(os.Stderr, "retained clone %s: %s; inspect with `moe clone gc %s/%s`\n", result.Path, result.Reason, md.Project, md.ID)
			return nil
		}
		return &clonereclaim.HoldError{Reason: result.Reason + ": " + result.Detail, Receipt: result.Receipt}
	}
	return nil
}
