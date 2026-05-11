package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/session"
)

// newSessionTestRoot seeds a bureaucracy-like repo with a seed commit
// so session worktrees have something to branch from. Deliberately
// separate from newTestBureaucracy in stage_test.go to keep this file
// self-contained.
func newSessionTestRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	// The ambient $MOE_HOME (from the developer's shell) would steer
	// bureaucracy.Find at the real repo instead of this temp one. Clear it.
	t.Setenv(bureaucracy.EnvHome, "")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
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
