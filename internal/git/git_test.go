package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
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
