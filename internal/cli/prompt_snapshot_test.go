package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestWritePromptSnapshotPersistsPayload pins that the assembled
// prompt lands at the per-agent prompt path alongside the canvas
// and thread JSONL — that's the operator's debug surface for "what
// did the agent actually see this turn." commitTurn stages the docDir
// wholesale, so writing the file to its natural path is the entire
// wiring needed.
func TestWritePromptSnapshotPersistsPayload(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	payload := "## system prompt sentinel\n\nbody here\n"

	if err := writePromptSnapshot(root, "claude", md, "design", payload); err != nil {
		t.Fatalf("writePromptSnapshot: %v", err)
	}

	rel := run.PromptPathFor("claude", md.Project, md.ID, "design")
	got, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != payload {
		t.Errorf("snapshot mismatch:\nwant: %q\ngot:  %q", payload, got)
	}

	// Path lands inside docDir so commitTurn picks it up alongside
	// content.md and thread-<agent>.jsonl without extra wiring.
	wantDir := filepath.Join(root, run.DocDir(md.Project, md.ID, "design"))
	if !strings.HasPrefix(filepath.Join(root, rel), wantDir+string(filepath.Separator)) {
		t.Errorf("snapshot path %q not under docDir %q", filepath.Join(root, rel), wantDir)
	}
}

// TestWritePromptSnapshotIsPerAgent pins that two agents on the same
// document produce two distinct files — same shape as the
// thread-<agent>.jsonl mirror. Without this the second agent's
// snapshot would clobber the first.
func TestWritePromptSnapshotIsPerAgent(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	if err := writePromptSnapshot(root, "claude", md, "design", "claude payload"); err != nil {
		t.Fatal(err)
	}
	if err := writePromptSnapshot(root, "codex", md, "design", "codex payload"); err != nil {
		t.Fatal(err)
	}

	claudePath := filepath.Join(root, run.PromptPathFor("claude", md.Project, md.ID, "design"))
	codexPath := filepath.Join(root, run.PromptPathFor("codex", md.Project, md.ID, "design"))
	if claudePath == codexPath {
		t.Fatalf("per-agent paths collide: %s", claudePath)
	}
	c, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	x, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(c) != "claude payload" || string(x) != "codex payload" {
		t.Errorf("per-agent payloads got crossed: claude=%q codex=%q", c, x)
	}
}

// TestWritePromptSnapshotOverwrites pins per-turn overwrite. The
// design recommendation is "git history is the per-turn record; the
// file on disk is the latest." A regression that appends instead of
// overwrites would grow the file unboundedly across turns.
func TestWritePromptSnapshotOverwrites(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}

	if err := writePromptSnapshot(root, "claude", md, "design", "turn 1"); err != nil {
		t.Fatal(err)
	}
	if err := writePromptSnapshot(root, "claude", md, "design", "turn 2"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, run.PromptPathFor("claude", md.Project, md.ID, "design")))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "turn 2" {
		t.Errorf("snapshot should overwrite each turn; got %q", body)
	}
}
