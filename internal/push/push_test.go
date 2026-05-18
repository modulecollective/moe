package push

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/sandbox"
)

// TestFilterSandboxBindMountsDropsCharDevices pins the bind-mount
// carve-out: a status entry whose on-disk file is a character device
// (the runtime's /dev/null stand-in for shadowed host config files)
// shouldn't gate the push. Real regular files stay in.
//
// Uses a symlink to /dev/null instead of mknod so the test doesn't
// need CAP_MKNOD — os.Stat follows the symlink and reports the device
// mode bits from the target, which is the same shape the filter sees
// in production.
func TestFilterSandboxBindMountsDropsCharDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-to-/dev/null shape not available on windows")
	}
	clone := t.TempDir()

	// Real file — an actual uncommitted agent edit.
	if err := os.WriteFile(filepath.Join(clone, "real.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bind-mount stand-in via a symlink to /dev/null. os.Stat follows
	// the symlink and reports `ModeDevice|ModeCharDevice`, matching
	// what the production runtime's bind-mounted /dev/null entries
	// look like.
	if err := os.Symlink("/dev/null", filepath.Join(clone, ".bashrc")); err != nil {
		t.Skipf("symlink /dev/null: %v", err)
	}

	entries := []git.StatusEntry{
		{XY: "??", Path: "real.go"},
		{XY: "??", Path: ".bashrc"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 1 || got[0].Path != "real.go" {
		t.Fatalf("filterSandboxBindMounts kept %v; want only real.go", got)
	}
}

// TestFilterSandboxBindMountsKeepsMissingEntries: a status entry whose
// file is missing (stat error) stays in the slice. Better to refuse
// the push on an ambiguous record than silently let a deleted-but-
// uncommitted edit through.
func TestFilterSandboxBindMountsKeepsMissingEntries(t *testing.T) {
	clone := t.TempDir()
	entries := []git.StatusEntry{
		{XY: " D", Path: "gone.go"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 1 || got[0].Path != "gone.go" {
		t.Fatalf("filterSandboxBindMounts dropped a missing entry: %v", got)
	}
}

// TestCheckCleanWorkTreeIgnoresMoeDir locks in the loop the original
// incident exposed: harness-private artifacts dropped into
// `<clone>/.moe/` (the dev-env cache being the first one) must not
// gate the push pre-flight. EnsureAt adds `.moe/` to the clone's
// `.git/info/exclude`, so `git status` doesn't even report the file,
// and CheckCleanWorkTree sees a clean tree.
func TestCheckCleanWorkTreeIgnoresMoeDir(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	clone, err := sandbox.Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("sandbox.Ensure: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(clone, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".moe", "dev-env.env"), []byte("FOO=bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CheckCleanWorkTree(clone, "thing"); err != nil {
		t.Fatalf("CheckCleanWorkTree: %v", err)
	}
}

// TestFilterSandboxBindMountsKeepsRegularFiles: the steady-state case.
// Every entry is a normal file; the filter returns the slice unchanged
// so a real uncommitted edit still gates the push.
func TestFilterSandboxBindMountsKeepsRegularFiles(t *testing.T) {
	clone := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(clone, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries := []git.StatusEntry{
		{XY: " M", Path: "a.go"},
		{XY: "??", Path: "b.go"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 2 {
		t.Fatalf("filterSandboxBindMounts dropped a regular file: %v", got)
	}
}
