package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSandboxSettingsJSONBare covers the document-only / headless path:
// no clone, no widening, just the toggle. Locks the literal payload so
// any drift is loud — operator settings.json layouts depend on this
// exact key/value to merge correctly.
func TestSandboxSettingsJSONBare(t *testing.T) {
	got := sandboxSettingsJSON("")
	want := `{"sandbox":{"enabled":true}}`
	if got != want {
		t.Fatalf("bare sandbox JSON = %q, want %q", got, want)
	}
}

// TestSandboxSettingsJSONWidensToWorktreeGitdir covers the code-stage
// path: clone present, .git gitfile points at the worktree's gitdir,
// the emitted JSON grants write to that exact path. Without the
// widening the first index-mutating git command in every code turn
// hits "Read-only file system" because the gitdir lives outside the
// default cwd-only writable scope.
func TestSandboxSettingsJSONWidensToWorktreeGitdir(t *testing.T) {
	clone := t.TempDir()
	gitdir := filepath.Join(t.TempDir(), "modules", "projects", "moe", "src", "worktrees", "demo")
	writeGitfile(t, clone, gitdir)

	got := sandboxSettingsJSON(clone)

	var parsed struct {
		Sandbox struct {
			Enabled    bool `json:"enabled"`
			Filesystem struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted JSON does not parse: %v\nraw: %s", err, got)
	}
	if !parsed.Sandbox.Enabled {
		t.Fatalf("sandbox.enabled = false, want true; raw: %s", got)
	}
	if len(parsed.Sandbox.Filesystem.AllowWrite) != 1 || parsed.Sandbox.Filesystem.AllowWrite[0] != gitdir {
		t.Fatalf("allowWrite = %v, want [%s]; raw: %s",
			parsed.Sandbox.Filesystem.AllowWrite, gitdir, got)
	}
}

// TestSandboxSettingsJSONFallsBackOnMissingGitfile covers the
// degenerate case (clone path with no .git gitfile, or one shaped
// like a plain directory): we fall back to the bare payload rather
// than crashing the turn. Better to keep the historical
// first-attempt-failure behavior than to bomb a turn over a missing
// optimisation.
func TestSandboxSettingsJSONFallsBackOnMissingGitfile(t *testing.T) {
	got := sandboxSettingsJSON(t.TempDir())
	want := `{"sandbox":{"enabled":true}}`
	if got != want {
		t.Fatalf("missing-gitfile sandbox JSON = %q, want %q", got, want)
	}
}

func writeGitfile(t *testing.T, clonePath, gitdir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clonePath, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
}
