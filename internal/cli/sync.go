package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

func init() {
	Register(&Command{
		Name:    "sync",
		Summary: "sync the bureaucracy repo with origin (git pull --rebase, bump project pointers, then push)",
		Run:     runSync,
	})
}

func runSync(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe sync")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// sync is one logical bureaucracy mutation (pull → pointer bumps →
	// reconcile pushed runs → push). Hold the repo lock for the whole
	// sequence so two syncs don't clobber each other mid-flight.
	// Heartbeat is on because sync can sit on the network for a while.
	err = repolock.With(root, repolock.Options{
		Purpose:   "sync",
		Budget:    repolock.CronBudget,
		Heartbeat: true,
	}, func() error {
		return doSync(root, stdout, stderr)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// doSync runs the sync pipeline under an already-held repo lock.
func doSync(root string, stdout, stderr io.Writer) error {
	// If a previous sync left a rebase mid-flight, refuse with a
	// recovery block instead of charging into another pull. Resolution
	// is plain git: `git status` → fix → `git rebase --continue` (or
	// `--abort`). Once the rebase is gone, the next sync proceeds.
	if sync.RebaseInProgress(root) {
		return sync.RebaseRecoveryError(root)
	}

	// --rebase --autostash so the two-machines case (operator commits
	// turns on machine A and B between syncs) reconciles to a linear
	// sequence. Rebase preserves MoE-* trailers on replayed commits,
	// which is what every trailer-aware reader (dash, sync's own
	// reconcile walk) relies on. Skipped on a brand-new branch
	// with no upstream — nothing to pull from. On rebase conflict, git
	// leaves the worktree mid-rebase; we surface a recovery block
	// rather than git's raw stderr.
	//
	// --no-recurse-submodules is explicit: rebase preflights
	// submodule_touches_in_range(upstream..HEAD) and aborts if any
	// local commit ahead of upstream changes a gitlink. Bump commits
	// from sync itself routinely move gitlinks, so the preflight would
	// fire on every sync against a worktree that is ahead. The recursion
	// work the flag would do (fetch submodules in parallel, then
	// `git submodule update` to match the new gitlinks) is already
	// owned by BumpProjectPointers below, which fetches each submodule
	// and reconciles its worktree against the recorded gitlink. Passing
	// --no- explicitly so a user-side pull.recurseSubmodules / submodule.recurse
	// config can't re-enable it.
	if sync.HasUpstream(root) {
		if err := git.Stream(root, stdout, stderr, "pull", "--rebase", "--autostash", "--no-recurse-submodules"); err != nil {
			if sync.RebaseInProgress(root) {
				return sync.RebaseRecoveryError(root)
			}
			return fmt.Errorf("git pull: %w", err)
		}
	}

	// Walk each project submodule, fast-forward its tracking branch from
	// origin, and bump the gitlink in bureaucracy if the submodule moved.
	// Done after the pull so we're working from the latest bureaucracy state,
	// and before the push so the bump goes out in the same round trip.
	if err := sync.BumpProjectPointers(root, stdout, stderr); err != nil {
		return err
	}

	// Reconcile any pushed runs: if GitHub says the PR merged or
	// closed, flip the run's status and clean up the branch + sandbox
	// so the end state matches the direct-merge path.
	if err := reconcilePushedRuns(root, stdout, stderr); err != nil {
		return err
	}

	// If the current branch has no upstream configured, push with -u so the
	// first push sets one. After that, plain `git push` is correct and keeps
	// whatever upstream the operator chose.
	pushArgs := []string{"push", "--recurse-submodules=on-demand"}
	if !sync.HasUpstream(root) {
		pushArgs = []string{"push", "--recurse-submodules=on-demand", "-u", "origin", "HEAD"}
	}
	if err := git.Stream(root, stdout, stderr, pushArgs...); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

// reconcilePushedRuns walks every run in StatusPushed, asks GitHub
// what state its PR is in, and — when the PR has been merged or
// closed — flips the run's status, tears down the branch and sandbox,
// and records a closing trailer. Open PRs are a silent no-op; sync
// prints exactly one line per transition and nothing for runs that
// didn't move.
func reconcilePushedRuns(root string, stdout, stderr io.Writer) error {
	mds, err := run.Scan(root)
	if err != nil {
		return fmt.Errorf("moe sync: scan runs: %w", err)
	}
	// Deterministic order so transition lines come out the same way
	// across invocations — helps when the operator is scanning output
	// and makes test assertions stable.
	sort.Slice(mds, func(i, j int) bool {
		if mds[i].Project != mds[j].Project {
			return mds[i].Project < mds[j].Project
		}
		return mds[i].ID < mds[j].ID
	})
	for _, md := range mds {
		if md.Status != run.StatusPushed {
			continue
		}
		if err := reconcileOnePushedRun(root, md, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func reconcileOnePushedRun(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	prURL := push.TrailerValue(root, md.ID, "MoE-PR")
	if prURL == "" {
		// No MoE-PR trailer on record despite StatusPushed. Flag and
		// skip rather than guess — the operator can untangle by hand.
		moePrintf(stderr, "moe sync: %s/%s is pushed but has no MoE-PR trailer; skipping\n", md.Project, md.ID)
		return nil
	}
	state, err := sync.PRStateOf(prURL)
	if err != nil {
		moePrintf(stderr, "moe sync: %s/%s: %v; skipping\n", md.Project, md.ID, err)
		return nil
	}
	switch strings.ToUpper(state.State) {
	case "OPEN":
		return nil
	case "MERGED":
		mergeSHA := state.MergeCommit.OID
		if mergeSHA == "" {
			moePrintf(stderr, "moe sync: %s/%s merged but gh returned no mergeCommit; skipping\n", md.Project, md.ID)
			return nil
		}
		ok, err := finalizePushedRun(root, md, run.StatusMerged, trailers.Block{
			Merged:       mergeSHA,
			ChoreTouched: touchedChoresForCommit(root, md.Project, mergeSHA),
		}, stderr)
		if err != nil {
			return err
		}
		if ok {
			moePrintf(stdout, "%s: pushed -> merged (%s)\n", md.ID, git.ShortSHA(mergeSHA))
		}
	case "CLOSED":
		ok, err := finalizePushedRun(root, md, run.StatusClosed, trailers.Block{Closed: prURL}, stderr)
		if err != nil {
			return err
		}
		if ok {
			moePrintf(stdout, "%s: pushed -> closed\n", md.ID)
		}
	default:
		moePrintf(stderr, "moe sync: %s/%s has unexpected PR state %q; skipping\n", md.Project, md.ID, state.State)
	}
	return nil
}

// finalizePushedRun harvests follow-ups, flips md.Status, deletes the
// remote branch and the sandbox clone, and commits run.json with the
// closing trailer. The cleanup mirrors the direct-merge path so the
// end state is indistinguishable regardless of how the run reached a
// terminal status. Branch/sandbox deletion failures are warned but
// non-fatal — the reconciliation has otherwise succeeded and a stray
// branch or clone is a cleanup nuisance, not a correctness bug.
//
// Harvest is best-effort here: a follow-up failure leaves the run in
// `pushed`, prints a one-line warning, and returns (false, nil) so
// reconcile can continue with other runs and the next `moe sync`
// retries. Returns (true, nil) when the transition committed.
func finalizePushedRun(root string, md *run.Metadata, status string, extra trailers.Block, stderr io.Writer) (bool, error) {
	paths, err := enterTerminal(root, md, status, true)
	if err != nil {
		moePrintf(stderr, "moe sync: %s/%s harvest failed: %v; retry next sync\n", md.Project, md.ID, err)
		return false, nil
	}
	if err := deleteRemoteBranchForRun(root, md); err != nil {
		moePrintf(stderr, "warning: %s/%s: %v\n", md.Project, md.ID, err)
	}
	if err := releaseRunWorkspace(root, md); err != nil {
		moePrintf(stderr, "warning: %s/%s: release workspace: %v\n", md.Project, md.ID, err)
	}
	extra.Run = md.ID
	extra.Project = md.Project
	extra.Document = "push"
	msg := fmt.Sprintf("sync: %s/%s %s\n\n", md.Project, md.ID, strings.ToLower(status)) +
		extra.String()
	if err := run.StageAndCommit(root, msg, paths...); err != nil {
		return false, fmt.Errorf("moe sync: commit %s for %s/%s: %w", strings.ToLower(status), md.Project, md.ID, err)
	}
	return true, nil
}

// deleteRemoteBranchForRun loads the project's remote, derives the
// gh-flavored owner/repo, and asks GitHub to drop moe/<run> from it.
// A thin wrapper around sync.DeleteRemoteBranch that supplies the run
// → repo lookup so the domain function can stay signed in pure terms.
func deleteRemoteBranchForRun(root string, md *run.Metadata) error {
	pj, err := project.Load(root, md.Project)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	repo, err := push.GHRepoSpec(pj.Remote)
	if err != nil {
		return err
	}
	return sync.DeleteRemoteBranch(repo, branchPrefix+md.ID)
}
