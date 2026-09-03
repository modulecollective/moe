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

	if err := doSync(root, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// doSync runs the sync pipeline: pull → pointer bumps → reconcile
// pushed runs → push. It takes the repo lock itself, in windows, and
// must be called with no lock held (the repolock is not reentrant).
//
// The windows are deliberately narrow. Only the first leg rewrites the
// worktree, so only it is serialized; the reconcile walk takes its own
// lock per transition (see reconcilePushedRuns), and the final push is
// outside every window because a push writes no worktree state and no
// local ref — serve's pusher already races it today.
//
// What that gives up: two `moe sync` invocations racing can now both
// commit an identical bump and the loser gets a non-fast-forward push
// error. It's loud, the fix is to run `moe sync` again, and it costs
// an operator typing the verb twice — cheaper than half a second of
// `gh` per pushed run with the whole bureaucracy locked behind it.
func doSync(root string, stdout, stderr io.Writer) error {
	if err := repolock.With(root, repolock.Options{
		Purpose: "sync",
		Budget:  repolock.CronBudget,
		// The pull and BumpProjectPointers' per-submodule fetches sit
		// on the network inside this window, so it needs the heartbeat
		// to stay distinguishable from a crashed holder.
		Heartbeat: true,
	}, func() error {
		return syncWorktree(root, stdout, stderr)
	}); err != nil {
		return err
	}

	// Reconcile any pushed runs: if GitHub says the PR merged or
	// closed, flip the run's status and clean up the branch + sandbox
	// so the end state matches the direct-merge path. The walk holds
	// no lock while it talks to GitHub — see reconcileOnePushedRun.
	if _, err := reconcilePushedRuns(root, "" /*all projects*/, stdout, stderr); err != nil {
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

// syncWorktree is doSync's first leg — everything that rewrites the
// bureaucracy worktree, and the only part of sync that needs the repo
// lock held around it. Runs inside doSync's window.
func syncWorktree(root string, stdout, stderr io.Writer) error {
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
		// -c advice.skippedCherryPicks=false: when this pull rebases a
		// local "sync: bump project pointers" commit over an identical
		// bump that another machine already pushed, git deduplicates the
		// patch and prints a "skipped previously applied commit" warning
		// plus two hints to re-run with --reapply-cherry-picks. The dedup
		// is correct and benign (the gitlink converges either way), but
		// the hints are actively misleading here — reapplying would force
		// an empty/conflicting duplicate bump back in. Suppress the advice;
		// the bare warning stays as honest signal.
		if err := git.Stream(root, stdout, stderr, "-c", "advice.skippedCherryPicks=false", "pull", "--rebase", "--autostash", "--no-recurse-submodules"); err != nil {
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
	return sync.BumpProjectPointers(root, stdout)
}

// reconcilePushedRuns walks every run in StatusPushed, asks GitHub
// what state its PR is in, and — when the PR has been merged or
// closed — flips the run's status, tears down the branch and sandbox,
// and records a closing trailer. Open PRs are a silent no-op; sync
// prints exactly one line per transition and nothing for runs that
// didn't move.
//
// projectID scopes the walk: "" is every project (sync's shape), a
// project id is that project alone (the pulse's tail reconcile, which
// has no business touching a neighbour's runs). The returned count is
// how many runs actually transitioned — the pulse uses it to decide
// whether the journal is worth pushing, since the common case moves
// nothing and a no-op push is a network leg for free.
//
// Must be called with no repo lock held: the walk takes the lock
// itself, one short window per transition, and holds none of it while
// it talks to GitHub. A sweep over a project with nothing pushed
// touches neither the lock nor the network.
func reconcilePushedRuns(root, projectID string, stdout, stderr io.Writer) (int, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return 0, fmt.Errorf("moe sync: scan runs: %w", err)
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
	moved := 0
	for _, md := range mds {
		if md.Status != run.StatusPushed {
			continue
		}
		if projectID != "" && md.Project != projectID {
			continue
		}
		ok, err := reconcileOnePushedRun(root, md, stdout, stderr)
		if err != nil {
			return moved, err
		}
		if ok {
			moved++
		}
	}
	return moved, nil
}

// reconcileOnePushedRun asks GitHub about one pushed run's PR and
// applies the transition it implies. The bool is whether the run
// actually moved — a still-open PR, a skipped run, or a deferred
// harvest all report false.
//
// This is phase one, and it holds no lock: the trailer walk, the `gh
// pr view`, the merge-commit chore fetch and the remote branch delete
// all happen here. Only the disk mutation of an actual transition
// takes the lock, in finalizePushedRun. Everything that ends in
// warn-and-skip returns before the lock is ever reached.
func reconcileOnePushedRun(root string, md *run.Metadata, stdout, stderr io.Writer) (bool, error) {
	prURL := push.TrailerValue(root, md.Project, md.ID, "MoE-PR")
	if prURL == "" {
		// No MoE-PR trailer on record despite StatusPushed. Flag and
		// skip rather than guess — the operator can untangle by hand.
		moePrintf(stderr, "moe sync: %s/%s is pushed but has no MoE-PR trailer; skipping\n", md.Project, md.ID)
		return false, nil
	}
	state, err := sync.PRStateOf(prURL)
	if err != nil {
		moePrintf(stderr, "moe sync: %s/%s: %v; skipping\n", md.Project, md.ID, err)
		return false, nil
	}

	var status, line string
	var extra trailers.Block
	switch strings.ToUpper(state.State) {
	case "OPEN":
		return false, nil
	case "MERGED":
		mergeSHA := state.MergeCommit.OID
		if mergeSHA == "" {
			moePrintf(stderr, "moe sync: %s/%s merged but gh returned no mergeCommit; skipping\n", md.Project, md.ID)
			return false, nil
		}
		status = run.StatusMerged
		extra = trailers.Block{
			Merged:       mergeSHA,
			ChoreTouched: touchedChoresForCommit(root, md.Project, mergeSHA),
		}
		line = fmt.Sprintf("%s: pushed -> merged (%s)\n", md.ID, git.ShortSHA(mergeSHA))
	case "CLOSED":
		status = run.StatusClosed
		extra = trailers.Block{Closed: prURL}
		line = fmt.Sprintf("%s: pushed -> closed\n", md.ID)
	default:
		moePrintf(stderr, "moe sync: %s/%s has unexpected PR state %q; skipping\n", md.Project, md.ID, state.State)
		return false, nil
	}

	// Delete the branch before the lock rather than between harvest and
	// commit. MERGED/CLOSED is already GitHub's point of no return for
	// a pushed run, and sync.DeleteRemoteBranch reads a 422 "Reference
	// does not exist" as success — so a delete whose transition then
	// fails to commit costs nothing: the run stays `pushed`, the next
	// reconcile still sees MERGED, and the second delete is a no-op.
	// The alternative keeps half a second of network inside the lock on
	// every transition.
	if err := deleteRemoteBranchForRun(root, md); err != nil {
		moePrintf(stderr, "warning: %s/%s: %v\n", md.Project, md.ID, err)
	}

	ok, err := finalizePushedRun(root, md.Project, md.ID, status, extra, stderr)
	if err != nil {
		return false, err
	}
	if ok {
		moePrintf(stdout, "%s", line)
	}
	return ok, nil
}

// finalizePushedRun is phase two: the one lock window a transition
// needs. Inside it, harvest follow-ups, flip the status, drop the
// sandbox clone, and commit run.json with the closing trailer. The
// cleanup mirrors the direct-merge path so the end state is
// indistinguishable regardless of how the run reached a terminal
// status. Sandbox teardown failures are warned but non-fatal — the
// reconciliation has otherwise succeeded and a stray clone is a
// cleanup nuisance, not a correctness bug.
//
// It re-loads run.json and re-judges under the lock, and that is the
// load-bearing line. Phase one's read of the status happened outside
// any window, so this write has to re-read: a concurrent `moe sync`, a
// neighbouring pulse, or an operator `push` may have finalised the run
// in between. When it has, this returns (false, nil) silently — both
// racers query GitHub, both issue the idempotent branch delete, the
// first commits, the second prints nothing. That is what keeps
// "exactly one stdout line per transition" true.
//
// Harvest is best-effort here: a follow-up failure leaves the run in
// `pushed`, prints a one-line warning, and returns (false, nil) so
// reconcile can continue with other runs and the next `moe sync`
// retries. Returns (true, nil) when the transition committed.
func finalizePushedRun(root, projectID, runID, status string, extra trailers.Block, stderr io.Writer) (bool, error) {
	moved := false
	err := repolock.With(root, repolock.Options{
		Purpose: "reconcile",
		Run:     projectID + "/" + runID,
		Budget:  repolock.CronBudget,
		// releaseRunWorkspace runs the project's dev-env teardown
		// hooks and rm -rf's a whole sandbox clone; neither is bounded
		// by anything moe controls, so the window keeps its heartbeat
		// rather than risk reading as a crashed holder.
		Heartbeat: true,
	}, func() error {
		md, err := run.Load(root, projectID, runID)
		if err != nil {
			return fmt.Errorf("moe sync: reload %s/%s: %w", projectID, runID, err)
		}
		if md.Status != run.StatusPushed {
			// Someone else finalised it between phase one's read and
			// this window. Their transition printed the line.
			return nil
		}
		priorStatus := md.Status
		paths, err := enterTerminal(root, md, status, true)
		if err != nil {
			moePrintf(stderr, "moe sync: %s/%s harvest failed: %v; retry next sync\n", projectID, runID, err)
			return nil
		}
		if err := releaseRunWorkspace(root, md); err != nil {
			moePrintf(stderr, "warning: %s/%s: release workspace: %v\n", projectID, runID, err)
		}
		extra.Run = runID
		extra.Project = projectID
		extra.Document = "push"
		msg := fmt.Sprintf("sync: %s/%s %s\n\n", projectID, runID, strings.ToLower(status)) +
			extra.String()
		if err := commitTerminal(root, md, priorStatus, msg, paths); err != nil {
			return fmt.Errorf("moe sync: commit %s for %s/%s: %w", strings.ToLower(status), projectID, runID, err)
		}
		moved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
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
