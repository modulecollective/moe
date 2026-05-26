package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/workspace"
)

// TestRunWorkspaceRebaseHappyPath: a workspace on a feature branch,
// canonical advanced one commit — the verb should FF the workspace's
// local main, rebase the feature branch on top, and print a delta
// line. Exercises the end-to-end CLI plumbing including
// defaultBranchForProject.
func TestRunWorkspaceRebaseHappyPath(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Materialize a workspace on a feature branch by going through the
	// same Acquire+Attach pair attachRunWorkspace would use. This
	// avoids running runNew (which would also set up bureaucracy state
	// we don't need for the rebase verb).
	wp, err := workspace.Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := workspace.Attach(wp, "moe/feature", "main"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// One feature commit so the rebase has something to move.
	writeFile(t, filepath.Join(wp, "feature.txt"), "a\n")
	gittest.Run(t, wp, "add", "feature.txt")
	gittest.Run(t, wp, "commit", "-m", "feature")

	// Advance the canonical's main by one commit.
	src := filepath.Join(root, "projects", "tele", "src")
	gittest.WriteAndCommit(t, src, "advance.txt", "advance\n", "advance main")

	prev := launchRebaseResolve
	launchRebaseResolve = func(_, _ string, _, _ io.Writer) error {
		t.Fatal("happy path must not fire the resolver agent")
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	var out, errb bytes.Buffer
	code := runWorkspaceRebase([]string{"tele/dev"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	for _, want := range []string{"workspace tele/dev", "main", "moe/feature"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q: %q", want, out.String())
		}
	}
}

// TestRunWorkspaceRebaseConflictFiresResolverAndRetries pins the
// chain-back contract: on *workspace.RebaseFailureError, fire the
// shared one-shot resolver in the workspace dir and retry once. Stubs
// launchRebaseResolve to run the resolution itself (rebase -X theirs)
// so the retry's workspace.Rebase call succeeds.
func TestRunWorkspaceRebaseConflictFiresResolverAndRetries(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	wp, err := workspace.Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Attach(wp, "moe/feature", "main"); err != nil {
		t.Fatal(err)
	}
	// Branch edits README.md.
	writeFile(t, filepath.Join(wp, "README.md"), "seed\nfrom-branch\n")
	gittest.Run(t, wp, "add", "README.md")
	gittest.Run(t, wp, "commit", "-m", "branch edit")

	// Canonical edits the same file differently.
	src := filepath.Join(root, "projects", "tele", "src")
	gittest.WriteAndCommit(t, src, "README.md", "seed\nfrom-canonical\n", "canonical edit")

	var capturedDir, capturedPrompt string
	calls := 0
	prev := launchRebaseResolve
	launchRebaseResolve = func(worktreePath, userPrompt string, _, _ io.Writer) error {
		calls++
		capturedDir = worktreePath
		capturedPrompt = userPrompt
		// Stand in for the resolver: rebase the branch onto main using
		// the branch's side of every conflict. The retry's
		// workspace.Rebase call then runs against a branch already on
		// top of main and succeeds as a no-op rebase.
		gittest.Run(t, worktreePath, "rebase", "-X", "theirs", "main")
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	var out, errb bytes.Buffer
	code := runWorkspaceRebase([]string{"tele/dev"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if calls != 1 {
		t.Errorf("launchRebaseResolve called %d times, want 1", calls)
	}
	if capturedDir != wp {
		t.Errorf("agent worktree = %q, want %q", capturedDir, wp)
	}
	for _, want := range []string{"moe/feature", "README.md"} {
		if !strings.Contains(capturedPrompt, want) {
			t.Errorf("kickoff missing %q: %q", want, capturedPrompt)
		}
	}
	if !strings.Contains(errb.String(), "launching an agent") {
		t.Errorf("stderr should announce the chain-back: %q", errb.String())
	}
}

// TestRunWorkspaceRebaseConflictRetryFailsSurfacesTypedError covers
// the single-shot exit: when the resolver returns but the retry still
// hits the conflict, the typed-error message reprints and exit is
// non-zero. Mirrors the session-close stage_rebase_test.go pin.
func TestRunWorkspaceRebaseConflictRetryFailsSurfacesTypedError(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	wp, err := workspace.Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.Attach(wp, "moe/feature", "main"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wp, "README.md"), "seed\nfrom-branch\n")
	gittest.Run(t, wp, "add", "README.md")
	gittest.Run(t, wp, "commit", "-m", "branch edit")
	src := filepath.Join(root, "projects", "tele", "src")
	gittest.WriteAndCommit(t, src, "README.md", "seed\nfrom-canonical\n", "canonical edit")

	calls := 0
	prev := launchRebaseResolve
	launchRebaseResolve = func(_, _ string, _, _ io.Writer) error {
		calls++
		// "Agent" returns without doing anything; retry will hit the
		// same conflict.
		return nil
	}
	t.Cleanup(func() { launchRebaseResolve = prev })

	var out, errb bytes.Buffer
	code := runWorkspaceRebase([]string{"tele/dev"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit when retry still fails; stdout=%q", out.String())
	}
	if calls != 1 {
		t.Errorf("launchRebaseResolve called %d times, want 1 (single-shot)", calls)
	}
	if !strings.Contains(errb.String(), "rebase moe/feature onto main failed") {
		t.Errorf("stderr should reprint the typed error: %q", errb.String())
	}
}

// TestRunWorkspaceRebaseRefusesMissingWorkspace exits non-zero with a
// pointer to `moe workspace new`. Mirrors the release / refresh
// "refuse if not on disk" gate.
func TestRunWorkspaceRebaseRefusesMissingWorkspace(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runWorkspaceRebase([]string{"tele/ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing workspace; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "does not exist") {
		t.Errorf("stderr should name the missing dir: %q", errb.String())
	}
}
