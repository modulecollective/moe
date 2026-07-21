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

func TestScanClosedSurfacesSoftBudgetWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n\n"+strings.Repeat("x", 1024))
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision", SoftBudgetKB: 1},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.OverBudgetDocs, []string{"vision.md"}; !equalStrings(got, want) {
		t.Errorf("OverBudgetDocs: got %v want %v", got, want)
	}
	if f.IsEmpty() {
		t.Fatal("soft budget warning should render in the kickoff findings")
	}
	if f.HasBlocking() {
		t.Fatal("soft budget warning must not be a blocking finding")
	}
	got := RenderFindings(f)
	for _, want := range []string{
		"Docs over their soft size budget",
		"warning does not block finalize",
		"- vision.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderFindings missing %q:\n%s", want, got)
		}
	}
}

// architectureFixture is the cited doc for the dangling-xref tests. It
// carries the anchor shapes the real twin uses: markdown headings, a
// backticked bold lead, a long bold lead a citation will quote only
// the first clause of, a bold lead wrapped across source lines inside
// a blank-line-free list, and a plain-prose lead-in that is *not* an
// anchor.
const architectureFixture = `# Architecture

## Components

- **` + "`moe intent`" + `** — the intent surface.
- **Motion roots in operator verbs; merges are hook-gated.**
  Everything else follows from that.

Top-level command surface today:

## Decisions

- **Section-scoped segment resolution.** ` + "`" + `"A → B"` + "`" + ` needs B under A.
- **Prefix match, not
  substring.** Substring would silently accept an imprecise citation.
`

// The core case: a managed doc citing another by quoted heading. A
// citation resolves when every segment names a heading or bold lead in
// the cited doc; anything else is a pointer some rename stranded.
func TestScanDanglingXrefs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "architecture.md"), architectureFixture)
	writeFile(t, filepath.Join(dir, "vision.md"), `# Vision

The intent surface is architecture.md "Components → `+"`moe intent`"+`"
and it holds.

The framing lives in architecture.md "The bets" — or it used to.

A citation that wraps across source lines runs into architecture.md
"Motion roots in operator verbs." and still resolves.

Blank-line-free list entries anchor too: architecture.md
"Prefix match, not substring."

A path with a dead leaf: architecture.md "Components → Nonexistent lead".

Quoted prose like "what's in play" names no doc, so it is not a
citation. Neither is README.md "Anything" — not a managed doc.
`)
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []DanglingXref{
		{From: "vision.md", Target: "architecture.md", Span: "Components → Nonexistent lead"},
		{From: "vision.md", Target: "architecture.md", Span: "The bets"},
	}
	if len(f.DanglingXrefs) != len(want) {
		t.Fatalf("DanglingXrefs: got %+v want %+v", f.DanglingXrefs, want)
	}
	for i := range want {
		if f.DanglingXrefs[i] != want[i] {
			t.Errorf("DanglingXrefs[%d]: got %+v want %+v", i, f.DanglingXrefs[i], want[i])
		}
	}
}

// The resolution rules in isolation, so a regression names the rule it
// broke rather than just moving a count.
func TestXrefResolves(t *testing.T) {
	catalogue := xrefCatalogue(architectureFixture)
	cases := []struct {
		name string
		span string
		want bool
	}{
		{"plain heading", "Components", true},
		{"backticked bold lead cited bare", "moe intent", true},
		{"path through heading to lead", "Components → `moe intent`", true},
		{"first clause of a long lead", "Motion roots in operator verbs.", true},
		{"lead wrapped across source lines", "Prefix match, not substring.", true},
		{"trailing punctuation and case folded", "COMPONENTS,", true},
		{"prefix, not substring", "intent", false},
		{"plain prose is not an anchor", "Top-level command surface", false},
		{"one dead segment fails the path", "Components → Nonexistent lead", false},
		{"unknown heading", "The bets", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := xrefResolves(tc.span, catalogue); got != tc.want {
				t.Errorf("xrefResolves(%q) = %v, want %v", tc.span, got, tc.want)
			}
		})
	}
}

// scopedFixture reproduces the incident this check was hardened for: a
// reflect pass moved the `internal/…` package leads out of
// `## Components` into a new `## Domain packages`, and both sections
// survived. Every segment of a stale "Components → internal/dash"
// pointer still names something in the doc — only the nesting is gone.
//
// It also carries a lead text that appears under two sections (so a
// scoped match has to backtrack), a sub-bullet that nests by indent
// alone, and a long lead a citation quotes only the first clause of.
const scopedFixture = `# Architecture

## Components

- **moe pulse** — the dashboard verb.
  - **moe pulse --watch** — the live variant.
- **Shared surface** — the Components one.

## Domain packages

- **` + "`internal/dash`" + `** — dashboard rendering.
- **Shared surface** — the Domain packages one, same text.
  - **Nested only here** — the sub-bullet that makes backtracking matter.

## Decisions

- **Hooks gate merges; the operator never rebases by hand.**
  Everything else follows from that.
`

// Section scoping in isolation: a segment must name an entry inside the
// previous segment's section, and the search backtracks across every
// section a segment could name.
func TestXrefResolvesSectionScoped(t *testing.T) {
	catalogue := xrefCatalogue(scopedFixture)
	cases := []struct {
		name string
		span string
		want bool
	}{
		{"the stranded pointer: lead moved to another section", "Components → `internal/dash`", false},
		{"same lead cited under its new section", "Domain packages → `internal/dash`", true},
		{"sub-bullet nests inside its parent bullet by indent", "Components → moe pulse → moe pulse --watch", true},
		{"duplicate lead text resolves under either section", "Components → Shared surface", true},
		{"backtracks past a first match with an empty section", "Shared surface → Nested only here", true},
		{"nesting is not transitive across sections", "Components → Shared surface → Nested only here", false},
		{"first clause of a long lead, in scope", "Decisions → Hooks gate merges.", true},
		{"single segment still resolves doc-wide", "Nested only here", true},
		{"dead leaf inside a live section", "Domain packages → Nonexistent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := xrefResolves(tc.span, catalogue); got != tc.want {
				t.Errorf("xrefResolves(%q) = %v, want %v", tc.span, got, tc.want)
			}
		})
	}
}

// End to end through Scan: the section move strands pointers, and a
// continuation quote is checked against the doc its line already named.
func TestScanDanglingXrefsScopedAndContinuation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "architecture.md"), scopedFixture)
	writeFile(t, filepath.Join(dir, "glossary.md"), `# Glossary

### internal/dash

The package lives in architecture.md "Domain packages → `+"`internal/dash`"+`"
and the verb in "Components → moe pulse".

### operator

The word "operator" is prose, not a pointer: it precedes any citation
on this line, so architecture.md "Components" is the only span checked.

### moe pulse

Stranded by the section move: architecture.md "Components → `+"`internal/dash`"+`".

### hooks

A stale continuation: architecture.md "Decisions" and "The bets".
`)
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "glossary.md", Title: "Glossary"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []DanglingXref{
		{From: "glossary.md", Target: "architecture.md", Span: "Components → `internal/dash`"},
		{From: "glossary.md", Target: "architecture.md", Span: "The bets"},
	}
	if len(f.DanglingXrefs) != len(want) {
		t.Fatalf("DanglingXrefs: got %+v want %+v", f.DanglingXrefs, want)
	}
	for i := range want {
		if f.DanglingXrefs[i] != want[i] {
			t.Errorf("DanglingXrefs[%d]: got %+v want %+v", i, f.DanglingXrefs[i], want[i])
		}
	}
	if !f.HasBlocking() {
		t.Error("a stranded pointer must block finalize")
	}
}

// A citation into a managed doc that isn't on disk is MissingManagedDocs'
// story, not this scan's — otherwise every pointer into the absent doc
// piles onto the same finding.
func TestScanDanglingXrefsSkipsAbsentTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"),
		"# Vision\n\nSee architecture.md \"Components\" for the shape.\n")
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.DanglingXrefs) != 0 {
		t.Errorf("absent target should not produce xref findings, got %+v", f.DanglingXrefs)
	}
	if got, want := f.MissingManagedDocs, []string{"architecture.md"}; !equalStrings(got, want) {
		t.Errorf("MissingManagedDocs: got %v want %v", got, want)
	}
}

// Open-schema wikis cross-reference with markdown links, which
// BrokenLinks covers; the quoted-heading convention is a twin
// convention and the scan must stay off open-schema corpora.
func TestScanDanglingXrefsClosedSchemaOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.md"), "# Index\n\n[DNS](topics/dns.md)\n")
	writeFile(t, filepath.Join(dir, "topics", "dns.md"),
		"# DNS\n\nSee architecture.md \"Nonexistent heading\" for more.\n")
	f, err := Scan(Config{Mode: Open, ContentDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.DanglingXrefs) != 0 {
		t.Errorf("open-schema scan should not produce xref findings, got %+v", f.DanglingXrefs)
	}
}

// Render path: dangling xrefs get their own labelled group, and the
// rubric line is how a stage agent learns what fix the entry invites.
func TestRenderFindingsIncludesDanglingXrefs(t *testing.T) {
	got := RenderFindings(Findings{DanglingXrefs: []DanglingXref{
		{From: "vision.md", Target: "architecture.md", Span: "The bets"},
	}})
	for _, want := range []string{
		"**Dangling cross-refs**",
		"repoint the citation or restore the heading",
		`- vision.md: architecture.md "The bets"`,
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
	if !strings.Contains(got, opsStashPath(cfg.ContentDir)) {
		t.Errorf("lint prompt should print %q:\n%s", opsStashPath(cfg.ContentDir), got)
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
