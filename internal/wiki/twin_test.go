package wiki

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

	cfg := Config{
		ContentDir: twinDir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
			{Filename: "patterns.md", Title: "Patterns"},
			{Filename: "operations.md", Title: "Operations"},
		},
	}
	f, err := Scan(cfg)
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
	got := TwinReferenceSectionAt(root, "p")
	if got != "" {
		t.Errorf("expected empty for missing twin dir, got %q", got)
	}
}

func TestTwinReferenceSectionRendersWithDocs(t *testing.T) {
	root := t.TempDir()
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	got := TwinReferenceSectionAt(root, "p")
	for _, want := range []string{
		"## Project digital twin",
		twinDir,
		"vision.md",
		"architecture.md",
		"patterns.md",
		"operations.md",
		"glossary.md",
		"twin wins until a run updates it",
		"`moe-bureaucracy`",
		"`moe-context`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("twin reference missing %q in:\n%s", want, got)
		}
	}
}
