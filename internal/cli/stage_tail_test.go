package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTailHeadlessTranscript_MissingFileIsSoft mirrors the production
// case where claude `-p` exits before writing any per-session JSONL:
// the auto-tail must not surface an error, since the absent file is a
// legitimate "agent died before init" state and the operator already
// sees the executor's exit through other channels.
func TestTailHeadlessTranscript_MissingFileIsSoft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thread-claude.jsonl")
	var w bytes.Buffer
	tailHeadlessTranscript("claude", path, &w)
	if w.Len() != 0 {
		t.Fatalf("expected no output for missing transcript, got %q", w.String())
	}
}

func TestTailHeadlessTranscript_RendersTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thread-claude.jsonl")
	body := strings.Join([]string{
		`{"type":"user","timestamp":"2026-05-16T10:00:00Z","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-05-16T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var w bytes.Buffer
	tailHeadlessTranscript("claude", path, &w)
	got := w.String()
	for _, want := range []string{"last", "transcript events", "user", "assistant", "ok"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---output---\n%s", want, got)
		}
	}
}

// TestTailHeadlessTranscript_BookkeepingOnlyDoesNotPrintBanner avoids
// the "--- last 0 transcript events ---" annoyance when the only
// lines in the file are claude bookkeeping that the adapter drops.
func TestTailHeadlessTranscript_BookkeepingOnlyDoesNotPrintBanner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thread-claude.jsonl")
	body := `{"type":"permission-mode","permissionMode":"auto","sessionId":"x"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var w bytes.Buffer
	tailHeadlessTranscript("claude", path, &w)
	if w.Len() != 0 {
		t.Fatalf("expected empty output for bookkeeping-only transcript, got %q", w.String())
	}
}
