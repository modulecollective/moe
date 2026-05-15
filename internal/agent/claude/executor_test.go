package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
)

// TestSandboxSettingsJSONLiteral pins the exact `--settings` payload
// claude is launched with. Operator settings.json layouts depend on
// this exact key/value to merge correctly, so any drift is loud.
func TestSandboxSettingsJSONLiteral(t *testing.T) {
	want := `{"sandbox":{"enabled":true}}`
	if sandboxSettingsJSON != want {
		t.Fatalf("sandboxSettingsJSON = %q, want %q", sandboxSettingsJSON, want)
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
