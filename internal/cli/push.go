package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

var pushCmd = &Command{
	Name:    "push",
	Summary: "ship the run's code branch: fast-forward merge to default, or open a PR with --pr",
	Run:     runPush,
}

const branchPrefix = "moe/"

// runPush ships the sandbox branch. The default path fast-forwards the
// target repo's default branch to include moe/<run>, deletes the remote
// branch, drops the sandbox clone, and marks the run `merged`. The
// `--pr` path is today's behavior: push the branch, open (or re-use) a
// PR, mark the run `pushed`, keep the sandbox. A pushed run later
// reconciles to merged/closed via `moe sync`.
//
// Idempotent on terminal runs: rerunning after a merged/closed run is
// a no-op that prints the terminal state and exits 0.
func runPush(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prFlag := fs.Bool("pr", false, "open a PR instead of fast-forward merging to the default branch")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe <wf> push [--pr] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Default: push moe/<run>, fast-forward-merge it into the target repo's")
		moePrintln(stderr, "default branch, delete the remote branch, and remove the sandbox clone.")
		moePrintln(stderr, "--pr: push moe/<run> and open (or re-use) a PR; leave the sandbox in place.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, runID := fs.Arg(0), fs.Arg(1)

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

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Terminal statuses short-circuit before touching the sandbox — the
	// clone is expected to be gone for `merged`, and for `closed` the
	// run is archived. Mirror today's "existing PR" idempotency.
	switch md.Status {
	case run.StatusMerged:
		if sha := push.MergedSHA(root, md.ID); sha != "" {
			moePrintf(stdout, "already merged at %s\n", git.ShortSHA(sha))
		} else {
			moePrintln(stdout, "already merged")
		}
		return 0
	case run.StatusClosed:
		moePrintln(stdout, "already closed")
		return 0
	}

	pj, err := project.Load(root, md.Project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if err := checkCodeContent(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	clonePath, err := sandboxClonePath(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	branch := branchPrefix + md.ID
	if err := push.CheckCleanWorkTree(clonePath); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := push.CheckBranchHasCommits(clonePath, branch, pj.DefaultBranch); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := push.EnsureOrigin(clonePath, pj.Remote); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	hooks := hookEnv{
		Project:      md.Project,
		Run:          md.ID,
		Document:     "push",
		Workflow:     md.Workflow,
		Sandbox:      clonePath,
		Bureaucracy:  root,
		TargetBranch: pj.DefaultBranch,
	}
	if err := runHooks(root, hookEventPrePush, hooks, stdout, stderr); err != nil {
		var conflict *push.RebaseConflictError
		if errors.As(err, &conflict) {
			moePrintf(stderr, "%v\n", conflict)
			return openCodeSessionForRebaseConflict(md, conflict, stdout, stderr)
		}
		var fail *hookFailure
		if errors.As(err, &fail) {
			moePrintf(stderr, "%v\n", fail)
			return openCodeSessionForHookFailure(md, fail, stdout, stderr)
		}
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// When origin already has moe/<run> (a prior `--pr` cycle, or a
	// re-run after an agent-side rebase resolved a conflict), the
	// upcoming push may not be a fast-forward — the local branch's
	// history could differ from origin's. Force-with-lease is harmless
	// when the two match and refuses to overwrite a concurrent update
	// when they don't. Skip when origin has no copy of the branch:
	// the first push is a plain push with -u to establish tracking.
	force := push.OriginHasBranch(clonePath, branch)

	if err := push.PushBranch(clonePath, branch, pj.Remote, force, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if *prFlag {
		return openPRPath(root, md, pj, branch, stdout, stderr)
	}
	return mergePath(root, md, pj, clonePath, branch, stdout, stderr)
}

// init registers the rebase-onto-default check as the first pre-push
// built-in. Built-ins run before project scripts (in pre-push.d/) so
// the scripts see the tree the rebase produced — the one about to be
// pushed. Vetting the pre-rebase tree is how a stale call site against
// a sibling branch's API change slips past local hooks and breaks CI.
func init() {
	registerBuiltinHook(hookEventPrePush, builtinHook{
		Name: "rebase-onto-default",
		Run: func(env hookEnv, stdout, stderr io.Writer) error {
			branch := branchPrefix + env.Run
			return push.EnsureRebasedOntoDefault(env.Sandbox, branch, env.TargetBranch, stdout, stderr)
		},
	})
}

// openCodeSessionForRebaseConflict is the chain-back: spawn a fresh
// interactive code session against the same run with a kickoff prompt
// that names the conflicting paths and the target branch, then propagate
// that session's exit code so a clean resolve-and-commit lets the
// workflow's chain prompt offer push next — same shape `moe <wf> code`
// already produces.
//
// Overridable in tests; the default invokes runStageSession directly
// with docID="code", same as `moe <wf> code` would.
var openCodeSessionForRebaseConflict = func(md *run.Metadata, conflict *push.RebaseConflictError, stdout, stderr io.Writer) int {
	moePrintln(stderr, "       opening a fresh code session — resolve the conflicts and commit; the chain prompt will offer push next")
	kickoff := buildRebaseConflictKickoff(md.Workflow, conflict)
	return runStageSession(md.Project, md.ID, "code", stageSessionOpts{
		NeedsSandbox:  true,
		InitialPrompt: kickoff,
	}, stdout, stderr)
}

// buildRebaseConflictKickoff is the agent-facing kickoff prompt for a
// chain-back code session. Names the target branch, lists the
// conflicting paths (when git left any), and tells the agent what
// "done" looks like — resolve, commit, exit; the post-turn chain
// prompt will offer push.
func buildRebaseConflictKickoff(workflow string, c *push.RebaseConflictError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`moe %s push` just tried to rebase %s onto origin/%s and hit conflicts. ",
		workflow, c.Branch, c.DefaultBranch)
	b.WriteString("The rebase has been aborted, so the working tree is clean and the branch is back at its pre-rebase tip — you are starting from the conflict state, not mid-rebase.\n\n")
	if len(c.Conflicts) > 0 {
		b.WriteString("Files git flagged as conflicting on the abandoned rebase:\n")
		for _, p := range c.Conflicts {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Re-run the rebase yourself (`git rebase origin/%s` from the sandbox), resolve the conflicts, ",
		c.DefaultBranch)
	fmt.Fprintf(&b, "verify the result still does what the design intended, and commit. Then exit the session — the post-turn chain prompt will offer `moe %s push` next.\n", workflow)
	return b.String()
}

// openPRPath is the --pr behavior: open (or re-use) a PR for the
// already-pushed branch and record the first push's state. The
// sandbox is intentionally left in place — iteration via
// `moe <wf> code` stays a one-liner until the PR merges.
func openPRPath(root string, md *run.Metadata, pj *project.Metadata, branch string, stdout, stderr io.Writer) int {
	ghRepo, err := push.GHRepoSpec(pj.Remote)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	url, existing, err := push.FindOpenPR(ghRepo, branch)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if existing {
		moePrintf(stdout, "existing PR: %s\n", url)
	} else {
		bodyPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
		url, err = push.CreatePR(ghRepo, branch, pj.DefaultBranch, md.Title, bodyPath, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		moePrintf(stdout, "opened PR: %s\n", url)
	}

	// Only the first push flips status and records the MoE-PR trailer.
	// Re-runs just pushed branch updates to an already-recorded PR.
	if md.Status != run.StatusPushed {
		md.Status = run.StatusPushed
		if err := run.Save(root, md); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
		msg := fmt.Sprintf("push: %s %s\n\n", md.Project, md.ID) +
			trailers.Block{
				Run:      md.ID,
				Project:  md.Project,
				Workflow: md.Workflow,
				Document: "push",
				PR:       url,
			}.String()
		err := withRepoLock(root, repolock.Options{
			Purpose: "push-pr",
			Run:     md.Project + "/" + md.ID,
		}, func() error {
			return run.StageAndCommit(root, msg, runJSON)
		})
		if err != nil {
			moePrintf(stderr, "commit push record: %v\n", err)
			return 1
		}
	}
	return 0
}

// mergePath is the default path: fast-forward the target repo's
// default branch to include moe/<run>, delete the remote branch, drop
// the sandbox, and mark the run merged. Sandbox and branch deletion
// happen after the merge-push succeeds so a failure mid-flight leaves
// both intact for retry.
func mergePath(root string, md *run.Metadata, pj *project.Metadata, clonePath, branch string, stdout, stderr io.Writer) int {
	tipSHA, err := git.RevParse(clonePath, "refs/heads/"+branch)
	if err != nil {
		moePrintf(stderr, "push: resolve %s: %v\n", branch, err)
		return 1
	}

	// Harvest follow-ups and flip run.json to merged before the
	// ff-push: harvest (and any per-idea slug failures) must be
	// reversible, and FastForwardToDefault is the point of no return
	// for the merged transition. enterTerminal does the harvest under
	// lock so each createIdea sees a held bureaucracy lock. skipEdit=
	// false: push is the operator's termination decision, so the
	// editor pops on followups.md before harvest just like close.
	priorStatus := md.Status
	var paths []string
	err = withRepoLock(root, repolock.Options{
		Purpose: "push-harvest",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		var ferr error
		paths, ferr = enterTerminal(root, md, run.StatusMerged, false)
		return ferr
	})
	if err != nil {
		moePrintf(stderr, "push: harvest: %v\n", err)
		return 1
	}

	moePrintf(stdout, "fast-forwarding %s to %s on %s...\n", pj.DefaultBranch, branch, pj.Remote)
	if err := push.FastForwardToDefault(clonePath, branch, pj.DefaultBranch, stdout, stderr); err != nil {
		// Roll back the status flip enterTerminal just wrote: the
		// remote merge didn't happen, so the run shouldn't be
		// "merged" on disk. Harvest commits and followups.md
		// rewrites stay; harvest is idempotent on retry.
		if rerr := revertTerminal(root, md, priorStatus); rerr != nil {
			moePrintf(stderr, "warning: revert run.json after ff-push failure: %v\n", rerr)
		}
		moePrintf(stderr, "%v\n", err)
		moePrintf(stderr, "       origin/%s may have advanced between the pre-push rebase and ff-push — re-run `moe %s push %s %s`\n",
			pj.DefaultBranch, md.Workflow, md.Project, md.ID)
		return 1
	}

	if err := push.DeleteRemoteBranch(clonePath, branch, stdout, stderr); err != nil {
		// Merge already landed; warn but don't fail the command.
		moePrintf(stderr, "warning: %v\n", err)
	}

	msg := fmt.Sprintf("push: %s %s merged\n\n", md.Project, md.ID) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
			Document: "push",
			Merged:   tipSHA,
		}.String()
	err = withRepoLock(root, repolock.Options{
		Purpose: "push-merge",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		if err := releaseRunWorkspace(root, md); err != nil {
			moePrintf(stderr, "warning: release workspace: %v\n", err)
		}
		return run.StageAndCommit(root, msg, paths...)
	})
	if err != nil {
		moePrintf(stderr, "commit merge record: %v\n", err)
		return 1
	}
	moePrintf(stdout, "merged %s %s at %s\n", md.Project, md.ID, git.ShortSHA(tipSHA))
	return 0
}

func checkCodeContent(root string, md *run.Metadata) error {
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("push: code document not written yet; run `moe %s code %s %s` first", md.Workflow, md.Project, md.ID)
		}
		return fmt.Errorf("push: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("push: code document is empty; run `moe %s code %s %s` and produce a PR body first", md.Workflow, md.Project, md.ID)
	}
	return nil
}

func sandboxClonePath(root string, md *run.Metadata) (string, error) {
	wp, err := resolveRunWorkspacePath(root, md)
	if err != nil {
		return "", fmt.Errorf("push: %w", err)
	}
	return wp, nil
}
