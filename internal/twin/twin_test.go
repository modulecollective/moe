package twin

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScanManagedDocs(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n\nthe bet\n")
	// architecture.md missing → MissingManagedDocs
	// patterns.md present but stub → EmptyDocs
	writeFile(t, filepath.Join(twinDir, "patterns.md"), "# Patterns\n")
	// operations.md links to a missing sibling → BrokenLinks
	writeFile(t, filepath.Join(twinDir, "operations.md"),
		"# Operations\n\nSee [missing](missing.md) for details.\n")
	writeFile(t, filepath.Join(twinDir, "glossary.md"), "# Glossary\n\nNo entries yet.\n")

	f, err := Scan(twinDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.MissingManagedDocs, []string{"architecture.md"}; !equalStrings(got, want) {
		t.Errorf("MissingManagedDocs: got %v want %v", got, want)
	}
	if got, want := f.EmptyDocs, []string{"patterns.md"}; !equalStrings(got, want) {
		t.Errorf("EmptyDocs: got %v want %v", got, want)
	}
	if len(f.BrokenLinks) != 1 || f.BrokenLinks[0].From != "operations.md" || f.BrokenLinks[0].Target != "missing.md" {
		t.Errorf("BrokenLinks: got %+v", f.BrokenLinks)
	}
}

func TestTwinReferenceSectionEmptyWithoutDir(t *testing.T) {
	root := t.TempDir()
	got := ReferenceSectionAt(root, "p", "claude")
	if got != "" {
		t.Errorf("expected empty for missing twin dir, got %q", got)
	}
}

// A twin dir holding no managed doc renders nothing, rather than a
// header over an empty list.
func TestTwinReferenceSectionEmptyWithoutManagedDocs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "projects", "p", "digital-twin", "notes.md"), "# Notes\n")
	if got := ReferenceSectionAt(root, "p", "claude"); got != "" {
		t.Errorf("expected empty without managed docs, got %q", got)
	}
}

// Each managed doc on disk is listed by absolute path with its line
// count; a missing one is not listed (pointing at it only manufactures
// a not-found error). The read-one-doc-per-call rule is for every
// backend; the result-cap sentence is codex-only.
func TestTwinReferenceSectionRendersWithDocs(t *testing.T) {
	root := t.TempDir()
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n\nthe bet\n")
	writeFile(t, filepath.Join(twinDir, "architecture.md"), "# Architecture\n\nno trailing newline")
	got := ReferenceSectionAt(root, "p", "claude")
	for _, want := range []string{
		"## Project digital twin",
		"- vision.md — what this project is trying to be",
		"(3 lines)\n  " + filepath.Join(twinDir, "vision.md") + "\n",
		"- architecture.md — components",
		"(3 lines)\n  " + filepath.Join(twinDir, "architecture.md") + "\n",
		"Read one doc per tool call",
		"twin wins until a run updates it",
		"`moe-bureaucracy`",
		"`moe-context`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("twin reference missing %q in:\n%s", want, got)
		}
	}
	for _, deny := range []string{"patterns.md", "operations.md", "glossary.md", "truncated output"} {
		if strings.Contains(got, deny) {
			t.Errorf("claude twin reference should not mention %q:\n%s", deny, got)
		}
	}

	codex := ReferenceSectionAt(root, "p", "codex")
	if !strings.Contains(codex, "capped at about 10k tokens") {
		t.Errorf("codex twin reference missing the result-cap sentence:\n%s", codex)
	}
}
