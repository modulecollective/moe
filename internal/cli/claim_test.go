package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// TestBuildClaimSystemPromptSectionsEndWithNewline pins the same
// trailing-newline contract as TestBuildSystemPromptSectionsEndWithNewline,
// but for buildClaimSystemPrompt's three-section join (soul, twin
// reference, claim body). Closed-schema only — claim refuses other
// modes — so the fixture mirrors the closed-schema fixture in
// stage_test.go.
func TestBuildClaimSystemPromptSectionsEndWithNewline(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wikiCfg := &wiki.Config{
		Name:            "twin",
		Mode:            wiki.Closed,
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "what this is."},
		},
	}

	got, err := buildClaimSystemPrompt(wikiCfg)
	if err != nil {
		t.Fatal(err)
	}
	assertPromptSectionsEndWithNewline(t, got, 3)
}

// The canvas-write instruction is the load-bearing prompt change for
// the "claim-seems-broken" fix: the agent must know to write the
// durable per-pass record to the per-run canvas, and the path it sees
// in the prompt is the same path commitClaimTurn stages and
// session.Close reads at the branch tip. Same shape as the reflect
// kickoff test.
func TestClaimKickoffInstructsCanvasWrite(t *testing.T) {
	det := wiki.DetectionResult{UnrecordedDocs: []string{"vision.md"}}
	canvasRel := "projects/tele/runs/claim-2026-05-12-120000/documents/claim/content.md"
	got := claimKickoff(det, "", nil, "", canvasRel)
	for _, want := range []string{
		"per-pass record",
		canvasRel,
		"refuses to seal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, got)
		}
	}
}

// commitClaimTurn must stage both the twin content dir and the
// per-run canvas in the same commit. Without the canvas hunk, the
// session-close gate refuses to fast-forward main — the original bug.
// Mirror of TestCommitReflectTurnStagesCanvas.
func TestCommitClaimTurnStagesCanvas(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n\nupdated.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runSlug := "claim-2026-05-12-120000"
	canvasRel := run.ContentPath("tele", runSlug, "claim")
	canvasPath := filepath.Join(root, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasPath, []byte("Trigger / Decision / Diff.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wikiRel, err := filepath.Rel(root, twinDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := commitClaimTurn(root, "twin", "tele", runSlug, wikiRel); err != nil {
		t.Fatalf("commitClaimTurn: %v", err)
	}

	names := gittest.Output(t, root, "show", "--name-only", "--pretty=", "HEAD")
	for _, want := range []string{
		canvasRel,
		filepath.Join(wikiRel, "vision.md"),
	} {
		if !strings.Contains(names, want) {
			t.Errorf("commit missing %q in:\n%s", want, names)
		}
	}
}

// If the agent skipped the canvas write, commitClaimTurn must still
// land the wiki edits — the close-time gate is what refuses an empty
// canvas. Staging would error if we tried to add a missing path, so
// the helper has to filter it out. Mirror of
// TestCommitReflectTurnTolerateMissingCanvas.
func TestCommitClaimTurnTolerateMissingCanvas(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n\nupdated.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wikiRel, err := filepath.Rel(root, twinDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := commitClaimTurn(root, "twin", "tele", "claim-2026-05-12-120000", wikiRel); err != nil {
		t.Fatalf("commitClaimTurn with missing canvas: %v", err)
	}
}
