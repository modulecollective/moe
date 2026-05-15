package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// syncCanvasIntoClone copies the bureaucracy canvas into
// <clonePath>/.moe-canvas.md so codex's apply_patch sees the canvas as
// in-project. The shape this test pins: when the bureaucracy canvas
// exists, its bytes land in the clone unchanged; when it doesn't, any
// stale clone canvas is cleared so the agent reads from a clean slate
// (the failure mode the design calls out — "Pre-turn copy missing
// source on first turn: handle gracefully").
func TestSyncCanvasIntoClonePropagatesAndClears(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	canvasRel := run.ContentPath(md.Project, md.ID, docID)
	canvasAbs := filepath.Join(workRoot, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "bureaucracy canvas body\n"
	if err := os.WriteFile(canvasAbs, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncCanvasIntoClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncCanvasIntoClone: %v", err)
	}
	cloneCanvas := filepath.Join(clonePath, CloneCanvasName)
	got, err := os.ReadFile(cloneCanvas)
	if err != nil {
		t.Fatalf("read clone canvas: %v", err)
	}
	if string(got) != want {
		t.Errorf("clone canvas body = %q, want %q", got, want)
	}

	// Re-run with the bureaucracy canvas removed — the stale clone
	// canvas left over from the previous run must be cleared so the
	// next turn reads from empty.
	if err := os.Remove(canvasAbs); err != nil {
		t.Fatal(err)
	}
	if err := syncCanvasIntoClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncCanvasIntoClone (no source): %v", err)
	}
	if _, err := os.Stat(cloneCanvas); !os.IsNotExist(err) {
		t.Errorf("expected stale clone canvas to be removed, got err=%v", err)
	}
}

// syncCanvasIntoClone is a no-op when clonePath is empty — document-only
// stages still go through the same BuildSpec scaffolding but don't have
// a clone to write to. This pins the early-return so a future refactor
// can't accidentally start touching the bureaucracy canvas itself.
func TestSyncCanvasIntoCloneEmptyClonePathIsNoOp(t *testing.T) {
	workRoot := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	canvasRel := run.ContentPath(md.Project, md.ID, "design")
	canvasAbs := filepath.Join(workRoot, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "untouched"
	if err := os.WriteFile(canvasAbs, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncCanvasIntoClone(workRoot, "", md, "design"); err != nil {
		t.Fatalf("syncCanvasIntoClone: %v", err)
	}
	got, err := os.ReadFile(canvasAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("bureaucracy canvas mutated: got %q, want %q", got, want)
	}
}

// syncCanvasFromClone copies the agent's in-clone canvas back to the
// bureaucracy path before commitTurn's existence gate runs. Clone-wins
// is the contract — if the agent also wrote the bureaucracy canvas
// (e.g. fell back to an absolute bash write), the clone copy
// overwrites it so MoE's convention is enforced not merely advised.
func TestSyncCanvasFromCloneOverwritesBureaucracy(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	cloneCanvas := filepath.Join(clonePath, CloneCanvasName)
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

	if err := syncCanvasFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncCanvasFromClone: %v", err)
	}
	got, err := os.ReadFile(canvasAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("bureaucracy canvas = %q, want %q (clone must win)", got, want)
	}
}

// syncCanvasFromClone with no clone canvas is a no-op so commitTurn's
// own existence gate can fire loudly on "agent did not write" — the
// failure mode we want preserved end-to-end.
func TestSyncCanvasFromCloneNoCloneCanvasIsNoOp(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	if err := syncCanvasFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncCanvasFromClone: %v", err)
	}
	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if _, err := os.Stat(canvasAbs); !os.IsNotExist(err) {
		t.Errorf("bureaucracy canvas appeared without a clone canvas, err=%v", err)
	}
}

// syncCanvasFromClone creates the bureaucracy doc dir if missing — on
// the first code-stage turn against a fresh run, the dir may not exist
// yet (EnsureDocument creates it in the session worktree, but a future
// caller may pass any workRoot). Pin the mkdir-parents behaviour.
func TestSyncCanvasFromCloneCreatesDocDir(t *testing.T) {
	workRoot := t.TempDir()
	clonePath := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	docID := "code"

	cloneCanvas := filepath.Join(clonePath, CloneCanvasName)
	if err := os.WriteFile(cloneCanvas, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncCanvasFromClone(workRoot, clonePath, md, docID); err != nil {
		t.Fatalf("syncCanvasFromClone: %v", err)
	}
	canvasAbs := filepath.Join(workRoot, run.ContentPath(md.Project, md.ID, docID))
	if _, err := os.Stat(canvasAbs); err != nil {
		t.Errorf("bureaucracy canvas not created: %v", err)
	}
}

// excludeCloneCanvas writes .moe-canvas.md into the clone's local
// exclude file so `git status` stays quiet. Uses .git/info/exclude (the
// common gitdir's, shared across worktrees of the same submodule) so
// the entry doesn't pollute the project's tracked .gitignore — which
// would otherwise show up in every PR.
func TestExcludeCloneCanvasAddsEntryAndIsIdempotent(t *testing.T) {
	clonePath := t.TempDir()
	gittest.InitAt(t, clonePath)

	if err := excludeCloneCanvas(clonePath); err != nil {
		t.Fatalf("excludeCloneCanvas: %v", err)
	}
	excludePath := filepath.Join(clonePath, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(data), CloneCanvasName) {
		t.Errorf("exclude missing %q:\n%s", CloneCanvasName, data)
	}

	// Second call must not duplicate.
	if err := excludeCloneCanvas(clonePath); err != nil {
		t.Fatalf("excludeCloneCanvas (rerun): %v", err)
	}
	data2, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data2), CloneCanvasName); count != 1 {
		t.Errorf("exclude has %d entries for %q, want 1:\n%s", count, CloneCanvasName, data2)
	}

	// Tracked .gitignore must not be modified — that's the whole
	// reason we use the local exclude file. A future refactor that
	// switches mechanism would land here as a failing test.
	if _, err := os.Stat(filepath.Join(clonePath, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("excludeCloneCanvas wrote .gitignore (must use .git/info/exclude); err=%v", err)
	}
}
