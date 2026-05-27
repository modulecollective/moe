package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// newSessionTestRoot seeds a bureaucracy-like repo with a seed commit
// so session worktrees have something to branch from. Deliberately
// separate from newTestBureaucracy in stage_test.go to keep this file
// self-contained.
func newSessionTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	// The ambient $MOE_HOME (from the developer's shell) would steer
	// bureaucracy.Find at the real repo instead of this temp one. Clear it.
	t.Setenv(bureaucracy.EnvHome, "")
	gittest.Commit(t, root, "seed")
	// Plant the bureaucracy marker so findRoot's Find succeeds when
	// the CLI subcommand runs against it via chdir.
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// withCwd runs fn with cwd set to dir, restoring afterward. Session
// subcommands discover the bureaucracy root via cwd + Find, so tests
// that exercise the CLI wrappers have to chdir in.
func withCwd(t *testing.T, dir string, fn func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	fn()
}

func TestSessionListEmpty(t *testing.T) {
	root := newSessionTestRoot(t)
	var stdout, stderr bytes.Buffer
	withCwd(t, root, func() {
		if code := Run([]string{"session", "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), "no open sessions") {
		t.Errorf("stdout = %q, want to contain 'no open sessions'", stdout.String())
	}
}

func TestSessionListAndAbandon(t *testing.T) {
	root := newSessionTestRoot(t)
	s, err := session.Open(root, "demo", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var stdout, stderr bytes.Buffer
	withCwd(t, root, func() {
		if code := Run([]string{"session", "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("list exit=%d stderr=%q", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), s.Branch) {
		t.Errorf("list stdout missing branch %q:\n%s", s.Branch, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	withCwd(t, root, func() {
		if code := Run([]string{"session", "abandon", s.Branch}, &stdout, &stderr); code != 0 {
			t.Fatalf("abandon exit=%d stderr=%q", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), "abandoned") {
		t.Errorf("abandon stdout = %q", stdout.String())
	}
	// List should now be empty.
	stdout.Reset()
	stderr.Reset()
	withCwd(t, root, func() {
		if code := Run([]string{"session", "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("list-after exit=%d stderr=%q", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), "no open sessions") {
		t.Errorf("post-abandon list = %q", stdout.String())
	}
}

func TestSessionAbandonUnknownBranch(t *testing.T) {
	root := newSessionTestRoot(t)
	var stdout, stderr bytes.Buffer
	withCwd(t, root, func() {
		if code := Run([]string{"session", "abandon", "session/no/such/doc"}, &stdout, &stderr); code != 1 {
			t.Fatalf("exit=%d, want 1; stderr=%q", code, stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "no session found") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// runSessionGCInRoot runs `moe session gc` against root. Tests use it
// to avoid the chdir dance for every assertion.
func runSessionGCInRoot(t *testing.T, root string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var code int
	withCwd(t, root, func() {
		code = Run([]string{"session", "gc"}, &stdout, &stderr)
	})
	return code, stdout.String(), stderr.String()
}

// TestSessionGCReapsTerminalRun covers rule 1: a session whose run has
// reached a terminal status (merged here) is reaped on the next gc.
func TestSessionGCReapsTerminalRun(t *testing.T) {
	root := newSessionTestRoot(t)
	trailerstest.SeedRun(t, root, "alpha", "merged-one", "sdlc", run.StatusMerged)
	s, err := session.Open(root, "alpha", "merged-one", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "removed "+s.Branch) {
		t.Errorf("stdout missing reap line for %s:\n%s", s.Branch, stdout)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after gc: %v", err)
	}
}

// TestSessionGCReapsMissingRunJson covers rule 2: a session whose
// run.json is gone — manually deleted, lost in a botched commit — is
// reaped on the next gc.
func TestSessionGCReapsMissingRunJson(t *testing.T) {
	root := newSessionTestRoot(t)
	trailerstest.SeedRun(t, root, "alpha", "lost-json", "sdlc", run.StatusInProgress)
	s, err := session.Open(root, "alpha", "lost-json", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "projects", "alpha", "runs", "lost-json", "run.json")); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "removed "+s.Branch) {
		t.Errorf("stdout missing reap line for %s:\n%s", s.Branch, stdout)
	}
}

// TestSessionGCReapsMissingProject covers rule 3: a session whose
// project directory is gone (the operator wiped projects/<project>/
// after a renamed-or-deleted decision) is reaped on the next gc.
func TestSessionGCReapsMissingProject(t *testing.T) {
	root := newSessionTestRoot(t)
	trailerstest.SeedRun(t, root, "alpha", "doomed", "sdlc", run.StatusInProgress)
	s, err := session.Open(root, "alpha", "doomed", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "projects", "alpha")); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "removed "+s.Branch) {
		t.Errorf("stdout missing reap line for %s:\n%s", s.Branch, stdout)
	}
}

// TestSessionGCReapsOrphanBranch covers rule 4: a bare
// `refs/heads/session/<p>/<r>/<d>` ref with no worktree (the exact case
// session.Open refuses with "abandoned close?") is reaped on the next
// gc.
func TestSessionGCReapsOrphanBranch(t *testing.T) {
	root := newSessionTestRoot(t)
	gittest.Run(t, root, "branch", "session/alpha/orphan/design")

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "removed session/alpha/orphan/design") {
		t.Errorf("stdout missing reap line:\n%s", stdout)
	}
	// Branch must be gone.
	if git.Probe(root, "rev-parse", "--verify", "--quiet", "refs/heads/session/alpha/orphan/design") {
		t.Errorf("branch still present after gc")
	}
}

// TestSessionGCReapsOrphanWorktreeDir covers rule 5: a directory under
// `.moe/worktrees/<uuid>/` that git's `worktree list` doesn't know
// about (mid-add abort, manual rm of the canonical registration, …) is
// removed on the next gc.
func TestSessionGCReapsOrphanWorktreeDir(t *testing.T) {
	root := newSessionTestRoot(t)
	wt := filepath.Join(root, ".moe", "worktrees", "00000000-0000-4000-8000-000000000000")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "stub"), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "removed worktree dir 00000000-0000-4000-8000-000000000000") {
		t.Errorf("stdout missing dir reap:\n%s", stdout)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir still present: %v", err)
	}
}

// TestSessionGCNoOrphans pins the empty path: a clean bureaucracy
// surfaces a status line rather than producing no output, so the
// operator can confirm gc ran.
func TestSessionGCNoOrphans(t *testing.T) {
	root := newSessionTestRoot(t)
	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "no orphan sessions") {
		t.Errorf("expected 'no orphan sessions', got %q", stdout)
	}
}

// TestSessionGCPartialFailureExits1 covers the partial-failure
// contract: one reapable orphan succeeds, one fails because its
// worktree dir is read-only at the parent level, the surviving orphan
// stays on disk (and on `moe session list`), and exit is 1. Mirrors
// the same contract `moe clone gc` already ships.
func TestSessionGCPartialFailureExits1(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission checks")
	}
	root := newSessionTestRoot(t)

	// Reapable: a clean orphan branch.
	gittest.Run(t, root, "branch", "session/alpha/orphan/design")

	// Un-reapable: a rule-5 worktree dir whose parent is locked, so
	// os.RemoveAll fails. The parent-chmod technique mirrors the
	// sandbox.TestRemoveSurfacesPermissionHint setup.
	wtDir := filepath.Join(root, ".moe", "worktrees", "00000000-0000-4000-8000-000000000001")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "stuck"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wtDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(wtDir, 0o755) })

	code, stdout, stderr := runSessionGCInRoot(t, root)
	if code != 1 {
		t.Fatalf("exit=%d, want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "removed session/alpha/orphan/design") {
		t.Errorf("reapable orphan missing from stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "session gc:") || !strings.Contains(stderr, wtDir) {
		t.Errorf("stderr missing the failure mention of %s:\n%s", wtDir, stderr)
	}
	// Surviving orphan stays on disk so a follow-up gc (after the
	// operator fixes perms) still picks it up.
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("surviving orphan dir gone: %v", err)
	}
}
