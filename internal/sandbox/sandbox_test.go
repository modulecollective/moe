package sandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestClonePlainTree exercises the lowest-level primitive: recursive copy
// of a directory with regular files and a subdirectory. The Darwin build
// uses clonefile; the !darwin build uses the recursive walk. Both must
// produce byte-identical contents.
func TestClonePlainTree(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("a.txt: got=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); err != nil || !bytes.Equal(got, []byte("world")) {
		t.Fatalf("sub/b.txt: got=%q err=%v", got, err)
	}
}

// TestEnsurePlainRepo covers the simpler of the two submodule layouts:
// `projects/<id>/.git` is a real directory, no gitfile. This is what a
// freshly `git init`-ed repo looks like, and what the submodule looks
// like on older git versions.
func TestEnsurePlainRepo(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "code.txt")
	runGit(t, src, "commit", "-m", "v1")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !filepath.IsAbs(clone) {
		t.Fatalf("clone path not absolute: %s", clone)
	}
	if got, err := os.ReadFile(filepath.Join(clone, "code.txt")); err != nil || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("code.txt: got=%q err=%v", got, err)
	}

	// Second call reuses the existing clone.
	clone2, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure (reuse): %v", err)
	}
	if clone2 != clone {
		t.Fatalf("Ensure returned %s then %s", clone, clone2)
	}

	// Writes in the clone must not leak into the source worktree — that's
	// the whole point of the sandbox.
	if err := os.WriteFile(filepath.Join(clone, "code.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(src, "code.txt")); err != nil || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("source worktree contaminated: got=%q err=%v", got, err)
	}
}

// TestEnsureGitfileSubmodule exercises the real submodule layout:
// `projects/<id>/.git` is a gitfile pointing at a sibling directory
// holding the actual git data. Ensure must clone both, fix up the
// gitfile, and reset core.worktree so the clone is a fully independent
// git repo.
func TestEnsureGitfileSubmodule(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Start with a normal repo, then relocate its gitdir to mimic what
	// `git submodule add` produces.
	runGit(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "code.txt")
	runGit(t, src, "commit", "-m", "v1")

	realGitDir := filepath.Join(root, ".git", "modules", "projects", "thing")
	if err := os.MkdirAll(filepath.Dir(realGitDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(src, ".git"), realGitDir); err != nil {
		t.Fatal(err)
	}
	absSrc, err := filepath.Abs(src)
	if err != nil {
		t.Fatal(err)
	}
	// Tell the gitdir where its working tree now lives.
	runGitDir(t, realGitDir, "config", "core.worktree", absSrc)
	rel, err := filepath.Rel(src, realGitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sanity: the submodule-style source works.
	runGit(t, src, "status")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	gitfile, err := os.ReadFile(filepath.Join(clone, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(gitfile, []byte("gitdir: ")) {
		t.Fatalf("expected rewritten gitfile, got %q", gitfile)
	}

	// Verify end-to-end that the clone is a working git repo with its
	// own refs — running git status and a new commit inside the clone
	// must succeed and not touch the source.
	runGit(t, clone, "status")

	if err := os.WriteFile(filepath.Join(clone, "code.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "commit", "-am", "v2")

	srcContent, _ := os.ReadFile(filepath.Join(src, "code.txt"))
	if !bytes.Equal(srcContent, []byte("v1")) {
		t.Fatalf("source worktree contaminated: %q", srcContent)
	}
	out := runGitOut(t, src, "log", "--format=%s")
	if got := string(bytes.TrimSpace(out)); got != "v1" {
		t.Fatalf("source gitdir contaminated; log=%q", got)
	}
	out = runGitOut(t, clone, "log", "--format=%s")
	if !bytes.Contains(out, []byte("v2")) || !bytes.Contains(out, []byte("v1")) {
		t.Fatalf("clone log missing commits: %q", out)
	}
}

// TestRemoveIdempotent exercises the common case where a request is
// signed or scrapped before any clone was ever created (document-only
// work, say). Remove must not error in that situation.
func TestRemoveIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatalf("Remove on missing: %v", err)
	}
}

// TestRemoveAfterEnsure confirms Remove cleans up both the worktree
// clone and the sibling gitdir, and that Exists tracks both states.
func TestRemoveAfterEnsure(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "commit", "--allow-empty", "-m", "init")

	if Exists(root, "thing", "req-a") {
		t.Fatal("Exists true before Ensure")
	}
	if _, err := Ensure(root, "thing", "req-a"); err != nil {
		t.Fatal(err)
	}
	if !Exists(root, "thing", "req-a") {
		t.Fatal("Exists false after Ensure")
	}
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatal(err)
	}
	if Exists(root, "thing", "req-a") {
		t.Fatal("Exists true after Remove")
	}
	// Second Remove is a no-op.
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatalf("Remove idempotent: %v", err)
	}
}

// TestEnsureWritesGitignore confirms the lazy .moe/.gitignore is
// created so clones never accidentally get staged into the bureaucracy.
func TestEnsureWritesGitignore(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "commit", "--allow-empty", "-m", "init")

	if _, err := Ensure(root, "thing", "req-a"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".moe", ".gitignore"))
	if err != nil {
		t.Fatalf("expected .moe/.gitignore: %v", err)
	}
	if string(b) != "*\n" {
		t.Fatalf("unexpected .gitignore content: %q", b)
	}
}

// TestEnsureRejectsMissingSource confirms the common "project not
// registered or submodule not checked out" failure surfaces clearly.
func TestEnsureRejectsMissingSource(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, "ghost", "req-a"); err == nil {
		t.Fatal("expected error for missing source")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Point git at an isolated config so the host's global settings (commit
	// signing, templates, hooks) don't influence fixture commits. Matches
	// the pattern used by bureaucracy_test and project_test.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[user]\n\temail = t@example.com\n\tname = T\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

// runGit runs `git -C dir <args...>` with deterministic author/committer
// env so commits are reproducible. Fails the test on non-zero exit.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
}

// runGitOut is runGit's sibling that returns the command's stdout for
// assertions (log output, etc.).
func runGitOut(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s %v: %v", dir, args, err)
	}
	return out
}

// runGitDir runs git against a gitdir directly (for fixing up the
// relocated gitdir before its gitfile is written).
func runGitDir(t *testing.T, gitDir string, args ...string) {
	t.Helper()
	full := append([]string{"--git-dir", gitDir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git --git-dir=%s %v: %v: %s", gitDir, args, err, out)
	}
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
}
