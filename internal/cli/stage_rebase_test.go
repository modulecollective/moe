package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/session"
)

// TestCloseWithAutoResolveLaunchesAgentOnRebaseFailure pins the
// chain-back contract: on *RebaseFailureError, fire the one-shot
// agent inside the session worktree with the failure context as the
// user prompt, then retry close. Stubs launchSessionRebaseResolve so
// the test process doesn't need a real claude on PATH.
func TestCloseWithAutoResolveLaunchesAgentOnRebaseFailure(t *testing.T) {
	rfe := &session.RebaseFailureError{
		Branch:       "session/moe/r1/code",
		WorktreePath: "/tmp/sessions/r1",
		Conflicts:    []string{"projects/moe/runs/r1/scratch.md"},
		GitOutput:    "CONFLICT (content): Merge conflict in projects/moe/runs/r1/scratch.md",
	}

	var capturedWorktree, capturedPrompt string
	prev := launchRebaseResolve
	launchRebaseResolve = func(worktreePath, userPrompt string, _, _ io.Writer) error {
		capturedWorktree = worktreePath
		capturedPrompt = userPrompt
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	calls := 0
	var lastOkToPush bool
	closeSess := func(okToPush bool) error {
		calls++
		lastOkToPush = okToPush
		if calls == 1 {
			return rfe
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	if err := closeWithAutoResolve(closeSess, true, &stdout, &stderr); err != nil {
		t.Fatalf("closeWithAutoResolve: %v", err)
	}
	if !lastOkToPush {
		t.Errorf("retry should reuse the original okToPush; got false")
	}
	if calls != 2 {
		t.Errorf("closeSess called %d times, want 2 (first triggers chain-back, second is the retry)", calls)
	}
	if capturedWorktree != rfe.WorktreePath {
		t.Errorf("agent worktree = %q, want %q", capturedWorktree, rfe.WorktreePath)
	}
	if !strings.Contains(capturedPrompt, rfe.Branch) {
		t.Errorf("kickoff missing branch name: %q", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "scratch.md") {
		t.Errorf("kickoff missing conflict path: %q", capturedPrompt)
	}
	if !strings.Contains(stderr.String(), "launching an agent") {
		t.Errorf("stderr should announce the chain-back: %q", stderr.String())
	}
}

// TestCloseWithAutoResolveRetriesOnceThenSurfacesTypedError: the
// chain-back is single-shot. If the second close also returns a
// typed *RebaseFailureError, closeWithAutoResolve returns that error
// untouched so the caller's reportWikiSessionExit prints today's
// "resolve by hand" message.
func TestCloseWithAutoResolveRetriesOnceThenSurfacesTypedError(t *testing.T) {
	rfe := &session.RebaseFailureError{
		Branch:       "session/moe/r1/code",
		WorktreePath: "/tmp/sessions/r1",
		GitOutput:    "stuck",
	}

	prev := launchRebaseResolve
	launchRebaseResolve = func(_, _ string, _, _ io.Writer) error {
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	calls := 0
	closeSess := func(okToPush bool) error {
		calls++
		return rfe
	}

	err := closeWithAutoResolve(closeSess, true, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected typed error to surface after retry, got nil")
	}
	var got *session.RebaseFailureError
	if !errors.As(err, &got) {
		t.Errorf("expected *RebaseFailureError to surface unchanged, got %T", err)
	}
	if calls != 2 {
		t.Errorf("closeSess called %d times, want 2 (single-shot retry)", calls)
	}
}

// TestCloseWithAutoResolvePassesNonRebaseErrorsUnchanged: any error
// that isn't a *RebaseFailureError (commit failure, worktree-remove
// failure, etc.) passes through without firing the agent or retrying.
func TestCloseWithAutoResolvePassesNonRebaseErrorsUnchanged(t *testing.T) {
	prev := launchRebaseResolve
	launchRebaseResolve = func(_, _ string, _, _ io.Writer) error {
		t.Fatal("agent must not fire for non-rebase errors")
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	wantErr := errors.New("some other close failure")
	calls := 0
	closeSess := func(okToPush bool) error {
		calls++
		return wantErr
	}

	got := closeWithAutoResolve(closeSess, true, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(got, wantErr) {
		t.Errorf("non-rebase error must pass through unchanged: got %v, want %v", got, wantErr)
	}
	if calls != 1 {
		t.Errorf("closeSess called %d times, want 1 (no retry for non-rebase errors)", calls)
	}
}

// TestBuildSessionRebaseResolveKickoffConflictShape: the conflict
// kickoff names the branch, the conflicting files, includes the raw
// git output, and tells the agent to re-run the rebase itself.
func TestBuildSessionRebaseResolveKickoffConflictShape(t *testing.T) {
	rfe := &session.RebaseFailureError{
		Branch:    "session/moe/r1/code",
		Conflicts: []string{"a.txt", "b.txt"},
		GitOutput: "CONFLICT (content): Merge conflict in a.txt",
	}
	got := buildSessionRebaseResolveKickoff(rfe)
	for _, want := range []string{
		"session/moe/r1/code",
		"a.txt",
		"b.txt",
		"git rebase main",
		"CONFLICT (content)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "refused to start") {
		t.Errorf("conflict shape leaked into dirty-shape wording:\n%s", got)
	}
}

// TestBuildSessionRebaseResolveKickoffDirtyShape: when Dirty is true,
// the kickoff opens with the "rebase refused" framing and tells the
// agent to clean up before retrying. Distinguishable from the
// conflict shape so the agent doesn't waste cycles trying to resolve
// a conflict that doesn't exist yet.
func TestBuildSessionRebaseResolveKickoffDirtyShape(t *testing.T) {
	rfe := &session.RebaseFailureError{
		Branch:    "session/moe/r1/code",
		GitOutput: "error: cannot rebase: You have unstaged changes.",
		Dirty:     true,
	}
	got := buildSessionRebaseResolveKickoff(rfe)
	for _, want := range []string{
		"session/moe/r1/code",
		"refused to start",
		"uncommitted or unstaged changes",
		"git status",
		"cannot rebase",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dirty kickoff missing %q:\n%s", want, got)
		}
	}
}
