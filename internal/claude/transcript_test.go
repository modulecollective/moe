package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeTranscript plants a session JSONL under a fake CLAUDE_CONFIG_DIR,
// mimicking Claude Code's layout: <config>/projects/<encoded-cwd>/<sid>.jsonl.
// Returns the src path so tests can assert copies preserve it byte-for-byte.
func writeFakeTranscript(t *testing.T, configDir, projectDir, sid, body string) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCopyTranscriptCopiesClaudeSessionLog(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0"
	body := `{"type":"user","content":"hi"}` + "\n" + `{"type":"assistant","content":"hi back"}` + "\n"
	writeFakeTranscript(t, cfg, "-home-user-moe", sid, body)

	dest := filepath.Join(t.TempDir(), "docs", "spec", "thread.jsonl")
	found, err := CopyTranscript(sid, dest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true when transcript exists")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("transcript body not preserved; got %q, want %q", got, body)
	}
}

func TestCopyTranscriptGlobsAcrossProjectDirs(t *testing.T) {
	// Same session id can only appear once in Claude Code's store (UUIDs),
	// but the project dir encoding is Claude Code's business. Put the file
	// under an arbitrary dir name to prove we're not reconstructing paths.
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "1c8e2b9f-3441-4d5a-8e23-9d0f7c2b3a14"
	writeFakeTranscript(t, cfg, "some-weird-encoding-we-dont-control", sid, "x\n")

	dest := filepath.Join(t.TempDir(), "thread.jsonl")
	found, err := CopyTranscript(sid, dest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true regardless of project dir encoding")
	}
}

func TestCopyTranscriptAbsentIsNotAnError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dest := filepath.Join(t.TempDir(), "thread.jsonl")
	found, err := CopyTranscript("7d2a5e1c-90b3-4c11-a4d2-2e5b1c0a9f33", dest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false when no transcript exists")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest should not have been created; stat err=%v", err)
	}
}

func TestCopyTranscriptOverwritesExisting(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "5e2b1c0a-9f33-4d2a-5e1c-90b34c11a4d2"
	// Simulate a mid-session copy: initial file, then claude appends,
	// then we copy again. Mirror behavior is "full snapshot each turn."
	writeFakeTranscript(t, cfg, "p", sid, "turn1\n")
	dest := filepath.Join(t.TempDir(), "thread.jsonl")
	if err := os.WriteFile(dest, []byte("stale-previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyTranscript(sid, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "turn1\n" {
		t.Fatalf("expected overwrite; got %q", got)
	}
}

func TestConfigDirRespectsEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	if got := ConfigDir(); got != "/custom/claude" {
		t.Fatalf("ConfigDir = %q, want /custom/claude", got)
	}
}
