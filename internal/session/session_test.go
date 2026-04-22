package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRoot seeds a git repo with one commit on main so that worktree
// creation has something to branch from. Mirrors the pattern in
// internal/run/run_test.go.
func newTestRoot(t *testing.T) string {
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
	return root
}

func commitInWorktree(t *testing.T, worktreePath, relPath, body, msg string) {
	t.Helper()
	abs := filepath.Join(worktreePath, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", relPath},
		{"commit", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = worktreePath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestOpenCloseRoundtrip(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Branch != "session/moe/r1/design" {
		t.Errorf("Branch = %q, want %q", s.Branch, "session/moe/r1/design")
	}
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}

	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\nhello\n", "work: update design")

	if err := Close(s); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Worktree and branch should be gone.
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Close: err=%v", err)
	}
	if branchExists(root, s.Branch) {
		t.Errorf("branch %s still present after Close", s.Branch)
	}

	// Main should now carry the content.
	mainHead, err := gitRevParse(root, "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	cmd := exec.Command("git", "cat-file", "-p",
		mainHead+":projects/moe/runs/r1/documents/design/content.md")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read landed file: %v", err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("landed content missing expected body: %q", out)
	}

	// The working tree at the canonical root must also reflect the
	// landed content — `update-ref` updates the ref but not the index
	// or worktree, so downstream commands that read files via os.Stat
	// would see stale state. Guard against regressing to that shape.
	diskPath := filepath.Join(root, "projects/moe/runs/r1/documents/design/content.md")
	body, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read landed file from working tree: %v", err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("working tree content missing expected body: %q", body)
	}
}

// TestCloseWithSubmodule guards against regressing to plain `git worktree
// remove`, which refuses with "working trees containing submodules
// cannot be moved or removed" whenever the superproject has a submodule
// checked out (as the bureaucracy root does for projects/*/src).
func TestCloseWithSubmodule(t *testing.T) {
	root := newTestRoot(t)

	// Donor repo to serve as the submodule source.
	donor := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "donor seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = donor
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("donor git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Add the donor as a submodule. `protocol.file.allow=always` is
	// needed since Git 2.38 (CVE-2022-39253) disabled file:// by default.
	addCmd := exec.Command("git", "-c", "protocol.file.allow=always",
		"submodule", "add", donor, "sub")
	addCmd.Dir = root
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git submodule add: %v\n%s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "-m", "add submodule")
	commitCmd.Dir = root
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit submodule: %v\n%s", err, out)
	}

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Init the submodule inside the new worktree. `git worktree add`
	// does not populate submodules by default, but in real bureaucracy
	// use the submodule ends up checked out in the session worktree.
	// Plain `git worktree remove` only refuses when the submodule is
	// actually present on disk, so this step is what makes the test
	// actually exercise the regression.
	initCmd := exec.Command("git", "-c", "protocol.file.allow=always",
		"submodule", "update", "--init")
	initCmd.Dir = s.WorktreePath
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("submodule update --init: %v\n%s", err, out)
	}

	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\n", "work: update design")

	if err := Close(s); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Close: err=%v", err)
	}
}

func TestOpenResumesExistingSession(t *testing.T) {
	root := newTestRoot(t)
	first, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	second, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if second.WorktreePath != first.WorktreePath {
		t.Errorf("resume landed on a different worktree: first=%s second=%s",
			first.WorktreePath, second.WorktreePath)
	}
	if second.Branch != first.Branch {
		t.Errorf("resume landed on a different branch: first=%s second=%s",
			first.Branch, second.Branch)
	}
	if err := Abandon(first); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
}

func TestOpenOrphanBranchErrors(t *testing.T) {
	root := newTestRoot(t)
	// Create a session branch without a worktree to simulate a busted
	// close state.
	cmd := exec.Command("git", "branch", "session/moe/r1/design")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if _, err := Open(root, "moe", "r1", "design"); err == nil {
		t.Fatal("expected error on orphan branch, got nil")
	} else if !strings.Contains(err.Error(), "without a registered worktree") {
		t.Fatalf("error does not point at the orphan state: %v", err)
	}
}

func TestCloseRebaseConflictLeavesSessionIntact(t *testing.T) {
	root := newTestRoot(t)
	// Seed main with an initial file.
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "shared.txt"},
		{"commit", "-m", "seed shared"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Session branch edits shared.txt.
	commitInWorktree(t, s.WorktreePath, "shared.txt", "session-edit\n", "session: touch shared")

	// Main moves ahead with a conflicting edit.
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte("main-edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "shared.txt"},
		{"commit", "-m", "main: touch shared"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
	}

	err = Close(s)
	if err == nil {
		t.Fatal("expected rebase conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Errorf("error does not mention rebase: %v", err)
	}
	// Worktree and branch remain, so the operator can fix by hand.
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Errorf("worktree missing after rebase failure: %v", err)
	}
	if !branchExists(root, s.Branch) {
		t.Errorf("branch missing after rebase failure")
	}
}

func TestAbandonRemovesWorktreeAndBranch(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Abandon(s); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Abandon")
	}
	if branchExists(root, s.Branch) {
		t.Errorf("branch still present after Abandon")
	}
}

func TestListIncludesOpenSessions(t *testing.T) {
	root := newTestRoot(t)
	a, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	b, err := Open(root, "moe", "r2", "code")
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer Abandon(a)
	defer Abandon(b)

	got, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2\n%+v", len(got), got)
	}
	branches := map[string]bool{}
	for _, s := range got {
		branches[s.Branch] = true
	}
	if !branches["session/moe/r1/design"] || !branches["session/moe/r2/code"] {
		t.Errorf("missing expected branches: %v", branches)
	}
}

func TestFindByBranchOnOrphanReturnsSynthetic(t *testing.T) {
	root := newTestRoot(t)
	cmd := exec.Command("git", "branch", "session/moe/r1/design")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	s, err := FindByBranch(root, "session/moe/r1/design")
	if err != nil {
		t.Fatalf("FindByBranch: %v", err)
	}
	if s == nil {
		t.Fatal("FindByBranch returned nil for an existing branch")
	}
	if s.WorktreePath != "" {
		t.Errorf("synthetic session should have empty WorktreePath, got %q", s.WorktreePath)
	}
	if err := Abandon(s); err != nil {
		t.Fatalf("Abandon synthetic: %v", err)
	}
	if branchExists(root, "session/moe/r1/design") {
		t.Error("branch still present after Abandon")
	}
}
