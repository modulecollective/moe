package wiki

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoreReferenceSectionEmptyWithoutDir(t *testing.T) {
	root := t.TempDir()
	if got := LoreReferenceSectionAt(root); got != "" {
		t.Errorf("expected empty for missing lore dir, got %q", got)
	}
}

func TestLoreReferenceSectionEmptyDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", ".gitkeep"), "")
	if got := LoreReferenceSectionAt(root); got != "" {
		t.Errorf("expected empty for lore dir with no .md entries, got %q", got)
	}
}

func TestLoreReferenceSectionRendersEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", "compose-tailscale.md"),
		"---\ntitle: Reaching compose ports\napplies-when: fly + compose + tailscale\n---\n\nbody\n")
	writeFile(t, filepath.Join(root, "lore", "codex-sandbox.md"),
		"---\ntitle: Interactive codex blocks .git/\napplies-when: codex under a submodule\n---\n\nbody\n")
	got := LoreReferenceSectionAt(root)
	for _, want := range []string{
		"## Lore (cross-project)",
		filepath.Join(root, "lore"),
		"Reaching compose ports",
		"fly + compose + tailscale",
		"Interactive codex blocks .git/",
		"codex under a submodule",
		"`moe-bureaucracy`",
		"`moe-context`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lore reference missing %q in:\n%s", want, got)
		}
	}
}

// Entries are rendered in stable filename order so the section is
// deterministic across runs (and across test invocations on different
// filesystems whose ReadDir ordering may vary).
func TestLoreReferenceSectionOrderingByFilename(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", "zeta.md"),
		"---\ntitle: Zeta\napplies-when: z\n---\n")
	writeFile(t, filepath.Join(root, "lore", "alpha.md"),
		"---\ntitle: Alpha\napplies-when: a\n---\n")
	writeFile(t, filepath.Join(root, "lore", "mid.md"),
		"---\ntitle: Mid\napplies-when: m\n---\n")
	got := LoreReferenceSectionAt(root)
	ai := strings.Index(got, "alpha.md")
	mi := strings.Index(got, "mid.md")
	zi := strings.Index(got, "zeta.md")
	if ai < 0 || mi < 0 || zi < 0 {
		t.Fatalf("expected all three filenames in output, got:\n%s", got)
	}
	if !(ai < mi && mi < zi) {
		t.Errorf("expected alpha < mid < zeta in output, got positions %d/%d/%d:\n%s", ai, mi, zi, got)
	}
}

// Missing frontmatter falls back to filename for title and a "(missing)"
// hint for applies-when — fail-soft, not fail-loud. A half-written
// entry shouldn't break every stage prompt, but the placeholder is
// visible enough that the operator notices.
func TestLoreReferenceSectionFallsBackForMissingFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", "no-frontmatter.md"),
		"# Plain markdown, no frontmatter at all\n\nbody\n")
	writeFile(t, filepath.Join(root, "lore", "title-only.md"),
		"---\ntitle: Just A Title\n---\n\nbody\n")
	got := LoreReferenceSectionAt(root)
	for _, want := range []string{
		"no-frontmatter",
		"(missing)",
		"Just A Title",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lore reference missing %q in:\n%s", want, got)
		}
	}
}

// Files starting with `.` or `_` are operator escape hatches for
// drafts and stashed entries — never injected into the catalog.
// Assertions check filename leaves and unique title sentinels rather
// than the words "Hidden"/"Draft" (which appear in the test name and
// therefore the tempdir path).
func TestLoreReferenceSectionSkipsHiddenAndDraftFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", ".hidden.md"),
		"---\ntitle: SENTINEL-hidden-title\napplies-when: never\n---\n")
	writeFile(t, filepath.Join(root, "lore", "_draft.md"),
		"---\ntitle: SENTINEL-draft-title\napplies-when: never\n---\n")
	writeFile(t, filepath.Join(root, "lore", "real.md"),
		"---\ntitle: Real Entry\napplies-when: always\n---\n")
	got := LoreReferenceSectionAt(root)
	if !strings.Contains(got, "Real Entry") {
		t.Errorf("real entry missing in:\n%s", got)
	}
	for _, skipped := range []string{
		"SENTINEL-hidden-title",
		"SENTINEL-draft-title",
		"/.hidden.md",
		"/_draft.md",
	} {
		if strings.Contains(got, skipped) {
			t.Errorf("expected %q to be skipped, but found in:\n%s", skipped, got)
		}
	}
}

// Past the soft cap the block still renders fully — the warning is a
// nudge, not a gate.
func TestLoreReferenceSectionOverCapWarning(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < LoreSoftCap+3; i++ {
		writeFile(t,
			filepath.Join(root, "lore", fmt.Sprintf("entry-%02d.md", i)),
			fmt.Sprintf("---\ntitle: Entry %d\napplies-when: test %d\n---\n", i, i))
	}
	got := LoreReferenceSectionAt(root)
	if !strings.Contains(got, fmt.Sprintf("%d lore entries (soft cap %d)", LoreSoftCap+3, LoreSoftCap)) {
		t.Errorf("expected over-cap warning, got:\n%s", got)
	}
	// All entries still rendered.
	for i := 0; i < LoreSoftCap+3; i++ {
		want := fmt.Sprintf("Entry %d", i)
		if !strings.Contains(got, want) {
			t.Errorf("entry %q missing from over-cap render", want)
		}
	}
}

// At-cap (exactly LoreSoftCap entries) does NOT carry the warning —
// only `> cap` does.
func TestLoreReferenceSectionAtCapNoWarning(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < LoreSoftCap; i++ {
		writeFile(t,
			filepath.Join(root, "lore", fmt.Sprintf("entry-%02d.md", i)),
			fmt.Sprintf("---\ntitle: Entry %d\napplies-when: test %d\n---\n", i, i))
	}
	got := LoreReferenceSectionAt(root)
	if strings.Contains(got, "soft cap") {
		t.Errorf("expected no warning at exactly cap, got:\n%s", got)
	}
}

// Quoted frontmatter values get their quotes stripped — the catalog
// shouldn't render `"foo"` when the operator's YAML used quotes for
// safety.
func TestLoreReferenceSectionStripsQuotes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", "quoted.md"),
		"---\ntitle: \"Quoted Title\"\napplies-when: 'single quoted'\n---\n")
	got := LoreReferenceSectionAt(root)
	if !strings.Contains(got, "Quoted Title") || strings.Contains(got, `"Quoted Title"`) {
		t.Errorf("expected unquoted title in output, got:\n%s", got)
	}
	if !strings.Contains(got, "single quoted") || strings.Contains(got, "'single quoted'") {
		t.Errorf("expected unquoted applies-when in output, got:\n%s", got)
	}
}

// LoreReferenceSection (Config-flavored) defers to the path-flavored
// variant, same shape as TwinReferenceSection / TwinReferenceSectionAt.
func TestLoreReferenceSectionFromConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "lore", "x.md"),
		"---\ntitle: From Config\napplies-when: always\n---\n")
	got := LoreReferenceSection(Config{BureaucracyPath: root})
	if !strings.Contains(got, "From Config") {
		t.Errorf("LoreReferenceSection(cfg) missed entry, got:\n%s", got)
	}
}
