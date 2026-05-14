package wiki

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LintPromptSection is the wiki-specific block appended to the system
// prompt for an open-schema lint session. Sibling of
// IngestPromptSection: same preamble (schema-config body, wiki
// header, on-disk shape), different framing — a health pass instead
// of an ingest. The closed-schema hygiene pass folded into
// `moe twin reflect` (see ReflectPromptSection); this surface is
// now open-schema only and panics on Closed so a misregistration is
// loud rather than silent.
func LintPromptSection(cfg Config) string {
	if cfg.Mode != Open {
		// Closed-schema hygiene runs inside reflect now. A registration
		// that points lintCommand at a closed-schema builder is a bug —
		// fail loud so the operator notices instead of getting an
		// open-schema prompt against a twin.
		panic(fmt.Sprintf("wiki: LintPromptSection is open-schema only (got %s)", cfg.Mode))
	}
	var b strings.Builder
	b.WriteString(wikiPreamble(cfg))
	b.WriteString(`Lint pass (open-schema):

This is a health pass on the wiki, not fresh-source ingestion. There
is no bibliography to work in. Walk the corpus with the operator and
look for:

- **Structural** (topology of the wiki): orphaned topic docs, broken
  cross-links, index.md entries pointing at missing files, empty or
  stub topic docs. The engine has pre-scanned these and seeded them
  in your kickoff prompt as a known-issues block.
- **Semantic** (substance of the corpus): two docs covering the same
  ground (merge candidate), one doc grown too broad (split candidate),
  title drift (rename candidate), no-inbound-link docs whose content
  is duplicated elsewhere (retire candidate), index.md taxonomy that
  no longer matches doc contents.

Apply fixes inline as you and the operator agree on them. Findings
you don't act on this pass should be raised to the operator and
either deferred (note in the conversation) or surfaced as their own
follow-up — don't silently leave them.

The same schema-evolution primitives and rules apply as during ingest.
When you apply one, append a [wiki-op] tag to the engine's stash file
in the same shape ingest uses:

    [wiki-op] split <src>.md → <dst1>.md, <dst2>.md
    [wiki-op] merge <src>.md into <dst>.md
    [wiki-op] rename <old>.md → <new>.md
    [wiki-op] retire <doc>.md

Stash file: ` + OpsStashPath(cfg.ContentDir) + `

Do not edit log.md or checkpoint.json — the engine writes those at
session close, the same as during ingest.

`)
	if len(cfg.AllowedPrimitives) > 0 {
		fmt.Fprintf(&b, "Allowed primitives: %s.\n", strings.Join(cfg.AllowedPrimitives, ", "))
	} else {
		b.WriteString("Allowed primitives: (none — content edits only).\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Findings is the structural pre-scan result. Each field is a sorted
// list so the rendered known-issues block is deterministic.
//
// Scan is intentionally narrow: only deterministic, file-shape
// problems land here. Semantic findings (overlap, breadth, framing
// drift) come from the agent walking the corpus during the session.
type Findings struct {
	// Orphans are topic docs present on disk but not referenced from
	// index.md. Paths are relative to ContentDir (e.g.
	// "topics/dns.md"). Open-schema only.
	Orphans []string
	// MissingFromIndex are paths named in index.md links that don't
	// resolve to a topic doc on disk. Paths are relative to ContentDir,
	// resolved from the as-written link target (e.g. an index entry
	// "[X](topics/missing.md)" surfaces as "topics/missing.md").
	// Open-schema only.
	MissingFromIndex []string
	// BrokenLinks are internal links from topic docs (open-schema) or
	// managed docs (closed-schema) that point at files that don't
	// exist.
	BrokenLinks []BrokenLink
	// EmptyDocs are docs with no meaningful content (zero-byte,
	// whitespace-only, or just a title heading and nothing else).
	// In closed-schema, this captures unfilled managed docs (typically
	// just-bootstrapped stubs).
	EmptyDocs []string
	// MissingManagedDocs are filenames declared in cfg.ManagedDocs
	// that don't exist on disk. Closed-schema only — open-schema
	// wikis don't declare a fixed doc set.
	MissingManagedDocs []string
	// GlossaryOrphans are glossary entries (H3 terms in glossary.md)
	// whose term text appears nowhere in the other managed docs.
	// Closed-schema only. The contract is that the glossary is an
	// index over the prose, not a separate definition surface — an
	// orphan means the prose moved on without the entry.
	GlossaryOrphans []string
}

// BrokenLink is one cross-link a topic doc makes that doesn't resolve.
type BrokenLink struct {
	From   string // doc that contains the link, relative to ContentDir (e.g. "topics/dns.md")
	Target string // path the link resolves to, relative to ContentDir
}

// IsEmpty reports whether Scan found no structural issues at all.
// Used to short-circuit rendering the known-issues block when the
// wiki is clean — no point seeding the agent with an empty list.
func (f Findings) IsEmpty() bool {
	return len(f.Orphans) == 0 &&
		len(f.MissingFromIndex) == 0 &&
		len(f.BrokenLinks) == 0 &&
		len(f.EmptyDocs) == 0 &&
		len(f.MissingManagedDocs) == 0 &&
		len(f.GlossaryOrphans) == 0
}

// Scan walks the wiki content directory and returns the structural
// findings. A missing ContentDir (or missing topics/ subdir) is not an
// error — it produces empty findings (a fresh-wiki lint has nothing to
// find).
//
// The scan is best-effort and does not fail on per-file read errors:
// a doc the engine couldn't read becomes an absence in the orphan /
// link checks rather than a hard error. Errors that would prevent any
// scan from completing (e.g. ReadDir on the topics dir failing for
// reasons other than ENOENT) propagate.
//
// Open-schema: topic docs live under <ContentDir>/topics/; the
// catalogue is keyed by path relative to ContentDir (e.g.
// "topics/dns.md") so it matches index.md link targets verbatim, and so
// a topic doc using a flat sibling reference like [other](other.md)
// resolves correctly relative to its own directory.
//
// Closed-schema: the catalogue is cfg.ManagedDocs (flat, no topics/);
// MissingManagedDocs flags unstubbed docs.
func Scan(cfg Config) (Findings, error) {
	if cfg.Mode == Closed {
		return scanClosed(cfg)
	}
	return scanOpen(cfg)
}

func scanOpen(cfg Config) (Findings, error) {
	var f Findings

	topicsDir := TopicsDir(cfg.ContentDir)
	entries, err := os.ReadDir(topicsDir)
	if err != nil && !os.IsNotExist(err) {
		return f, fmt.Errorf("wiki: read %s: %w", topicsDir, err)
	}

	// Catalogue topic docs. Map keys are ContentDir-relative slash paths
	// (e.g. "topics/dns.md") so link-target resolution and index-entry
	// comparison work in a single namespace.
	topics := map[string]bool{}
	var topicList []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		rel := path.Join(TopicsSubdir, name)
		topics[rel] = true
		topicList = append(topicList, rel)
	}

	indexBody, indexExists, err := readMaybe(IndexPath(cfg.ContentDir))
	if err != nil {
		return f, err
	}
	indexed := map[string]bool{}
	for _, link := range extractMarkdownLinks(indexBody) {
		// Only consider local .md links — external URLs and anchors
		// aren't part of the index/topic relationship.
		if !isLocalMarkdownLink(link) {
			continue
		}
		canon := resolveLinkTarget(link, "index.md")
		indexed[canon] = true
		if !topics[canon] {
			f.MissingFromIndex = append(f.MissingFromIndex, canon)
		}
	}

	// Orphans — only meaningful when index.md exists. Without one,
	// every topic doc would be flagged as orphaned, which says nothing
	// the operator doesn't already know.
	if indexExists {
		for _, t := range topicList {
			if !indexed[t] {
				f.Orphans = append(f.Orphans, t)
			}
		}
	}

	// Walk every topic doc for broken internal links and empty bodies.
	for _, t := range topicList {
		body, _, err := readMaybe(filepath.Join(cfg.ContentDir, t))
		if err != nil {
			return f, err
		}
		if isEffectivelyEmpty(body) {
			f.EmptyDocs = append(f.EmptyDocs, t)
		}
		for _, link := range extractMarkdownLinks(body) {
			if !isLocalMarkdownLink(link) {
				continue
			}
			canon := resolveLinkTarget(link, t)
			if topics[canon] || canon == "index.md" {
				continue
			}
			f.BrokenLinks = append(f.BrokenLinks, BrokenLink{From: t, Target: canon})
		}
	}

	sort.Strings(f.Orphans)
	sort.Strings(f.MissingFromIndex)
	sort.Strings(f.EmptyDocs)
	sort.Slice(f.BrokenLinks, func(i, j int) bool {
		if f.BrokenLinks[i].From != f.BrokenLinks[j].From {
			return f.BrokenLinks[i].From < f.BrokenLinks[j].From
		}
		return f.BrokenLinks[i].Target < f.BrokenLinks[j].Target
	})

	return f, nil
}

// RenderFindings formats f as a markdown known-issues block suitable
// for splicing into the lint kickoff prompt. Returns "" when f is
// empty so callers can drop the heading entirely on a clean wiki.
//
// The block is grouped by category, with each group prefixed by a
// short rubric so the agent knows what kind of fix the entries
// invite. Operator judgement decides what to act on — the renderer
// doesn't editorialise.
func RenderFindings(f Findings) string {
	if f.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Structural pre-scan\n\n")
	b.WriteString("The engine has flagged the following deterministic issues " +
		"under the wiki dir. Walk through them with the operator and decide " +
		"which to fix this pass.\n\n")
	if len(f.Orphans) > 0 {
		b.WriteString("**Orphaned topic docs** (present but not in index.md):\n")
		for _, o := range f.Orphans {
			fmt.Fprintf(&b, "- %s\n", o)
		}
		b.WriteString("\n")
	}
	if len(f.MissingFromIndex) > 0 {
		b.WriteString("**Index entries pointing at missing files**:\n")
		for _, m := range f.MissingFromIndex {
			fmt.Fprintf(&b, "- %s\n", m)
		}
		b.WriteString("\n")
	}
	if len(f.BrokenLinks) > 0 {
		b.WriteString("**Broken cross-links** (link in left doc, missing target on the right):\n")
		for _, bl := range f.BrokenLinks {
			fmt.Fprintf(&b, "- %s → %s\n", bl.From, bl.Target)
		}
		b.WriteString("\n")
	}
	if len(f.EmptyDocs) > 0 {
		b.WriteString("**Empty or stub docs** (zero meaningful content):\n")
		for _, e := range f.EmptyDocs {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		b.WriteString("\n")
	}
	if len(f.MissingManagedDocs) > 0 {
		b.WriteString("**Missing managed docs** (declared in schema, not present on disk):\n")
		for _, m := range f.MissingManagedDocs {
			fmt.Fprintf(&b, "- %s\n", m)
		}
		b.WriteString("\n")
	}
	if len(f.GlossaryOrphans) > 0 {
		b.WriteString("**Glossary orphans** (entry term appears nowhere else — retire it or restore the prose reference):\n")
		for _, o := range f.GlossaryOrphans {
			fmt.Fprintf(&b, "- %s\n", o)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// scanClosed walks a closed-schema wiki: catalogues managed docs from
// cfg.ManagedDocs (flat, no topics/), checks each is on disk, and runs
// the per-doc empty/broken-link checks against the same flat namespace.
func scanClosed(cfg Config) (Findings, error) {
	var f Findings

	// Catalogue managed docs from cfg, not from disk — closed-schema
	// invariants forbid extras. Disk presence drives MissingManagedDocs.
	docs := map[string]bool{}
	var docList []string
	for _, d := range cfg.ManagedDocs {
		docs[d.Filename] = true
		docList = append(docList, d.Filename)
	}
	for _, name := range docList {
		if _, err := os.Stat(filepath.Join(cfg.ContentDir, name)); err != nil {
			if os.IsNotExist(err) {
				f.MissingManagedDocs = append(f.MissingManagedDocs, name)
				continue
			}
			return f, fmt.Errorf("wiki: stat %s: %w", name, err)
		}
	}

	// Walk every present managed doc for empty bodies and broken
	// cross-links.
	for _, name := range docList {
		body, ok, err := readMaybe(filepath.Join(cfg.ContentDir, name))
		if err != nil {
			return f, err
		}
		if !ok {
			continue
		}
		if isEffectivelyEmpty(body) {
			f.EmptyDocs = append(f.EmptyDocs, name)
		}
		for _, link := range extractMarkdownLinks(body) {
			if !isLocalMarkdownLink(link) {
				continue
			}
			canon := resolveLinkTarget(link, name)
			if docs[canon] {
				continue
			}
			f.BrokenLinks = append(f.BrokenLinks, BrokenLink{From: name, Target: canon})
		}
	}

	f.GlossaryOrphans = scanGlossaryOrphans(cfg)

	sort.Strings(f.MissingManagedDocs)
	sort.Strings(f.EmptyDocs)
	sort.Strings(f.GlossaryOrphans)
	sort.Slice(f.BrokenLinks, func(i, j int) bool {
		if f.BrokenLinks[i].From != f.BrokenLinks[j].From {
			return f.BrokenLinks[i].From < f.BrokenLinks[j].From
		}
		return f.BrokenLinks[i].Target < f.BrokenLinks[j].Target
	})
	return f, nil
}

// glossaryDocName is the closed-schema glossary doc the orphan scan
// targets. Kept in one place so a future rename (or a per-project
// override) lands in a single spot.
const glossaryDocName = "glossary.md"

// glossaryTermPattern matches the `### Term` H3 line a glossary entry
// opens with. The term capture includes any trailing whitespace, which
// scanGlossaryOrphans trims before use.
var glossaryTermPattern = regexp.MustCompile(`(?m)^###\s+(.+?)\s*$`)

// scanGlossaryOrphans extracts H3 entries from glossary.md and returns
// the terms whose text doesn't appear anywhere in the other managed
// docs. The orphan signal is "the prose moved on without the entry" —
// either retire the entry or restore the prose reference. Returns nil
// when glossary.md is absent (a fresh twin has no entries to orphan).
//
// Matching is case-insensitive substring: a glossary entry titled
// `### Sandbox worktree` is satisfied by prose that says "the sandbox
// worktree …". Headings in the glossary itself don't count — the
// check only walks the other managed docs.
func scanGlossaryOrphans(cfg Config) []string {
	managedHasGlossary := false
	for _, d := range cfg.ManagedDocs {
		if d.Filename == glossaryDocName {
			managedHasGlossary = true
			break
		}
	}
	if !managedHasGlossary {
		return nil
	}
	body, ok, err := readMaybe(filepath.Join(cfg.ContentDir, glossaryDocName))
	if err != nil || !ok {
		return nil
	}
	matches := glossaryTermPattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	var others strings.Builder
	for _, d := range cfg.ManagedDocs {
		if d.Filename == glossaryDocName {
			continue
		}
		otherBody, ok, err := readMaybe(filepath.Join(cfg.ContentDir, d.Filename))
		if err != nil || !ok {
			continue
		}
		others.WriteString(otherBody)
		others.WriteByte('\n')
	}
	prose := strings.ToLower(others.String())
	seen := map[string]bool{}
	var orphans []string
	for _, m := range matches {
		term := strings.TrimSpace(m[1])
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		if !strings.Contains(prose, strings.ToLower(term)) {
			orphans = append(orphans, term)
		}
	}
	return orphans
}

// readMaybe reads path, returning ("", false, nil) when the file is
// absent so callers can branch on existence without two filesystem
// trips. Other I/O errors propagate.
func readMaybe(path string) (string, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("wiki: read %s: %w", path, err)
	}
	return string(body), true, nil
}

// markdownLinkPattern matches `[text](target)` markdown inline links
// permissively. The capture is the target as authored — callers
// canonicalise from there. Reference-style links and image embeds are
// not in scope; topic docs in this engine use inline links.
var markdownLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)`)

// extractMarkdownLinks returns the targets of every `[text](target)`
// link in body, in document order, including duplicates. The caller
// filters for local .md links.
func extractMarkdownLinks(body string) []string {
	matches := markdownLinkPattern.FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
	}
	return out
}

// isLocalMarkdownLink reports whether link is a same-repo .md
// reference (rather than an http://, mailto:, anchor, etc.). Anchors
// like "topic.md#heading" still qualify — the file portion is what
// gets validated.
func isLocalMarkdownLink(link string) bool {
	if link == "" {
		return false
	}
	if strings.HasPrefix(link, "#") {
		return false
	}
	if strings.Contains(link, "://") {
		return false
	}
	if strings.HasPrefix(link, "mailto:") {
		return false
	}
	// Strip any fragment before deciding — `topic.md#heading` is a
	// local link to topic.md.
	target := link
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	return strings.HasSuffix(target, ".md")
}

// resolveLinkTarget canonicalises a markdown link target into a path
// relative to ContentDir, given the file the link appears in (also
// ContentDir-relative). Fragments are stripped; "./" and ".." segments
// are resolved against the link source's directory so a topic doc
// linking to a sibling ("other.md") and one linking up ("../index.md")
// both produce paths that match the topic catalogue or the engine-
// managed file names.
func resolveLinkTarget(link, fromRel string) string {
	target := link
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	// path.Clean uses forward slashes throughout — markdown link targets
	// are always slash-separated, and the catalogue keys use slashes
	// too, so resolution stays in one namespace regardless of host OS.
	return path.Clean(path.Join(path.Dir(fromRel), target))
}

// isEffectivelyEmpty reports whether body has no meaningful content.
// A zero-byte file is empty; a file with only whitespace is empty;
// a file with only a title heading (one `# Title` line) and nothing
// else is a stub. Anything past the title — even one paragraph —
// counts as content.
func isEffectivelyEmpty(body string) bool {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return true
	}
	lines := strings.Split(trimmed, "\n")
	// Skip a leading title heading; if anything non-empty remains,
	// the doc is not a stub.
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		lines = lines[1:]
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}
