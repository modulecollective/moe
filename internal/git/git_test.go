package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestStatus_SpacesAndRename pins down two facts that the project
// relies on: paths with spaces round-trip verbatim (no core-quoting,
// no octal escapes), and `-z` rename records arrive as NEW-then-OLD
// (the reverse of the human `R old -> new` form).
func TestStatus_SpacesAndRename(t *testing.T) {
	dir := newTempRepo(t)

	// Seed a tracked file with a space, then commit so we have a HEAD.
	writeFile(t, filepath.Join(dir, "kept name.txt"), "kept\n")
	gitMust(t, dir, "add", ".")
	gitMust(t, dir, "commit", "-m", "init")

	// Rename it through git so the rename record is staged.
	gitMust(t, dir, "mv", "kept name.txt", "renamed name.txt")
	// Drop an untracked file with a space so the ?? path is exercised too.
	writeFile(t, filepath.Join(dir, "stray two.txt"), "stray\n")

	entries, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	// Order is git's, not ours; sort by Path for a stable assert.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	want := []StatusEntry{
		{XY: "R ", Path: "renamed name.txt", From: "kept name.txt"},
		{XY: "??", Path: "stray two.txt"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
}

// TestStatus_CleanTreeReturnsNil confirms a clean working tree
// produces zero entries (and a nil slice, so callers can use
// len() == 0 as the dirty check).
func TestStatus_CleanTreeReturnsNil(t *testing.T) {
	dir := newTempRepo(t)
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	gitMust(t, dir, "add", ".")
	gitMust(t, dir, "commit", "-m", "init")

	entries, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if entries != nil {
		t.Fatalf("clean tree should return nil entries, got %#v", entries)
	}
}

// TestStatus_PathsScope confirms scoping limits results to the
// requested pathspec.
func TestStatus_PathsScope(t *testing.T) {
	dir := newTempRepo(t)
	gitMust(t, dir, "commit", "--allow-empty", "-m", "init")

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "sub", "in.txt"), "in\n")
	writeFile(t, filepath.Join(dir, "out.txt"), "out\n")

	entries, err := Status(dir, "sub")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "sub/in.txt" {
		t.Fatalf("scoped Status got %#v, want only sub/in.txt", entries)
	}
}

// TestRun_RetriesIndexLockUntilCleared confirms Run waits out a busy
// index.lock and succeeds once another process releases it — the
// steady-state half of the moe/hunk worktree contention fix.
func TestRun_RetriesIndexLockUntilCleared(t *testing.T) {
	dir := newTempRepo(t)
	gitMust(t, dir, "commit", "--allow-empty", "-m", "init")

	// Shrink the retry envelope so the test stays sub-second. The
	// production cap (2s) is policy, not behaviour we want to assert
	// here — we only need to prove the retry/clear/succeed path.
	withIndexLockTiming(t, 500*time.Millisecond, 10*time.Millisecond)

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	// Clear the lock partway through Run's retry loop. 80ms is well
	// inside the 500ms budget but past several 10ms ticks, so we know
	// the success came from a retry (not the first attempt).
	cleared := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.Remove(lock)
		close(cleared)
	}()

	if err := Run(dir, "commit", "--allow-empty", "-m", "after-lock"); err != nil {
		t.Fatalf("Run after lock cleared: %v", err)
	}
	<-cleared
}

// TestRun_IndexLockExhaustsCap confirms Run gives up at the cap and
// surfaces git's verbatim stderr — the operator-facing half of the
// fix, so a genuinely stuck lock from a crashed prior run doesn't
// disappear behind a synthetic error message.
func TestRun_IndexLockExhaustsCap(t *testing.T) {
	dir := newTempRepo(t)
	gitMust(t, dir, "commit", "--allow-empty", "-m", "init")

	withIndexLockTiming(t, 80*time.Millisecond, 10*time.Millisecond)

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	start := time.Now()
	err := Run(dir, "commit", "--allow-empty", "-m", "should-fail")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run with persistent lock should fail")
	}
	if !strings.Contains(err.Error(), indexLockSubstr) {
		t.Fatalf("error should preserve git's verbatim stderr: %v", err)
	}
	// At least one retry tick elapsed: cap (80ms) — proof the loop
	// actually retried before giving up. Upper bound is loose; CI
	// jitter shouldn't cause flakes.
	if elapsed < 80*time.Millisecond {
		t.Fatalf("Run returned in %v, expected to wait out cap", elapsed)
	}
}

// withIndexLockTiming swaps in test-friendly retry bounds and restores
// the production values when the test ends.
func withIndexLockTiming(t *testing.T, cap, step time.Duration) {
	t.Helper()
	prevCap, prevStep := indexLockRetryCap, indexLockRetryStep
	indexLockRetryCap = cap
	indexLockRetryStep = step
	t.Cleanup(func() {
		indexLockRetryCap = prevCap
		indexLockRetryStep = prevStep
	})
}

func newTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitMust(t, dir, "init", "-q")
	gitMust(t, dir, "config", "user.email", "test@example.com")
	gitMust(t, dir, "config", "user.name", "test")
	gitMust(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func gitMust(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
