package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
