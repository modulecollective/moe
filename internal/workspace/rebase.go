package workspace

import (
	"fmt"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

// RebaseFailureError is the typed error Rebase returns when the
// rebase-onto-default-branch step fails. Carries the diagnostic context
// the CLI chain-back needs to launch a one-shot agent inside the
// workspace: the branch/workspace paths it'll cd into, the raw git
// output that triggered the failure, and the unmerged paths git left
// before --abort discarded the rebase state.
//
// Workspace rebase has no Dirty shape because refuseDirty fires before
// the rebase is attempted — the resolver never sees the "rebase refused
// to start" case the session-close path has to handle.
type RebaseFailureError struct {
	Branch        string
	WorkspacePath string
	// Conflicts lists the unmerged paths git flagged before --abort
	// discarded the rebase state. Empty when the git status read
	// between rebase failure and --abort itself failed.
	Conflicts []string
	// GitOutput is the verbatim stdout+stderr of the failing rebase,
	// trimmed. Surfaced to the agent so it can read what git said.
	GitOutput string
}

func (e *RebaseFailureError) Error() string {
	return fmt.Sprintf(
		"workspace rebase: rebase %s onto main failed\n"+
			"  workspace: %s\n"+
			"  branch:    %s\n"+
			"  resolve by hand (cd into the workspace, rebase, commit), then re-run `moe workspace rebase`\n"+
			"  git output:\n%s",
		e.Branch, e.WorkspacePath, e.Branch, e.GitOutput)
}

// RebaseResult is what a successful Rebase returns. The two SHA pairs
// let the CLI report what moved without re-querying git.
type RebaseResult struct {
	// Branch is the workspace's current branch (the one HEAD pointed at
	// when Rebase started). "" when HEAD was the default branch itself
	// — in that case Rebase only advanced the default branch.
	Branch string
	// DefaultBranch is the project's default branch (almost always
	// "main"); used by the CLI's success message.
	DefaultBranch string
	// MainBefore / MainAfter bracket the FF of the workspace's local
	// default branch. Equal SHAs mean nothing moved.
	MainBefore, MainAfter string
	// BranchBefore / BranchAfter bracket the rebase of HEAD. Equal SHAs
	// mean the branch was already up-to-date (or HEAD was the default
	// branch itself, in which case both are MainAfter).
	BranchBefore, BranchAfter string
}

// Rebase advances the workspace's view of the canonical's default
// branch and rebases the workspace's current branch onto it.
//
// Steps:
//
//  1. Refuse if the workspace directory is missing — same shape as
//     release / refresh. Caller's responsibility to check Exists first;
//     surfaced as a plain error so the CLI can format it.
//  2. Refuse if the working tree is dirty. Reuse refuseDirty so the
//     handoff guarantee Attach gives also gates the rebase verb.
//  3. Refuse if HEAD is detached. The verb is "rebase a workspace
//     branch"; a detached HEAD has no named branch to advance.
//  4. `git fetch origin <defaultBranch>` in the workspace. Local-only
//     against the alternates link to the canonical submodule on disk;
//     surfaces canonical's current default-branch tip as
//     origin/<defaultBranch>.
//  5. Fast-forward the workspace's local default branch to
//     origin/<defaultBranch>. Refuse on non-fast-forward — a local
//     default diverging from canonical is a state we shouldn't paper
//     over. When HEAD is the default branch, `merge --ff-only` brings
//     ref, index, and worktree forward atomically; otherwise
//     `update-ref` moves the ref without switching HEAD off the
//     feature branch about to be rebased.
//  6. If HEAD is the default branch, step 5 already advanced it; return
//     the SHA delta and exit. Otherwise rebase HEAD onto the default
//     branch. On failure: abort the rebase, capture conflict paths,
//     return *RebaseFailureError so the CLI can chain back to a
//     resolver agent.
func Rebase(workspacePath, defaultBranch string) (RebaseResult, error) {
	if defaultBranch == "" {
		return RebaseResult{}, fmt.Errorf("workspace rebase: default branch is required")
	}
	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		return RebaseResult{}, fmt.Errorf("workspace rebase: %s does not exist on disk", workspacePath)
	} else if err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: stat %s: %w", workspacePath, err)
	}

	if err := refuseDirty(workspacePath); err != nil {
		return RebaseResult{}, err
	}

	headBranch, err := currentBranch(workspacePath)
	if err != nil {
		return RebaseResult{}, err
	}
	if headBranch == "" {
		return RebaseResult{}, fmt.Errorf(
			"workspace rebase: HEAD in %s is detached — check out a branch first",
			workspacePath)
	}

	if out, err := git.Combined(workspacePath, "fetch", "origin", defaultBranch); err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: git fetch origin %s in %s: %w (%s)",
			defaultBranch, workspacePath, err, out)
	}

	originRef := "refs/remotes/origin/" + defaultBranch
	if !git.HasRef(workspacePath, originRef) {
		return RebaseResult{}, fmt.Errorf(
			"workspace rebase: %s has no %s after fetch — check the project's default_branch in project.json",
			workspacePath, originRef)
	}
	originSHA, err := git.RevParse(workspacePath, originRef)
	if err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: rev-parse %s: %w", originRef, err)
	}

	mainRef := "refs/heads/" + defaultBranch
	mainBefore, err := git.RevParse(workspacePath, mainRef)
	if err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: rev-parse %s: %w", mainRef, err)
	}

	// Advance the local default branch to origin's tip. Two primitives
	// because HEAD may or may not be on the default branch:
	//
	//   - HEAD == default branch: `merge --ff-only` updates ref, index,
	//     and worktree atomically, and enforces FF-only natively. That's
	//     the right shape — we want the workspace's worktree to reflect
	//     the new tip.
	//   - HEAD on a feature branch: `update-ref` moves the ref without
	//     switching HEAD (the rebase below switches it for us). The
	//     merge-base check enforces FF-only by hand since update-ref
	//     doesn't.
	mainAfter := mainBefore
	if mainBefore != originSHA {
		if !git.Probe(workspacePath, "merge-base", "--is-ancestor", mainBefore, originSHA) {
			return RebaseResult{}, fmt.Errorf(
				"workspace rebase: local %s (%s) has diverged from origin/%s (%s)\n"+
					"  workspace: %s\n"+
					"  resolve by hand (or `moe sync` if the canonical needs to catch up)",
				defaultBranch, git.ShortSHA(mainBefore), defaultBranch, git.ShortSHA(originSHA),
				workspacePath)
		}
		if headBranch == defaultBranch {
			if out, err := git.Combined(workspacePath, "merge", "--ff-only", originRef); err != nil {
				return RebaseResult{}, fmt.Errorf("workspace rebase: ff-merge %s in %s: %w (%s)",
					originRef, workspacePath, err, out)
			}
		} else {
			if err := git.Run(workspacePath, "update-ref", mainRef, originSHA, mainBefore); err != nil {
				return RebaseResult{}, fmt.Errorf("workspace rebase: fast-forward %s %s..%s: %w",
					mainRef, git.ShortSHA(mainBefore), git.ShortSHA(originSHA), err)
			}
		}
		mainAfter = originSHA
	}

	result := RebaseResult{
		Branch:        headBranch,
		DefaultBranch: defaultBranch,
		MainBefore:    mainBefore,
		MainAfter:     mainAfter,
	}

	if headBranch == defaultBranch {
		result.BranchBefore = mainBefore
		result.BranchAfter = mainAfter
		return result, nil
	}

	// HEAD is a feature branch. Rebase onto the freshly-advanced local
	// default branch. Conflict files are read before --abort discards
	// the rebase state — same pattern session.Close uses.
	branchBefore, err := git.RevParse(workspacePath, "HEAD")
	if err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: rev-parse HEAD: %w", err)
	}
	if out, err := git.Combined(workspacePath, "rebase", defaultBranch); err != nil {
		trimmed := strings.TrimSpace(out)
		conflicts := unmergedPaths(workspacePath)
		_, _ = git.Combined(workspacePath, "rebase", "--abort")
		return RebaseResult{}, &RebaseFailureError{
			Branch:        headBranch,
			WorkspacePath: workspacePath,
			Conflicts:     conflicts,
			GitOutput:     trimmed,
		}
	}
	branchAfter, err := git.RevParse(workspacePath, "HEAD")
	if err != nil {
		return RebaseResult{}, fmt.Errorf("workspace rebase: rev-parse HEAD after rebase: %w", err)
	}
	result.BranchBefore = branchBefore
	result.BranchAfter = branchAfter
	return result, nil
}

// currentBranch returns the short branch name of HEAD, or "" if HEAD
// is detached. `rev-parse --symbolic-full-name HEAD` returns
// "refs/heads/<name>" for a branch and the literal "HEAD" for a
// detached HEAD, so the empty-branch case is a single string compare.
func currentBranch(workspacePath string) (string, error) {
	out, err := git.Output(workspacePath, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return "", fmt.Errorf("workspace rebase: rev-parse HEAD in %s: %w", workspacePath, err)
	}
	ref := strings.TrimSpace(out)
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) {
		return "", nil
	}
	return strings.TrimPrefix(ref, prefix), nil
}

// unmergedPaths reports the files git left in a conflicted state
// (UU/AA/DD/...) at the time of the failing rebase, before --abort
// discards the rebase state. Same shape as session.sessionUnmergedPaths.
// Returns nil on git.Status failure (the kickoff just lists no files;
// the agent can still run `git status` itself).
func unmergedPaths(workspacePath string) []string {
	entries, err := git.Status(workspacePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if len(e.XY) < 2 {
			continue
		}
		if e.XY[0] == 'U' || e.XY[1] == 'U' || e.XY == "AA" || e.XY == "DD" {
			out = append(out, e.Path)
		}
	}
	return out
}
