package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
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

// TestExecuteArgsIncludesAddDirsBeforeSettings pins the variadic-flag
// safety: each AddDirs entry becomes a `--add-dir <dir>` pair, the
// pairs sit before `--settings` (so the JSON payload isn't eaten as
// another directory), and the positional InitialPrompt — when set —
// lands at the very end after `--append-system-prompt`.
func TestExecuteArgsIncludesAddDirsBeforeSettings(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:     "sid-1",
		NewSession:    true,
		Root:          "/bureaucracy",
		AddDirs:       []string{"/tmp/moe-home", "/tmp/moe-devtmp"},
		Prompt:        "system",
		InitialPrompt: "go",
	})
	// First flag is --session-id (NewSession=true), then --add-dir Root,
	// then each AddDirs pair, then --settings, --append-system-prompt,
	// and the positional prompt last.
	want := []string{
		"--session-id", "sid-1",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/tmp/moe-home",
		"--add-dir", "/tmp/moe-devtmp",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
		"go",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteArgsAddsClonePathForCodeStages pins the cwd-inversion
// shape: when ClonePath is set, it lands as a `--add-dir` right after
// the bureaucracy root so the agent (sitting at cwd = root, the
// bureaucracy session worktree) can reach the source-tree workspace.
func TestExecuteArgsAddsClonePathForCodeStages(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:  "sid-1",
		NewSession: true,
		Root:       "/bureaucracy",
		ClonePath:  "/bureaucracy/.moe/clones/widget/req-1",
		Prompt:     "system",
	})
	want := []string{
		"--session-id", "sid-1",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/bureaucracy/.moe/clones/widget/req-1",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteArgsResumeWhenNotNewSession swaps --session-id for
// --resume on a returning session and omits InitialPrompt when empty.
func TestExecuteArgsResumeWhenNotNewSession(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:  "sid-2",
		NewSession: false,
		Root:       "/bureaucracy",
		Prompt:     "system",
	})
	want := []string{
		"--resume", "sid-2",
		"--add-dir", "/bureaucracy",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteOneShotArgsIncludesAddDirsBeforeSettings: the -p path
// must place AddDirs entries before --settings / --append-system-prompt
// and the positional UserPrompt — same variadic-flag rule as the
// interactive path. Otherwise claude eats the JSON settings blob or
// the user prompt as a directory.
func TestExecuteOneShotArgsIncludesAddDirsBeforeSettings(t *testing.T) {
	args := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		AddDirs:    []string{"/tmp/moe-home"},
		Prompt:     "system",
		UserPrompt: "user",
	})
	want := []string{
		"-p",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/tmp/moe-home",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
		"user",
	}
	assertArgsEqual(t, args, want)
}

// assertArgsEqual reports the entire mismatch when args differ — a
// single index error in a 15-element slice is easier to debug as the
// pair of slices than as a single failing field.
func assertArgsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(args) = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
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
