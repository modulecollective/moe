package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/wiki"
)

// TestBuildLintSystemPromptSectionsEndWithNewline pins the same
// trailing-newline contract as TestBuildSystemPromptSectionsEndWithNewline,
// but for buildLintSystemPrompt's three-section join (soul, twin
// reference, lint body). Lint is open-schema only — LintPromptSection
// panics on Closed — so the fixture pairs an open wiki config with a
// digital-twin/<project>/ dir on disk so TwinReferenceSection still
// fires.
func TestBuildLintSystemPromptSectionsEndWithNewline(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	kbDir := filepath.Join(root, "projects", "tele", "kb")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	wikiCfg := &wiki.Config{
		Name:            "kb",
		Mode:            wiki.Open,
		ContentDir:      kbDir,
		Project:         "tele",
		BureaucracyPath: root,
	}

	got, err := buildLintSystemPrompt(wikiCfg)
	if err != nil {
		t.Fatal(err)
	}
	assertPromptSectionsEndWithNewline(t, got, 3)
}
