package cli

import (
	"io"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	_ "github.com/modulecollective/moe/internal/agent/claude"
)

// rebaseAutoResolveTimeout caps the chain-back agent's wall-time.
// 5min matches wiki finalize's budget — the failure mode this targets
// (submodule pointer bump on main while a session was open, canonical
// main moved forward while a workspace held a branch) is sub-30-seconds
// work; longer timeouts only help genuinely ambiguous conflicts the
// agent probably shouldn't be resolving autonomously anyway.
const rebaseAutoResolveTimeout = 5 * time.Minute

// launchRebaseResolve fires the one-shot agent inside a worktree to
// resolve a stalled rebase. Two callers share this entry point:
// session-close (the bureaucracy-side session worktree) and workspace
// rebase (a named-workspace clone of a project's submodule). Both want
// the same tight system prompt and the same 5-minute budget; the
// per-caller kickoff carries the branch / conflict context.
//
// Overridable in tests so callers can be exercised end-to-end without
// spinning a real claude subprocess.
var launchRebaseResolve = func(worktreePath, userPrompt string, stdout, stderr io.Writer) error {
	// The chain-back rebase resolver is single-binary by construction
	// (it's a recovery fallback, not a stage turn), so it pins claude
	// rather than reading from any per-run agent setting.
	a, err := agent.Get("claude")
	if err != nil {
		return err
	}
	_, err = a.ExecuteOneShot(agent.OneShotRequest{
		Root:       worktreePath,
		Prompt:     rebaseResolveSystemPrompt,
		UserPrompt: userPrompt,
		ClonePath:  worktreePath,
		Stdout:     stdout,
		Stderr:     stderr,
		Timeout:    rebaseAutoResolveTimeout,
	})
	return err
}

// rebaseResolveSystemPrompt is the system-prompt addendum for the
// chain-back agent. Tight scope: it's landing the current branch on
// its target main, not editing any document. The "must not" list
// enumerates the destructive escape hatches we don't want the agent
// reaching for — push, force, hook bypass, branch switching, nuking
// the worktree.
const rebaseResolveSystemPrompt = `You are resolving a failed rebase of a branch onto main.

Scope:
- You are inside the worktree (a session worktree or a named workspace).
  cwd is the worktree root.
- Your single job is to get the current branch rebased onto main with a
  clean working tree, then exit. The outer caller ("moe session close" or
  "moe workspace rebase") will retry the rebase after you exit; if your
  work is correct, the retry is a no-op.

Do:
- Read git status first to see what state the worktree is in.
- Run "git rebase main" yourself and resolve conflicts as they come up.
- Preserve the branch's intent (the work on the branch) over main's edits
  when conflicts involve the branch's actual content; let main win for
  submodule-pointer bumps and other bureaucracy-side drift.
- Commit (or "git rebase --continue") as you go.

Do NOT:
- Push, force-push, or touch the remote.
- Use --no-verify, --no-gpg-sign, or any hook-bypass flag.
- Switch branches, delete branches, or check out anything other than the
  current branch.
- Run "git reset --hard" outside the worktree, or rm -rf the worktree.
- Edit canvas content as part of conflict resolution — when conflicts
  hit document content, resolve them keeping the branch's intent intact,
  not erased.

When the worktree is clean and "git rebase main" reports nothing to do,
you're done. Exit.`
