package sandbox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestEnsurePlainRepo covers the simpler of the two source layouts:
// `projects/<id>/.git` is a real directory, no gitfile. Equivalent to
// what a freshly `git init`-ed repo looks like.
func TestEnsurePlainRepo(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "add", "code.txt")
	gittest.Run(t, src, "commit", "-m", "v1")

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

	// The clone has its own .git/ directory (plain-clone primitive),
	// not a worktree gitfile into the canonical's gitdir.
	if !isPlainCloneAt(t, clone) {
		t.Fatalf("expected plain clone at %s, found gitfile", clone)
	}

	// Second call short-circuits to the same path.
	clone2, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure (reuse): %v", err)
	}
	if clone2 != clone {
		t.Fatalf("Ensure returned %s then %s", clone, clone2)
	}

	// Writes in the clone must not leak into the source's working
	// tree on disk — that's the whole point of the sandbox.
	if err := os.WriteFile(filepath.Join(clone, "code.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(src, "code.txt")); err != nil || !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("source worktree contaminated: got=%q err=%v", got, err)
	}
}

// TestEnsureGitfileSubmodule exercises the real submodule layout:
// `projects/<id>/.git` is a gitfile pointing at a sibling directory
// holding the actual git data. Under the plain-clone primitive, git
// clone follows the gitfile to the canonical gitdir, clones into a
// fresh `.git/` directory at the destination, and the result is a
// standalone repo whose only link back to the canonical is the
// `objects/info/alternates` shared-object reference.
func TestEnsureGitfileSubmodule(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "add", "code.txt")
	gittest.Run(t, src, "commit", "-m", "v1")

	realGitDir := filepath.Join(root, ".git", "modules", "projects", "thing", "src")
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
	gittest.Run(t, "", "--git-dir", realGitDir, "config", "core.worktree", absSrc)
	rel, err := filepath.Rel(src, realGitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "status")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The clone's .git is a real directory, not a gitfile pointing
	// into the canonical's .git/modules/.../worktrees/. This is the
	// load-bearing invariant for the cwd-inversion change: a plain
	// `.git/` makes codex's apply_patch see the clone as a project
	// of its own, not a worktree of the bureaucracy.
	if !isPlainCloneAt(t, clone) {
		t.Fatalf("expected plain clone .git dir at %s", clone)
	}

	gittest.Run(t, clone, "status")

	// Commit in the clone on its own main: the source working tree on
	// disk stays at v1, and main in the canonical gitdir is not
	// advanced (the new commit lives in the clone's ref-db only).
	if err := os.WriteFile(filepath.Join(clone, "code.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, clone, "commit", "-am", "v2")

	srcContent, _ := os.ReadFile(filepath.Join(src, "code.txt"))
	if !bytes.Equal(srcContent, []byte("v1")) {
		t.Fatalf("source worktree contaminated: %q", srcContent)
	}
	out := gittest.Output(t, src, "log", "--format=%s")
	if out != "v1" {
		t.Fatalf("source main advanced; log=%q", out)
	}
	out = gittest.Output(t, clone, "log", "--format=%s")
	if !strings.Contains(out, "v2") || !strings.Contains(out, "v1") {
		t.Fatalf("clone log missing commits: %q", out)
	}
}

// TestRemoveIdempotent: a request scrapped before code stage never
// produced a sandbox; Remove must still succeed.
func TestRemoveIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatalf("Remove on missing: %v", err)
	}
}

// TestRemoveAfterEnsure confirms Remove tears the clone down and that
// Exists tracks both states.
func TestRemoveAfterEnsure(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	if Exists(root, "thing", "req-a") {
		t.Fatal("Exists true before Ensure")
	}
	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatal(err)
	}
	if !Exists(root, "thing", "req-a") {
		t.Fatal("Exists false after Ensure")
	}
	if !isPlainCloneAt(t, clone) {
		t.Fatal("clone not present after Ensure")
	}
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatal(err)
	}
	if Exists(root, "thing", "req-a") {
		t.Fatal("Exists true after Remove")
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Fatalf("clone dir still present after Remove: %v", err)
	}
	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatalf("Remove idempotent: %v", err)
	}
}

// TestRemoveWithDirtyClone confirms Remove tears down a clone with
// uncommitted edits without complaint — by the time Remove runs the
// run is terminal, so any uncommitted state is intentionally being
// discarded.
func TestRemoveWithDirtyClone(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "add", "code.txt")
	gittest.Run(t, src, "commit", "-m", "v1")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "code.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "untracked.txt"), []byte("oops"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(root, "thing", "req-a"); err != nil {
		t.Fatalf("Remove on dirty clone: %v", err)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Fatalf("expected clone gone, stat err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(src, "code.txt")); string(got) != "v1" {
		t.Fatalf("source code.txt = %q, want v1", got)
	}
}

// TestEnsureWritesGitignore confirms the lazy .moe/.gitignore is
// created so clones never accidentally get staged into the
// bureaucracy.
func TestEnsureWritesGitignore(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

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

// TestEnsureRejectsMissingSource: a project that's not registered or
// whose submodule mountpoint doesn't even exist should surface a clear
// error rather than crash with a stat.
func TestEnsureRejectsMissingSource(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, "ghost", "req-a"); err == nil {
		t.Fatal("expected error for missing source")
	}
}

// TestEnsureAutoInit covers the Linux-cloud-box foot-gun: the
// bureaucracy was freshly cloned, .gitmodules declares a submodule,
// but the submodule has never been initialised on this machine. The
// sandbox primitive must materialise it before cloning rather than
// failing with a low-level stat error.
func TestEnsureAutoInit(t *testing.T) {
	gittest.SetupEnv(t)
	tmp := t.TempDir()

	upstream := filepath.Join(tmp, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, upstream, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstream, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, upstream, "add", "code.txt")
	gittest.Run(t, upstream, "commit", "-m", "v1")

	root := filepath.Join(tmp, "bureaucracy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-b", "main")
	// `submodule add` from a local path needs file-protocol consent
	// on git ≥ 2.38.1.
	gittest.Run(t, root, "-c", "protocol.file.allow=always", "submodule", "add", upstream, "projects/thing/src")
	gittest.Run(t, root, "commit", "-m", "add submodule")
	gittest.Run(t, root, "submodule", "deinit", "--force", "projects/thing/src")

	src := filepath.Join(root, "projects", "thing", "src")
	if entries, _ := os.ReadDir(src); len(entries) != 0 {
		t.Fatalf("expected empty mountpoint, got %d entries", len(entries))
	}

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(clone, "code.txt")); err != nil || string(got) != "v1" {
		t.Fatalf("clone code.txt: got=%q err=%v", got, err)
	}
	if entries, _ := os.ReadDir(src); len(entries) == 0 {
		t.Fatalf("auto-init didn't populate %s", src)
	}
}

// TestEnsureAutoInitFailureSurfacesTypedError covers the manual-
// fallback path: when `git submodule update --init` fails (here, a
// bogus URL), Ensure returns *SubmoduleInitError with a message that
// names the verbatim retry command.
func TestEnsureAutoInitFailureSurfacesTypedError(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-b", "main")

	// .gitmodules declares a submodule whose URL doesn't resolve; the
	// mountpoint is an empty dir so the auto-init pre-flight fires.
	if err := os.MkdirAll(filepath.Join(root, "projects", "thing", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	gm := "[submodule \"projects/thing/src\"]\n" +
		"\tpath = projects/thing/src\n" +
		"\turl = file:///definitely-does-not-exist-xyz\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(gm), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Ensure(root, "thing", "req-a")
	if err == nil {
		t.Fatal("Ensure should fail when auto-init fails")
	}
	var sie *SubmoduleInitError
	if !errors.As(err, &sie) {
		t.Fatalf("expected *SubmoduleInitError, got %T: %v", err, err)
	}
	if sie.ProjectID != "thing" {
		t.Errorf("ProjectID = %q, want thing", sie.ProjectID)
	}
	if !strings.Contains(err.Error(), "git -c protocol.file.allow=always submodule update --init --recursive projects/thing/src") {
		t.Errorf("error should name the retry command: %v", err)
	}
}

// TestEnsureWritesCloneExclude pins the exclude reconciliation: a
// fresh clone produced by EnsureAt has `.moe/` in
// `.git/info/exclude`, so harness-private artifacts dropped into
// `<clone>/.moe/` (the dev-env cache, etc.) don't show up as
// untracked and don't gate the push pre-flight.
func TestEnsureWritesCloneExclude(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(clone, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !containsExcludeLine(string(b), ".moe/") {
		t.Fatalf("exclude missing .moe/ line:\n%s", b)
	}
}

// TestEnsureCloneExcludeIdempotent confirms a second EnsureAt against
// the same clone doesn't duplicate the `.moe/` line — the helper has
// to be safe to run on every stage open.
func TestEnsureCloneExcludeIdempotent(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := Ensure(root, "thing", "req-a"); err != nil {
		t.Fatalf("Ensure (reuse): %v", err)
	}
	b, err := os.ReadFile(filepath.Join(clone, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if got := countExcludeLine(string(b), ".moe/"); got != 1 {
		t.Fatalf("exclude has %d .moe/ lines, want 1:\n%s", got, b)
	}
}

// TestEnsureCloneExcludeAppendsToHandEdited covers the backfill path:
// EnsureAt against a pre-existing clone whose `.git/info/exclude` was
// hand-edited with unrelated patterns appends `.moe/` without touching
// those lines.
func TestEnsureCloneExcludeAppendsToHandEdited(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	clone, err := Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Overwrite the exclude with unrelated hand-edited content (no
	// trailing newline, exercising the line-ending fix-up).
	hand := "# operator notes\nlocal-scratch/\n*.tmp"
	excludePath := filepath.Join(clone, ".git", "info", "exclude")
	if err := os.WriteFile(excludePath, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-EnsureAt should backfill `.moe/`.
	if _, err := Ensure(root, "thing", "req-a"); err != nil {
		t.Fatalf("Ensure (backfill): %v", err)
	}
	b, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "# operator notes") || !strings.Contains(got, "local-scratch/") || !strings.Contains(got, "*.tmp") {
		t.Fatalf("backfill clobbered hand-edited lines:\n%s", got)
	}
	if !containsExcludeLine(got, ".moe/") {
		t.Fatalf("backfill didn't add .moe/ line:\n%s", got)
	}
}

// containsExcludeLine reports whether content has `pattern` as a
// whole exclude line (ignoring leading/trailing whitespace).
func containsExcludeLine(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

// countExcludeLine returns the number of times `pattern` appears as a
// whole exclude line. Used by the idempotency assertion.
func countExcludeLine(content, pattern string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			n++
		}
	}
	return n
}

// isPlainCloneAt reports whether the clone at path has a real `.git/`
// directory rather than a worktree gitfile. The load-bearing invariant
// for the plain-clone primitive — a worktree gitfile is what made
// codex's apply_patch refuse cross-boundary writes.
func isPlainCloneAt(t *testing.T, clone string) bool {
	t.Helper()
	info, err := os.Stat(filepath.Join(clone, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}
