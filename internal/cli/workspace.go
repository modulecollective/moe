package cli

import (
	"fmt"
	"os"

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
	if md.Workspace == "" {
		clonePath, err := sandbox.Ensure(root, md.Project, md.ID)
		if err != nil {
			return "", err
		}
		if err := sandbox.CheckoutBranch(clonePath, branch); err != nil {
			return "", err
		}
		return clonePath, nil
	}

	runRef := md.Project + "/" + md.ID
	wp, err := workspace.Acquire(root, md.Project, md.Workspace, runRef)
	if err != nil {
		return "", err
	}
	baseBranch, err := defaultBranchForProject(root, md.Project)
	if err != nil {
		return "", err
	}
	if err := workspace.Attach(wp, branch, baseBranch); err != nil {
		return "", err
	}
	return wp, nil
}

// resolveRunWorkspacePath returns the working-tree path the run is
// using, without acquiring claims, switching branches, or creating
// the workspace if it doesn't exist. Used by verbs (push, shell) that
// need to point at an already-attached workspace; refuses with a clear
// error when the workspace doesn't exist on disk so the operator
// notices that `sdlc code` was never run.
func resolveRunWorkspacePath(root string, md *run.Metadata) (string, error) {
	if md.Workspace == "" {
		if !sandbox.Exists(root, md.Project, md.ID) {
			return "", fmt.Errorf("no sandbox clone for %s/%s; run `moe %s code %s/%s` first",
				md.Project, md.ID, md.Workflow, md.Project, md.ID)
		}
		return sandbox.Ensure(root, md.Project, md.ID)
	}
	if !workspace.Exists(root, md.Project, md.Workspace) {
		return "", fmt.Errorf("workspace %q for project %q does not exist on disk; run `moe %s code %s/%s` first",
			md.Workspace, md.Project, md.Workflow, md.Project, md.ID)
	}
	return workspace.Path(root, md.Project, md.Workspace), nil
}

// releaseRunWorkspace drops the run's hold on its workspace at
// terminal status. For per-run sandboxes that means running the
// project's dev-env teardown hooks (so postgres dbs / tmpdirs / etc.
// die alongside the sandbox), then removing the clone — the branch
// and worktree go with it. For named workspaces that means releasing
// the claim and leaving the directory in place for the next run to
// reuse; teardown happens at explicit `moe workspace refresh` or
// `moe workspace remove`, since the workspace's env is expected to
// outlive the run.
//
// Idempotent: a run that never reached `sdlc code` (no clone, no
// claim) is a no-op either way. Teardown is invoked best-effort —
// a non-zero exit is logged but doesn't block sandbox removal,
// because a half-removed sandbox is worse than a half-cleaned dev
// resource (the operator can re-run cleanup; they can't undo
// "sandbox stuck in a corrupt state").
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
	return workspace.Release(root, md.Project, md.Workspace)
}

// defaultBranchForProject reads the project's default branch from its
// project.json — used by attachRunWorkspace as the base for newly-
// created run branches in a named workspace, so a workspace that
// previously hosted run A's branch doesn't silently anchor run B's
// branch to A's tip. project.Load already validates the metadata
// shape; we just propagate the field.
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
