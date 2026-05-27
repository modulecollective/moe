package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
)

// TestFindOrphanClaudeCacheDirsClassifier is the unit test on the
// orphan rule: directories matching `worktrees-<UUID>` whose UUID is
// absent from the live worktree set are orphans; directories with a
// matching live UUID are kept; non-worktree-shaped directories
// (operator's own claude usage) are ignored entirely.
func TestFindOrphanClaudeCacheDirsClassifier(t *testing.T) {
	projectsDir := t.TempDir()

	liveUUID := "11111111-2222-3333-4444-555555555555"
	deadUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	nonMoEDir := "-Users-someone-some-other-project"

	live := "-home-dev-work-bureaucracy--moe-worktrees-" + liveUUID
	dead := "-home-dev-work-bureaucracy--moe-worktrees-" + deadUUID
	for _, name := range []string{live, dead, nonMoEDir} {
		if err := os.MkdirAll(filepath.Join(projectsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	orphans, err := findOrphanClaudeCacheDirs(projectsDir, map[string]bool{liveUUID: true})
	if err != nil {
		t.Fatalf("findOrphanClaudeCacheDirs: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %v, want one entry", orphans)
	}
	if filepath.Base(orphans[0]) != dead {
		t.Fatalf("orphan basename = %q, want %q", filepath.Base(orphans[0]), dead)
	}
}

// TestFindOrphanClaudeCacheDirsHandlesMultipleWorktreeSegments covers
// the encoded-cwd shapes where the path crosses more than one
// `worktrees-…` segment — the regex must latch onto the UUID
// immediately after the literal `worktrees-` anchor.
func TestFindOrphanClaudeCacheDirsHandlesMultipleWorktreeSegments(t *testing.T) {
	projectsDir := t.TempDir()
	uuid := "deadbeef-cafe-babe-feed-c0ffeec0ffee"
	// Encoded form of `/x/.moe/worktrees/<uuid>/projects/...` — the UUID
	// still sits right after the worktrees- anchor.
	name := "-x--moe-worktrees-" + uuid + "-projects-moe"
	if err := os.MkdirAll(filepath.Join(projectsDir, name), 0o755); err != nil {
		t.Fatal(err)
	}

	orphans, err := findOrphanClaudeCacheDirs(projectsDir, nil)
	if err != nil {
		t.Fatalf("findOrphanClaudeCacheDirs: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %v, want one entry (regex should match nested-worktree paths)", orphans)
	}
}

// TestFindOrphanClaudeCacheDirsMissingProjectsDir covers the
// freshly-installed claude where ~/.claude/projects/ doesn't exist —
// the classifier returns no orphans without erroring, same shape as
// findOrphanClones with no .moe/clones/ dir.
func TestFindOrphanClaudeCacheDirsMissingProjectsDir(t *testing.T) {
	orphans, err := findOrphanClaudeCacheDirs(filepath.Join(t.TempDir(), "never-created"), nil)
	if err != nil {
		t.Fatalf("findOrphanClaudeCacheDirs: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("expected no orphans, got %v", orphans)
	}
}

// TestLiveWorktreeUUIDsExtractsFromGitWorktreeList confirms the live
// UUID set is read out of `git worktree list --porcelain`'s `worktree
// <path>` lines, keyed by basename and filtered to UUID shape so
// non-MoE worktrees (the main checkout, ad-hoc ones) can't false-
// positive the orphan check.
func TestLiveWorktreeUUIDsExtractsFromGitWorktreeList(t *testing.T) {
	root := t.TempDir()
	if _, err := git.Combined(root, "init", "--initial-branch=main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := git.Combined(root, "config", "user.email", "test@example.com"); err != nil {
		t.Fatalf("config email: %v", err)
	}
	if _, err := git.Combined(root, "config", "user.name", "Test"); err != nil {
		t.Fatalf("config name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Combined(root, "add", "seed.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Combined(root, "commit", "-m", "seed"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	// Add a worktree at .moe/worktrees/<uuid>/. UUIDs are arbitrary
	// strings to git; the classifier only cares about basename shape.
	uuid := "12345678-1234-5678-1234-567812345678"
	wtPath := filepath.Join(root, ".moe", "worktrees", uuid)
	if _, err := git.Combined(root, "worktree", "add", "--detach", wtPath); err != nil {
		t.Fatalf("worktree add: %v", err)
	}

	live, err := liveWorktreeUUIDs(root)
	if err != nil {
		t.Fatalf("liveWorktreeUUIDs: %v", err)
	}
	if !live[uuid] {
		t.Fatalf("uuid %q missing from live set: %v", uuid, live)
	}
	// The main checkout's basename is the TempDir name (not UUID-
	// shaped), so it must NOT appear in the live set.
	if live[filepath.Base(root)] {
		t.Fatalf("non-UUID basename leaked into live set: %v", live)
	}
}

// TestClaudeCacheGCRemovesOrphansEndToEnd is the verb-level happy path:
// it discovers the dead-UUID dir, removes it, leaves the live-UUID dir
// and the non-MoE dir alone, prints one removal line, and exits 0.
func TestClaudeCacheGCRemovesOrphansEndToEnd(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Point claude config at a tempdir we control so the test never
	// touches the operator's actual ~/.claude.
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	projectsDir := filepath.Join(cfg, "projects")

	liveUUID := "11111111-2222-3333-4444-555555555555"
	deadUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// Register a worktree at the live UUID path so `git worktree list`
	// returns it. Reuse the bureaucracy test repo as the source.
	liveWT := filepath.Join(root, ".moe", "worktrees", liveUUID)
	if _, err := git.Combined(root, "worktree", "add", "--detach", liveWT); err != nil {
		t.Fatalf("worktree add live: %v", err)
	}

	live := "-home-dev-work-bureaucracy--moe-worktrees-" + liveUUID
	dead := "-home-dev-work-bureaucracy--moe-worktrees-" + deadUUID
	nonMoE := "-Users-someone-project"
	for _, name := range []string{live, dead, nonMoE} {
		if err := os.MkdirAll(filepath.Join(projectsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		// Drop a sentinel file so we can prove the contents went too.
		if err := os.WriteFile(filepath.Join(projectsDir, name, "sentinel.jsonl"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"claude-cache", "gc"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "removed "+dead) {
		t.Fatalf("output missing removal line for %q:\n%s", dead, out.String())
	}
	if _, err := os.Stat(filepath.Join(projectsDir, dead)); !os.IsNotExist(err) {
		t.Fatalf("dead-UUID dir should be gone; stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(projectsDir, live)); err != nil {
		t.Fatalf("live-UUID dir removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectsDir, nonMoE)); err != nil {
		t.Fatalf("non-MoE dir removed unexpectedly: %v", err)
	}
}

// TestClaudeCacheGCNoOrphans is the "nothing to do" path — verb prints
// the status line and exits 0 (so a cron'd `claude-cache gc` doesn't
// surface a false failure on a clean box).
func TestClaudeCacheGCNoOrphans(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	var out, errb bytes.Buffer
	code := Run([]string{"claude-cache", "gc"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "no orphan dirs") {
		t.Fatalf("expected 'no orphan dirs', got %q", out.String())
	}
}
