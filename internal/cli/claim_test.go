package cli

import (
	"os"
	"path/filepath"
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
