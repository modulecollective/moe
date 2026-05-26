package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/modulecollective/moe/internal/session"
)

// closeWithAutoResolve wraps a close closure: on *RebaseFailureError,
// release the lock (already done — closeSess holds it inside its
// closure), launch a one-shot agent in the session worktree to
// resolve, then retry close once. Single-shot: if the second attempt
// still fails the caller sees the typed error and the operator falls
// back to today's `moe session resolve` / `moe session abandon` path.
// Other error types pass through unchanged.
//
// okToPush is threaded through to both attempts so the in-closure
// sync.AutoPush is suppressed when the turn that triggered the close
// didn't succeed. See openWikiSession's closeSess for the rationale.
func closeWithAutoResolve(closeSess func(okToPush bool) error, okToPush bool, stdout, stderr io.Writer) error {
	err := closeSess(okToPush)
	var rebaseFail *session.RebaseFailureError
	if !errors.As(err, &rebaseFail) {
		return err
	}
	moePrintf(stderr, "session close: %v\n", err)
	moePrintln(stderr, "  launching an agent to resolve the rebase; single-shot — if it can't, the message above is what you'll see again")

	prompt := buildSessionRebaseResolveKickoff(rebaseFail)
	if agentErr := launchRebaseResolve(rebaseFail.WorktreePath, prompt, stdout, stderr); agentErr != nil {
		moePrintf(stderr, "  auto-resolve agent: %v\n", agentErr)
	}
	return closeSess(okToPush)
}

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
