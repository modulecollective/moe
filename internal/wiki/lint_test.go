package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintPromptSectionOpenSchema(t *testing.T) {
	cfg := Config{
		Name:              "kb",
		ContentDir:        "/some/path/projects/p/kb",
		Mode:              Open,
		IngestPrompt:      "Project's open-schema knowledge base.",
		AllowedPrimitives: []string{"split", "merge", "rename", "retire"},
	}
	got := LintPromptSection(cfg)
	for _, want := range []string{
		"Project's open-schema knowledge base.",
		"## Wiki: kb (open-schema)",
		"/some/path/projects/p/kb",
		"Lint pass (open-schema)",
		"Structural",
		"Semantic",
		"[wiki-op] split",
		"[wiki-op] retire",
		"/some/path/projects/p/kb/.wiki-ops",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lint prompt missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ingest session") {
		t.Errorf("lint prompt leaked ingest framing:\n%s", got)
	}
}

// Closed-schema lint folded into reflect; LintPromptSection on a
// closed-schema config is a misregistration and panics.
func TestLintPromptSectionPanicsOnClosed(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("LintPromptSection should panic on closed-schema")
		}
	}()
	LintPromptSection(Config{Name: "twin", Mode: Closed})
}

func TestScanCleanWiki(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"),
		"# kb\n\n- [DNS basics](topics/dns-basics.md)\n- [TCP](topics/tcp.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns-basics.md"),
		"# DNS basics\n\nSee also [TCP](tcp.md).\n")
	writeFile(t, filepath.Join(dir, "topics", "tcp.md"),
		"# TCP\n\nThree-way handshake is described elsewhere.\n")

	f, err := Scan(Config{ContentDir: dir, Mode: Open})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !f.IsEmpty() {
		t.Fatalf("expected clean wiki, got %+v", f)
	}
}

func TestScanFlagsOrphanBrokenAndEmpty(t *testing.T) {
	dir := t.TempDir()
	// index.md links to one missing file and one real file; one
	// topic doc is on disk but unreferenced (orphan); one topic doc
	// is empty; one topic doc has a broken cross-link.
	writeFile(t, filepath.Join(dir, "index.md"),
		"# kb\n\n- [DNS](topics/dns.md)\n- [Phantom](topics/missing.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\nSee [TCP handshake](tcp-handshake.md) for context.\n")
	writeFile(t, filepath.Join(dir, "topics", "orphan.md"),
		"# Orphan\n\nNobody links here.\n")
	writeFile(t, filepath.Join(dir, "topics", "stub.md"), "# Stub\n")

	f, err := Scan(Config{ContentDir: dir, Mode: Open})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got, want := f.Orphans, []string{"topics/orphan.md", "topics/stub.md"}; !equalStrings(got, want) {
		t.Errorf("orphans: got %v want %v", got, want)
	}
	if got, want := f.MissingFromIndex, []string{"topics/missing.md"}; !equalStrings(got, want) {
		t.Errorf("missing-from-index: got %v want %v", got, want)
	}
	if got, want := f.EmptyDocs, []string{"topics/stub.md"}; !equalStrings(got, want) {
		t.Errorf("empty: got %v want %v", got, want)
	}
	if len(f.BrokenLinks) != 1 ||
		f.BrokenLinks[0].From != "topics/dns.md" ||
		f.BrokenLinks[0].Target != "topics/tcp-handshake.md" {
		t.Errorf("broken links: %+v", f.BrokenLinks)
	}
}

func TestScanIgnoresExternalLinksAndAnchors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"),
		"# kb\n\n- [DNS](topics/dns.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\n"+
			"See [RFC 1035](https://example.com/rfc1035) and "+
			"the [intro section](#intro) for context. "+
			"Cross-link to [self](dns.md#section-2).\n")
	f, err := Scan(Config{ContentDir: dir, Mode: Open})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.BrokenLinks) != 0 {
		t.Errorf("external + anchor links should not flag as broken: %+v", f.BrokenLinks)
	}
}

func TestScanMissingContentDirIsCleanFindings(t *testing.T) {
	// A fresh-wiki lint shouldn't error — there's nothing to find.
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	f, err := Scan(Config{ContentDir: dir, Mode: Open})
	if err != nil {
		t.Fatalf("Scan on missing dir: %v", err)
	}
	if !f.IsEmpty() {
		t.Errorf("expected empty findings on missing dir, got %+v", f)
	}
}

// TestScanMissingTopicsDirIsCleanFindings covers the half-built case
// where index.md exists at the wiki root but topics/ has not been
// created yet. Scan should treat that identically to a missing wiki
// dir — empty findings, no error — so the operator can lint a fresh
// corpus without seeing a phantom failure.
func TestScanMissingTopicsDirIsCleanFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# kb\n\nNothing yet.\n")
	f, err := Scan(Config{ContentDir: dir, Mode: Open})
	if err != nil {
		t.Fatalf("Scan with missing topics dir: %v", err)
	}
	if !f.IsEmpty() {
		t.Errorf("expected empty findings with no topics dir, got %+v", f)
	}
}

func TestRenderFindingsGroupsByCategory(t *testing.T) {
	f := Findings{
		Orphans:          []string{"orphan.md"},
		MissingFromIndex: []string{"phantom.md"},
		BrokenLinks:      []BrokenLink{{From: "a.md", Target: "b.md"}},
		EmptyDocs:        []string{"stub.md"},
	}
	got := RenderFindings(f)
	for _, want := range []string{
		"## Structural pre-scan",
		"**Orphaned topic docs**",
		"- orphan.md",
		"**Index entries pointing at missing files**",
		"- phantom.md",
		"**Broken cross-links**",
		"- a.md → b.md",
		"**Empty or stub docs**",
		"- stub.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderFindings missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderFindingsEmptyReturnsEmpty(t *testing.T) {
	if got := RenderFindings(Findings{}); got != "" {
		t.Errorf("expected empty string for clean findings, got %q", got)
	}
}

// Glossary orphan scan: a glossary entry whose term doesn't appear in
// any other managed doc is an orphan (retire it or restore the prose
// reference). The check only fires under closed-schema and only when
// glossary.md is in the managed-doc set; absent or empty glossary is
// a no-op.
func TestScanGlossaryOrphans(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"),
		"# Vision\n\nThe sandbox worktree is per-run.\n")
	writeFile(t, filepath.Join(dir, "architecture.md"),
		"# Architecture\n\nThe wiki engine has two modes.\n")
	writeFile(t, filepath.Join(dir, "glossary.md"),
		"# Glossary\n\n"+
			"### Sandbox worktree\n\nPer-run working tree of the target submodule.\n\n"+
			"### Wiki engine\n\nGeneric engine backing kb and twin.\n\n"+
			"### Phantom term\n\nNobody references this in the prose.\n")
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
			{Filename: "glossary.md", Title: "Glossary"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.GlossaryOrphans, []string{"Phantom term"}; !equalStrings(got, want) {
		t.Errorf("GlossaryOrphans: got %v want %v", got, want)
	}
}

// A glossary.md with no H3 entries doesn't produce orphan noise — the
// first reflect pass after the engine change adds the doc, and the
// initial stub is just `# Glossary\n` until the agent populates it.
func TestScanGlossaryOrphansEmptyGlossary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n\nbody\n")
	writeFile(t, filepath.Join(dir, "glossary.md"), "# Glossary\n\n")
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "glossary.md", Title: "Glossary"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.GlossaryOrphans) != 0 {
		t.Errorf("expected no orphans for an empty glossary, got %v", f.GlossaryOrphans)
	}
}

// Render path: glossary orphans surface under their own labelled
// bullet group so the agent knows what the rubric is for.
func TestRenderFindingsIncludesGlossaryOrphans(t *testing.T) {
	got := RenderFindings(Findings{GlossaryOrphans: []string{"Phantom term"}})
	for _, want := range []string{
		"## Structural pre-scan",
		"**Glossary orphans**",
		"- Phantom term",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderFindings missing %q in:\n%s", want, got)
		}
	}
}

// equalStrings compares two string slices element-wise. We can't use
// reflect.DeepEqual on []string{} vs nil cleanly; this helper treats
// them as equal when both have the same elements in the same order.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Defensive: if the lint preamble starts to silently drop the
// .wiki-ops path, the agent loses its tag-emission target. Belt-and-
// suspenders alongside the prompt-content assertions above.
func TestLintPromptSectionMentionsStashFile(t *testing.T) {
	cfg := Config{Name: "kb", ContentDir: "/x/y", Mode: Open}
	got := LintPromptSection(cfg)
	if !strings.Contains(got, OpsStashPath(cfg.ContentDir)) {
		t.Errorf("lint prompt should print %q:\n%s", OpsStashPath(cfg.ContentDir), got)
	}
}

// Sanity check the writeFile helper from wiki_test.go produces a real
// file Scan can find — a regression here would mean the other tests
// pass for the wrong reason.
func TestScanFixtureSanity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "topic.md")
	writeFile(t, path, "# Topic\n\nbody\n")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("writeFile didn't create %s: %v", path, err)
	}
}
