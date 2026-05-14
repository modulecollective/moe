package claude

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

// TestRenameLegacyThreadJSONLMigratesOnFirstTouch covers the one-time
// migration shape: an old thread.jsonl file (pre-multi-agent) sitting
// next to where we're about to write thread-claude.jsonl gets renamed
// in place so old git history rolls forward.
func TestRenameLegacyThreadJSONLMigratesOnFirstTouch(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "thread.jsonl")
	if err := os.WriteFile(legacy, []byte("legacy content"), 0o644); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(dir, "thread-claude.jsonl")

	if err := renameLegacyThreadJSONL(newPath); err != nil {
		t.Fatalf("renameLegacyThreadJSONL: %v", err)
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy thread.jsonl should be gone; stat err=%v", err)
	}
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read renamed thread-claude.jsonl: %v", err)
	}
	if string(got) != "legacy content" {
		t.Fatalf("renamed file content = %q, want legacy content", got)
	}
}

// TestRenameLegacyThreadJSONLDropsLegacyWhenNewAlreadyExists covers
// the second-turn shape: thread-claude.jsonl already exists (the
// turn 1 mirror) AND a thread.jsonl is somehow still hanging around
// (operator-edited working tree, partial migration). The legacy
// file is removed rather than clobbering the up-to-date new file.
func TestRenameLegacyThreadJSONLDropsLegacyWhenNewAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "thread.jsonl")
	newPath := filepath.Join(dir, "thread-claude.jsonl")
	if err := os.WriteFile(legacy, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := renameLegacyThreadJSONL(newPath); err != nil {
		t.Fatalf("renameLegacyThreadJSONL: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy thread.jsonl should be gone; stat err=%v", err)
	}
	if got, _ := os.ReadFile(newPath); string(got) != "fresh" {
		t.Fatalf("new path should be untouched; got %q want fresh", got)
	}
}

// TestRenameLegacyThreadJSONLNoOpWhenLegacyAbsent is the steady-state
// case: most documents have no legacy thread.jsonl, just the new
// per-agent file. The migration helper should silently do nothing.
func TestRenameLegacyThreadJSONLNoOpWhenLegacyAbsent(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "thread-claude.jsonl")
	if err := renameLegacyThreadJSONL(newPath); err != nil {
		t.Fatalf("renameLegacyThreadJSONL: %v", err)
	}
}
