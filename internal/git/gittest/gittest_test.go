package gittest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInit_HasIsolatedConfigAndIdentity confirms Init lands a usable
// repo with user identity set and the global config isolated — the
// two facts every downstream test depends on.
func TestInit_HasIsolatedConfigAndIdentity(t *testing.T) {
	dir := Init(t)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git missing in %s: %v", dir, err)
	}
	if got := Output(t, dir, "config", "user.email"); got != "test@example.com" {
		t.Fatalf("user.email = %q, want test@example.com", got)
	}
	if got := Output(t, dir, "config", "user.name"); got != "test" {
		t.Fatalf("user.name = %q, want test", got)
	}
	if got := Output(t, dir, "config", "commit.gpgsign"); got != "false" {
		t.Fatalf("commit.gpgsign = %q, want false", got)
	}
	if got := os.Getenv("GIT_CONFIG_GLOBAL"); !strings.HasSuffix(got, "gitconfig") {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want a throwaway path", got)
	}
}

// TestInit_IdentityCoversSecondaryRepos confirms a second repo
// initialized via raw Run (not Init) inherits the same identity — the
// donor/origin/seed pattern every fixture relies on.
func TestInit_IdentityCoversSecondaryRepos(t *testing.T) {
	Init(t) // first call installs the GIT_CONFIG_GLOBAL identity.

	second := filepath.Join(t.TempDir(), "secondary")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	Run(t, second, "init", "-q")
	// If the identity hadn't propagated, this commit would fail with
	// `unable to auto-detect email address`.
	Run(t, second, "commit", "--allow-empty", "-m", "secondary")
}

// TestInitSeeded_HasHeadOnMainAndCleanTree confirms the two-call seed
// lands the shape Init+Commit did: one root commit on main, HEAD
// resolvable, nothing staged or untracked.
func TestInitSeeded_HasHeadOnMainAndCleanTree(t *testing.T) {
	dir := InitSeeded(t)

	HeadSHA(t, dir)
	if got := Output(t, dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("branch = %q, want main", got)
	}
	if got := Output(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("commit count = %q, want 1", got)
	}
	if got := Output(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("status --porcelain = %q, want empty", got)
	}
}

// TestInitAt_UsesGivenDir confirms InitAt initializes the repo at the
// caller-provided path rather than a fresh TempDir.
func TestInitAt_UsesGivenDir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested", "repo")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	InitAt(t, sub)

	if _, err := os.Stat(filepath.Join(sub, ".git")); err != nil {
		t.Fatalf(".git missing in %s: %v", sub, err)
	}
}

// TestInitBare_IsBare confirms InitBare produces a bare repo (no
// working tree, HEAD lives at the root).
func TestInitBare_IsBare(t *testing.T) {
	bare := InitBare(t)

	if got := Output(t, bare, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("is-bare-repository = %q, want true", got)
	}
}

// TestCommit_EmptyAllowedAndReturnsSHA confirms Commit allows empty
// commits (so a fresh repo can be seeded with a HEAD in one line) and
// the returned SHA matches HEAD.
func TestCommit_EmptyAllowedAndReturnsSHA(t *testing.T) {
	dir := Init(t)

	sha := Commit(t, dir, "init")

	if got := HeadSHA(t, dir); got != sha {
		t.Fatalf("Commit returned %q, HeadSHA = %q", sha, got)
	}
	if len(sha) != 40 {
		t.Fatalf("SHA %q does not look like a full hash", sha)
	}
}

// TestWriteAndCommit_StagesAndCommits confirms WriteAndCommit writes
// the file, stages it, commits it, and leaves the tree clean.
func TestWriteAndCommit_StagesAndCommits(t *testing.T) {
	dir := Init(t)
	Commit(t, dir, "init")

	sha := WriteAndCommit(t, dir, "sub/file.txt", "hello\n", "add file")

	got, err := os.ReadFile(filepath.Join(dir, "sub", "file.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("file content = %q, want hello\\n", got)
	}
	if Output(t, dir, "status", "--porcelain") != "" {
		t.Fatal("tree should be clean after WriteAndCommit")
	}
	if HeadSHA(t, dir) != sha {
		t.Fatal("HEAD should match the returned SHA")
	}
}

// TestSetupEnv_AppliesIsolationWithoutRepo confirms SetupEnv installs
// the same GIT_CONFIG_GLOBAL identity Init does without creating a
// repo — the entry point sandbox/workspace tests use when they lay
// out their own gitdir tree.
func TestSetupEnv_AppliesIsolationWithoutRepo(t *testing.T) {
	SetupEnv(t)

	dir := t.TempDir()
	// A bare `git init` here would fail to commit without an identity if
	// SetupEnv hadn't seeded GIT_CONFIG_GLOBAL.
	Run(t, dir, "init", "-q")
	Run(t, dir, "commit", "--allow-empty", "-m", "ok")
}

// TestIsolateConfig_DisablesBackgroundWork confirms the seeded global
// gitconfig suppresses every git background-work path that has been
// known to spawn a detached grandchild after a foreground git CLI
// exits. Future tidy-up PRs that thin the config block must keep CI
// green; this test exists to make a regression here unmissable.
//
// The failure mode this prevents — `unlinkat .git/objects: directory
// not empty` during `testing.TempDir` cleanup — is rare enough to
// reflake intermittently on CI without anyone noticing the gitconfig
// changed, so a behavioural test would be slow and unreliable. A
// config-presence assertion is cheap and exact.
func TestIsolateConfig_DisablesBackgroundWork(t *testing.T) {
	dir := Init(t)

	for _, tc := range []struct {
		key, want string
	}{
		{"gc.auto", "0"},
		{"gc.autoDetach", "false"},
		{"gc.writeCommitGraph", "false"},
		{"maintenance.auto", "false"},
		{"maintenance.strategy", "none"},
		{"fetch.writeCommitGraph", "false"},
		{"core.fsmonitor", "false"},
		{"core.commitGraph", "false"},
	} {
		if got := Output(t, dir, "config", "--get", tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestOutput_TrimsTrailingNewline confirms Output strips git's trailing
// newline so callers can compare against literals without TrimSpace.
func TestOutput_TrimsTrailingNewline(t *testing.T) {
	dir := Init(t)
	Commit(t, dir, "init")

	got := Output(t, dir, "log", "-1", "--format=%s")
	if got != "init" {
		t.Fatalf("Output = %q, want %q (no trailing whitespace)", got, "init")
	}
}

func TestInitSeededUsingEnv_PreservesDefaultsAndCleansUp(t *testing.T) {
	// A hostile inherited config must not override the caller's isolation.
	hostile := filepath.Join(t.TempDir(), "hostile.gitconfig")
	if err := os.WriteFile(hostile, []byte("[commit]\n\tgpgsign = true\n[init]\n\tdefaultBranch = hostile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", hostile)
	t.Setenv("GIT_CONFIG_SYSTEM", hostile)
	SetupEnv(t)
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	t.Run("seed", func(t *testing.T) {
		dir = InitSeededUsingEnv(t)
		if got := Output(t, dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
			t.Fatalf("branch = %q, want main", got)
		}
		if got := Output(t, dir, "log", "-1", "--format=%an <%ae>"); got != "test <test@example.com>" {
			t.Fatalf("identity = %q", got)
		}
		if got := Output(t, dir, "status", "--porcelain"); got != "" {
			t.Fatalf("dirty seeded repo: %s", got)
		}
		if got := os.Getenv("GIT_CONFIG_GLOBAL"); got != cfg {
			t.Fatalf("helper changed global config from %q to %q", cfg, got)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fixture survived cleanup: %v", err)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("helper modified caller's config")
	}
}

func TestWriteConfig_ReportsWriteFailure(t *testing.T) {
	if err := WriteConfig(filepath.Join(t.TempDir(), "missing", "gitconfig")); err == nil {
		t.Fatal("writing into a missing directory succeeded")
	}
}
