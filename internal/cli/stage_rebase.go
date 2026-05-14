package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	_ "github.com/modulecollective/moe/internal/agent/claude"
	"github.com/modulecollective/moe/internal/session"
)

// rebaseAutoResolveTimeout caps the chain-back agent's wall-time.
// 5min matches wiki finalize's budget — the failure mode this targets
// (submodule pointer bump on main while a session was open) is sub-30-
// seconds work; longer timeouts only help genuinely ambiguous conflicts
// the agent probably shouldn't be resolving autonomously anyway.
const rebaseAutoResolveTimeout = 5 * time.Minute

// closeWithAutoResolve wraps a close closure: on *RebaseFailureError,
// release the lock (already done — closeSess holds it inside its
// closure), launch a one-shot agent in the session worktree to
// resolve, then retry close once. Single-shot: if the second attempt
// still fails the caller sees the typed error and the operator falls
// back to today's `moe session resolve` / `moe session abandon` path.
// Other error types pass through unchanged.
func closeWithAutoResolve(closeSess func() error, stdout, stderr io.Writer) error {
	err := closeSess()
	var rebaseFail *session.RebaseFailureError
	if !errors.As(err, &rebaseFail) {
		return err
	}
	moePrintf(stderr, "session close: %v\n", err)
	moePrintln(stderr, "  launching an agent to resolve the rebase; single-shot — if it can't, the message above is what you'll see again")

	prompt := buildSessionRebaseResolveKickoff(rebaseFail)
	if agentErr := launchSessionRebaseResolve(rebaseFail.WorktreePath, prompt, stdout, stderr); agentErr != nil {
		moePrintf(stderr, "  auto-resolve agent: %v\n", agentErr)
	}
	return closeSess()
}

// launchSessionRebaseResolve fires the one-shot agent inside the
// session worktree. Overridable in tests so closeWithAutoResolve can
// be exercised end-to-end without spinning a real claude subprocess.
var launchSessionRebaseResolve = func(worktreePath, userPrompt string, stdout, stderr io.Writer) error {
	// The chain-back rebase resolver is single-binary by construction
	// (it's a session-close fallback, not a stage turn), so it pins
	// claude rather than reading from any per-run agent setting.
	a, err := agent.Get("claude")
	if err != nil {
		return err
	}
	_, err = a.ExecuteOneShot(agent.OneShotRequest{
		Root:       worktreePath,
		Prompt:     sessionRebaseResolveSystemPrompt,
		UserPrompt: userPrompt,
		ClonePath:  worktreePath,
		Stdout:     stdout,
		Stderr:     stderr,
		Timeout:    rebaseAutoResolveTimeout,
	})
	return err
}

// sessionRebaseResolveSystemPrompt is the system-prompt addendum for
// the chain-back agent. Tight scope: it's not editing the canvas, just
// landing the session branch on main. The "must not" list mirrors the
// design's enumeration — no pushing, no force, no touching other
// branches, no hook bypass, no nuking the worktree from above.
const sessionRebaseResolveSystemPrompt = `You are resolving a failed rebase of a stage-session branch onto main.

Scope:
- You are inside the session worktree. cwd is the worktree root.
- Your single job is to get the session branch rebased onto main with a clean
  working tree, then exit. The outer "moe session close" will retry the rebase
  + ff-merge after you exit; if your work is correct, the retry is a no-op.

Do:
- Read git status first to see what state the worktree is in.
- Run "git rebase main" yourself and resolve conflicts as they come up.
- Preserve the session's intent (the work on the branch) over main's edits
  when conflicts involve the branch's actual content; let main win for
  submodule-pointer bumps and other bureaucracy-side drift.
- Commit (or "git rebase --continue") as you go.

Do NOT:
- Push, force-push, or touch the remote.
- Use --no-verify, --no-gpg-sign, or any hook-bypass flag.
- Switch branches, delete branches, or check out anything other than the
  current session branch.
- Run "git reset --hard" outside the worktree, or rm -rf the worktree.
- Edit the canvas content as part of conflict resolution — the canvas is
  the work; conflicts there are real and should be resolved keeping the
  session's intent intact, not erased.

When the worktree is clean and "git rebase main" reports nothing to do,
you're done. Exit.`

// buildSessionRebaseResolveKickoff is the agent-facing user prompt
// describing the specific failure. Two shapes keyed off Dirty: dirty
// = the rebase never started, so the agent first cleans up; conflict
// = the rebase hit conflicts in named files. Always includes the raw
// git output so the agent can read what git actually said.
func buildSessionRebaseResolveKickoff(e *session.RebaseFailureError) string {
	var b strings.Builder
	if e.Dirty {
		fmt.Fprintf(&b, "`moe session close` tried to rebase %s onto main and the rebase refused to start because the worktree has uncommitted or unstaged changes. This is almost always leftover dirt from a prior aborted rebase — conflict-marker residue or partially-applied hunks, not real work.\n\n", e.Branch)
		b.WriteString("Start by reading `git status` inside the worktree. Either commit the residue (if it really is intentional work) or reset it away (`git checkout -- .` for tracked-file dirt, `git clean -fd` for stray untracked files from a half-applied rebase). Then run `git rebase main` and resolve any conflicts that come up.\n\n")
	} else {
		fmt.Fprintf(&b, "`moe session close` tried to rebase %s onto main and hit conflicts. The rebase has been aborted, so the working tree is clean and the branch is back at its pre-rebase tip — you are starting from the conflict state, not mid-rebase.\n\n", e.Branch)
		if len(e.Conflicts) > 0 {
			b.WriteString("Files git flagged as conflicting on the abandoned rebase:\n")
			for _, p := range e.Conflicts {
				fmt.Fprintf(&b, "  - %s\n", p)
			}
			b.WriteString("\n")
		}
		b.WriteString("Re-run the rebase yourself (`git rebase main`), resolve the conflicts, and commit (or `git rebase --continue`) as you go. Then exit — the outer close will retry the rebase + ff-merge.\n\n")
	}
	if e.GitOutput != "" {
		b.WriteString("Raw git output that triggered this:\n\n")
		b.WriteString(e.GitOutput)
		if !strings.HasSuffix(e.GitOutput, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}
