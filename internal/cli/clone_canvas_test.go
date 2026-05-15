package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// syncRunIntoClone copies only the agent-writable files into
// <clonePath>/.moe-run/. Pin: the current canvas, followups, and twin
// feedback land in the clone with bytes unchanged.
func TestSyncRunIntoClonePropagatesWritableFiles(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	wantCanvas := "bureaucracy canvas body\n"
	if err := os.WriteFile(canvasAbs, []byte(wantCanvas), 0o644); err != nil {
		t.Fatal(err)
	}

	followups := filepath.Join(workRoot, run.FollowupsPath(md.Project, md.ID))
	wantFollowups := "- [ ] `slug` — title\n"
	if err := os.WriteFile(followups, []byte(wantFollowups), 0o644); err != nil {
		t.Fatal(err)
	}

	feedback := filepath.Join(workRoot, run.FeedbackPath(md.Project, md.ID, "twin"))
	if err := os.MkdirAll(filepath.Dir(feedback), 0o755); err != nil {
		t.Fatal(err)
	}
	wantFeedback := "twin note\n"
	if err := os.WriteFile(feedback, []byte(wantFeedback), 0o644); err != nil {
		t.Fatal(err)
	}

	runJSON := filepath.Join(workRoot, run.Dir(md.Project, md.ID), "run.json")
	if err := os.WriteFile(runJSON, []byte(`{"id":"fix-it","project":"tele"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	design := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, "design"))
	if err := os.MkdirAll(filepath.Dir(design), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(design, []byte("prior design"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncRunIntoClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncRunIntoClone: %v", err)
	}

	base := filepath.Join(clonePath, CloneRunDir)
	cases := map[string]string{
		filepath.Join(base, "documents", docID, "content.md"): wantCanvas,
		filepath.Join(base, "followups.md"):                   wantFollowups,
		filepath.Join(base, "feedback", "twin.md"):            wantFeedback,
	}
	for path, want := range cases {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	for _, path := range []string{
		filepath.Join(base, "run.json"),
		filepath.Join(base, "documents", "design", "content.md"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("non-writable file %s should not be copied, err=%v", path, err)
		}
	}
}

// thread-<agent>.jsonl files are not part of the writable allowlist.
// Pin: the shuttle skips them — copying multi-MB transcripts twice per
// turn would add nothing but I/O.
func TestSyncRunIntoCloneSkipsThreadTranscripts(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	docDir := filepath.Join(workRoot, run.DocDir(md.Project, md.ID, docID))
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docDir, "content.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(docDir, "thread-claude.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"role":"user"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncRunIntoClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncRunIntoClone: %v", err)
	}
	cloned := filepath.Join(clonePath, CloneRunDir, "documents", docID, "thread-claude.jsonl")
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Errorf("expected thread-claude.jsonl skipped in clone, err=%v", err)
	}
}

// First-turn shape: when no bureaucracy run subtree exists yet, pin
// that any stale .moe-run/ in the clone is cleared so the next turn
// starts from a clean slate rather than reading a prior session's
// tail.
func TestSyncRunIntoCloneClearsStaleWhenSourceMissing(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}

	stale := filepath.Join(clonePath, CloneRunDir, "documents", "code")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "content.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncRunIntoClone(workRoot, clonePath, md, "code"); err != nil {
		t.Fatalf("syncRunIntoClone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clonePath, CloneRunDir)); !os.IsNotExist(err) {
		t.Errorf("expected stale .moe-run cleared, err=%v", err)
	}
}

// syncRunIntoClone is a no-op when clonePath is empty — document-only
// stages still go through the same BuildSpec scaffolding but don't
// have a clone to write to. This pins the early-return so a future
// refactor can't accidentally start touching the bureaucracy subtree.
func TestSyncRunIntoCloneEmptyClonePathIsNoOp(t *testing.T) {
	workRoot := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, "design"))
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "untouched"
	if err := os.WriteFile(canvasAbs, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncRunIntoClone(workRoot, "", md, "design"); err != nil {
		t.Fatalf("syncRunIntoClone: %v", err)
	}
	got, err := os.ReadFile(canvasAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("bureaucracy canvas mutated: got %q, want %q", got, want)
	}
}

// syncRunFromClone copies the agent's in-clone writes back to the
// bureaucracy. Overwrite-only: an agent that wrote the in-clone canvas
// wins over whatever is in bureaucracy.
func TestSyncRunFromCloneOverwritesBureaucracy(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	cloneCanvas := filepath.Join(clonePath, CloneRunDir, "documents", docID, "content.md")
	if err := os.MkdirAll(filepath.Dir(cloneCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "agent's actual edit\n"
	if err := os.WriteFile(cloneCanvas, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasAbs, []byte("STALE — should be overwritten"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncRunFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncRunFromClone: %v", err)
	}
	got, err := os.ReadFile(canvasAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("bureaucracy canvas = %q, want %q (clone must win)", got, want)
	}
}

// followups.md and feedback/twin.md ride the same shuttle — pin that
// agents who write either land in bureaucracy after the post-sync
// step.
func TestSyncRunFromClonePropagatesFollowupsAndFeedback(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	canvasClone := filepath.Join(clonePath, CloneRunDir, "documents", docID, "content.md")
	if err := os.MkdirAll(filepath.Dir(canvasClone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasClone, []byte("canvas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	followupsClone := filepath.Join(clonePath, CloneRunDir, "followups.md")
	wantFollowups := "- [ ] `slug` — title\n"
	if err := os.WriteFile(followupsClone, []byte(wantFollowups), 0o644); err != nil {
		t.Fatal(err)
	}
	feedbackClone := filepath.Join(clonePath, CloneRunDir, "feedback", "twin.md")
	if err := os.MkdirAll(filepath.Dir(feedbackClone), 0o755); err != nil {
		t.Fatal(err)
	}
	wantFeedback := "twin note\n"
	if err := os.WriteFile(feedbackClone, []byte(wantFeedback), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncRunFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncRunFromClone: %v", err)
	}

	gotFollowups, err := os.ReadFile(filepath.Join(workRoot, run.FollowupsPath(md.Project, md.ID)))
	if err != nil || string(gotFollowups) != wantFollowups {
		t.Errorf("followups: got %q err=%v, want %q", gotFollowups, err, wantFollowups)
	}
	gotFeedback, err := os.ReadFile(filepath.Join(workRoot, run.FeedbackPath(md.Project, md.ID, "twin")))
	if err != nil || string(gotFeedback) != wantFeedback {
		t.Errorf("feedback/twin: got %q err=%v, want %q", gotFeedback, err, wantFeedback)
	}
}

// run.json is MoE-owned and not on the writable allowlist. Pin: if an
// agent somehow creates an in-clone copy, post-sync ignores it rather
// than letting the change reach bureaucracy.
func TestSyncRunFromCloneIgnoresRunJSON(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	runJSONBureau := filepath.Join(workRoot, run.Dir(md.Project, md.ID), "run.json")
	if err := os.MkdirAll(filepath.Dir(runJSONBureau), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runJSONBureau, []byte(`{"original":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runJSONClone := filepath.Join(clonePath, CloneRunDir, "run.json")
	if err := os.MkdirAll(filepath.Dir(runJSONClone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runJSONClone, []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	canvasClone := filepath.Join(clonePath, CloneRunDir, "documents", docID, "content.md")
	if err := os.MkdirAll(filepath.Dir(canvasClone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasClone, []byte("canvas"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := syncRunFromClone(workRoot, clonePath, md, docID)
	if err != nil {
		t.Fatalf("syncRunFromClone: %v", err)
	}
	// Bureaucracy run.json must be left untouched.
	got, _ := os.ReadFile(runJSONBureau)
	if string(got) != `{"original":true}` {
		t.Errorf("bureaucracy run.json was modified: %s", got)
	}
}

// Prior-stage canvases are not on the writable allowlist. Pin: an
// agent-created documents/design/content.md is ignored, and the
// bureaucracy design canvas stays at its prior contents.
func TestSyncRunFromCloneIgnoresPriorStageCanvasEdit(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	designBureau := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, "design"))
	if err := os.MkdirAll(filepath.Dir(designBureau), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(designBureau, []byte("design original"), 0o644); err != nil {
		t.Fatal(err)
	}

	designClone := filepath.Join(clonePath, CloneRunDir, "documents", "design", "content.md")
	if err := os.MkdirAll(filepath.Dir(designClone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(designClone, []byte("design TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	canvasClone := filepath.Join(clonePath, CloneRunDir, "documents", docID, "content.md")
	if err := os.MkdirAll(filepath.Dir(canvasClone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasClone, []byte("canvas"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := syncRunFromClone(workRoot, clonePath, md, docID)
	if err != nil {
		t.Fatalf("syncRunFromClone: %v", err)
	}
	got, _ := os.ReadFile(designBureau)
	if string(got) != "design original" {
		t.Errorf("bureaucracy design canvas modified: %s", got)
	}
}

// syncRunFromClone with no clone subtree is a no-op so commitTurn's
// own existence gate can fire loudly on "agent did not write" — the
// failure mode we want preserved end-to-end.
func TestSyncRunFromCloneNoSubtreeIsNoOp(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	if err := syncRunFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncRunFromClone: %v", err)
	}
	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if _, err := os.Stat(canvasAbs); !os.IsNotExist(err) {
		t.Errorf("bureaucracy canvas appeared without a clone subtree, err=%v", err)
	}
}

// excludeCloneRun writes .moe-run/ into the clone's local exclude
// file so `git status` stays quiet. Uses .git/info/exclude (the
// common gitdir's, shared across worktrees of the same submodule) so
// the entry doesn't pollute the project's tracked .gitignore.
func TestExcludeCloneRunAddsEntryAndIsIdempotent(t *testing.T) {
	clonePath := t.TempDir()
	gittest.InitAt(t, clonePath)

	if err := excludeCloneRun(clonePath); err != nil {
		t.Fatalf("excludeCloneRun: %v", err)
	}
	excludePath := filepath.Join(clonePath, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	entry := CloneRunDir + "/"
	if !strings.Contains(string(data), entry) {
		t.Errorf("exclude missing %q:\n%s", entry, data)
	}

	// Second call must not duplicate.
	if err := excludeCloneRun(clonePath); err != nil {
		t.Fatalf("excludeCloneRun (rerun): %v", err)
	}
	data2, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data2), entry); count != 1 {
		t.Errorf("exclude has %d entries for %q, want 1:\n%s", count, entry, data2)
	}

	// Tracked .gitignore must not be modified — the local exclude
	// file is the whole reason this helper exists. A future refactor
	// that switches mechanism would land here as a failing test.
	if _, err := os.Stat(filepath.Join(clonePath, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("excludeCloneRun wrote .gitignore (must use .git/info/exclude); err=%v", err)
	}
}
