package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/workspace"
)

// attachRunWorkspace resolves the working-tree clone for the run's
// code stage and pre-positions it on the run's branch. The path it
// returns is what the agent's session works in (cwd) and what
// downstream verbs (push, hooks, shell) refer to as the sandbox.
//
// Two shapes share one entry point so the rest of the engine doesn't
// need to know which it's working with:
//
//   - md.Workspace == "" — the original per-run sandbox at
//     .moe/clones/<project>/<run>/. Same behavior as the previous
//     sandbox.Ensure + sandbox.CheckoutBranch pair: fresh clone,
//     branch created off the source's HEAD.
//   - md.Workspace != "" — a persistent named workspace at
//     .moe/named/<project>/<name>/. Claimed for this run on first
//     attach (refused if claimed by another in-progress run), and
//     pre-positioned on the run's branch via workspace.Attach (which
//     refuses if the previous occupant left uncommitted changes and
//     anchors a freshly-created branch to the project's default
//     branch instead of whatever was previously checked out).
func attachRunWorkspace(root string, md *run.Metadata, branch string) (string, error) {
	defaultBranch, err := defaultBranchForProject(root, md.Project)
	if err != nil {
		return "", err
	}
	if md.Workspace == "" {
		clonePath, err := sandbox.Ensure(root, md.Project, md.ID)
		if err != nil {
			return "", err
		}
		if err := sandbox.CheckoutBranch(clonePath, branch); err != nil {
			return "", err
		}
		freshenRunBranch(clonePath, branch, defaultBranch)
		return clonePath, nil
	}

	runRef := md.Project + "/" + md.ID
	wp, err := workspace.Acquire(root, md.Project, md.Workspace, runRef)
	if err != nil {
		return "", err
	}
	if err := workspace.Attach(wp, branch, defaultBranch); err != nil {
		return "", err
	}
	freshenRunBranch(wp, branch, defaultBranch)
	return wp, nil
}

func freshenRunBranch(workTree, branch, defaultBranch string) {
	behind, err := sandbox.FreshenBranch(workTree, branch, defaultBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: freshen %s to origin/%s: %v\n", branch, defaultBranch, err)
		return
	}
	if behind > 0 {
		fmt.Printf("freshened %s → origin/%s (was %d commits behind)\n", branch, defaultBranch, behind)
	}
}

// resolveRunWorkspacePath returns the working-tree path the run is
// using, without acquiring claims, switching branches, or creating
// the workspace if it doesn't exist. Used by verbs (push, shell) that
// need to point at an already-attached workspace; refuses with a clear
// error when the workspace doesn't exist on disk so the operator
// notices that `sdlc build` was never run.
func resolveRunWorkspacePath(root string, md *run.Metadata) (string, error) {
	if md.Workspace == "" {
		if !sandbox.Exists(root, md.Project, md.ID) {
			return "", fmt.Errorf("no sandbox clone for %s/%s; run `moe %s build %s/%s` first",
				md.Project, md.ID, md.Workflow, md.Project, md.ID)
		}
		return sandbox.Ensure(root, md.Project, md.ID)
	}
	if !workspace.Exists(root, md.Project, md.Workspace) {
		return "", fmt.Errorf("workspace %q for project %q does not exist on disk; run `moe %s build %s/%s` first",
			md.Workspace, md.Project, md.Workflow, md.Project, md.ID)
	}
	return workspace.Path(root, md.Project, md.Workspace), nil
}

// releaseRunWorkspace drops the run's hold on its workspace at
// terminal status. For per-run sandboxes that means running the
// project's dev-env teardown hooks (so postgres dbs / tmpdirs / etc.
// die alongside the sandbox), then removing the clone — the branch
// and worktree go with it. For named workspaces that means parking
// the working tree on the project's default branch at the merged tip
// (so the next claim doesn't anchor itself to a stale view), deleting
// the local run branch, then releasing the claim. The directory
// itself stays for the next run to reuse; dev-env teardown happens at
// explicit `moe workspace refresh` or `moe workspace remove`, since
// the workspace's env is expected to outlive the run.
//
// Idempotent: a run that never reached `sdlc build` (no clone, no
// claim) is a no-op either way. Teardown is invoked best-effort —
// a non-zero exit is logged but doesn't block sandbox removal,
// because a half-removed sandbox is worse than a half-cleaned dev
// resource (the operator can re-run cleanup; they can't undo
// "sandbox stuck in a corrupt state"). The workspace reposition is
// similarly best-effort: the merge has already landed by the time
// this is called, so any failure is logged and the claim is still
// released. The operator can recover by hand with `moe workspace
// shell` and a manual `git checkout <default> && git pull`.
func releaseRunWorkspace(root string, md *run.Metadata) error {
	if md.Workspace == "" {
		if sandbox.Exists(root, md.Project, md.ID) {
			workTree := sandbox.Path(root, md.Project, md.ID)
			if err := devEnvRunTeardown(root, workTree, md, os.Stdout, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "dev-env: teardown for %s/%s: %v\n", md.Project, md.ID, err)
			}
		}
		return sandbox.Remove(root, md.Project, md.ID)
	}
	if workspace.Exists(root, md.Project, md.Workspace) {
		if defaultBranch, err := defaultBranchForProject(root, md.Project); err != nil {
			fmt.Fprintf(os.Stderr, "warning: reposition workspace: %v\n", err)
		} else {
			wp := workspace.Path(root, md.Project, md.Workspace)
			runBranch := branchPrefix + md.ID
			if err := workspace.ResetToDefault(wp, defaultBranch, runBranch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: reposition workspace: %v\n", err)
			}
		}
	}
	return workspace.Release(root, md.Project, md.Workspace)
}

// defaultBranchForProject reads the project's default branch from its
// project.json — used by attachRunWorkspace both as the base for
// newly-created run branches in a named workspace (so a workspace that
// previously hosted run A's branch doesn't silently anchor run B's
// branch to A's tip) and as the freshen target for commit-free run
// branches in either workspace shape. project.Load already validates
// the metadata shape; we just propagate the field.
func defaultBranchForProject(root, projectID string) (string, error) {
	pj, err := project.Load(root, projectID)
	if err != nil {
		return "", fmt.Errorf("workspace: load project %s: %w", projectID, err)
	}
	if pj.DefaultBranch == "" {
		return "", fmt.Errorf("workspace: project %s has no default_branch set in project.json", projectID)
	}
	return pj.DefaultBranch, nil
}

// addWorkspaceFlag declares --workspace on a verb that binds an sdlc
// run to a named workspace (new, reopen, the stage verbs). The value
// shape and the claim note are shared; extra is the clause that differs
// per verb ("" for none).
func addWorkspaceFlag(fs *flag.FlagSet, extra string) *string {
	usage := "bind the run to the named workspace `<project>/<name>` at .moe/named/<project>/<name>/ and use it as the run's working tree; the project must be the run's, and the claim is taken at stage attach"
	if extra != "" {
		usage += ". " + extra
	}
	return fs.String("workspace", "", usage)
}

// checkWorkspaceBinding validates a --workspace value against the run's
// project and refuses a workspace another run holds. value is the
// `<project>/<name>` the operator typed; the returned name is bare —
// run.json stores the name alone, since the project is the run's own.
// self is the "<project>/<run>" allowed to already hold the claim (a
// stage verb passes the run it binds; new and reopen pass "").
//
// Returns the bare name and 0, or "" and an exit code with stderr
// already written: 2 for a malformed or mismatched value, 1 for a claim
// held elsewhere. A stale claim (a crashed or force-closed holder)
// clears with `moe workspace release`; it isn't papered over here.
func checkWorkspaceBinding(root, runProject, value, self string, stderr io.Writer) (string, int) {
	project, name, ok := strings.Cut(value, "/")
	if !ok {
		moePrintf(stderr, "--workspace: want <project>/<name> — try %s/%s\n", runProject, value)
		return "", 2
	}
	if project != runProject {
		moePrintf(stderr, "--workspace: %s names project %q, but the run is in %q\n", value, project, runProject)
		return "", 2
	}
	if err := workspace.ValidateName(name); err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", 2
	}
	holder, err := workspace.ReadClaim(root, runProject, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", 1
	}
	if holder != nil && holder.Run != self {
		moePrintf(stderr,
			"workspace %q for project %q is claimed by run %s; close that run first or pick a different workspace\n",
			name, runProject, holder.Run)
		return "", 1
	}
	return name, 0
}

// runTreeWork reports what the run has in its current working tree that
// a rebind would strand: commits on the run branch ahead of
// origin/<default>, and modified tracked files. held is false when
// there is no tree to drop — a per-run clone that was never made, or a
// named workspace this run doesn't hold — and then there is no work
// either. Untracked files don't count: in a per-run clone they're
// scratch that goes with it, and a named workspace keeps its directory.
func runTreeWork(root string, md *run.Metadata) (path string, commits, modified int, held bool, err error) {
	if md.Workspace == "" {
		if !sandbox.Exists(root, md.Project, md.ID) {
			return "", 0, 0, false, nil
		}
		path = sandbox.Path(root, md.Project, md.ID)
	} else {
		claim, err := workspace.ReadClaim(root, md.Project, md.Workspace)
		if err != nil {
			return "", 0, 0, false, err
		}
		if claim == nil || claim.Run != md.Project+"/"+md.ID {
			return "", 0, 0, false, nil
		}
		path = workspace.Path(root, md.Project, md.Workspace)
	}
	branch := "refs/heads/" + branchPrefix + md.ID
	if git.HasRef(path, branch) {
		defaultBranch, err := defaultBranchForProject(root, md.Project)
		if err != nil {
			return path, 0, 0, true, err
		}
		if commits, err = git.AheadOf(path, "origin/"+defaultBranch, branch); err != nil {
			return path, 0, 0, true, err
		}
	}
	entries, err := git.Status(path)
	if err != nil {
		return path, commits, 0, true, err
	}
	for _, e := range entries {
		if !e.Untracked() {
			modified++
		}
	}
	return path, commits, modified, true, nil
}
