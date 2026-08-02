package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/md"
)

// Findings is the structural pre-scan result. Each field is a sorted
// list so the rendered known-issues block is deterministic.
//
// Scan is intentionally narrow: only deterministic, file-shape
// problems land here. Semantic findings (overlap, breadth, framing
// drift) come from the agent walking the corpus during the session.
type Findings struct {
	// BrokenLinks are internal links from managed docs that point at
	// files that don't exist.
	BrokenLinks []BrokenLink
	// EmptyDocs are docs with no meaningful content (zero-byte,
	// whitespace-only, or just a title heading and nothing else) —
	// typically unfilled, just-bootstrapped stubs.
	EmptyDocs []string
	// MissingManagedDocs are filenames declared in cfg.ManagedDocs
	// that don't exist on disk.
	MissingManagedDocs []string
	// GlossaryOrphans are glossary entries (H3 terms in glossary.md)
	// whose term text appears nowhere in the other managed docs. The
	// contract is that the glossary is an index over the prose, not a
	// separate definition surface — an orphan means the prose moved on
	// without the entry.
	GlossaryOrphans []string
	// DanglingXrefs are citations of the form `other.md "Some heading"`
	// whose quoted span names no heading or bold lead in the cited doc.
	DanglingXrefs []DanglingXref
	// OverBudgetDocs are managed docs whose current size exceeds their
	// declared SoftBudgetKB. This is a hygiene nudge, not a blocking
	// structural finding: it reaches the reflect kickoff but never
	// prevents finalize from sealing.
	OverBudgetDocs []string
}

// DanglingXref is one quoted-heading citation that doesn't resolve.
type DanglingXref struct {
	From   string // managed doc containing the citation
	Target string // managed doc it cites
	Span   string // the quoted span as authored, line-unwrapped
}

// BrokenLink is one cross-link a managed doc makes that doesn't resolve.
type BrokenLink struct {
	From   string // doc that contains the link, relative to ContentDir
	Target string // path the link resolves to, relative to ContentDir
}

// IsEmpty reports whether Scan found no structural issues or soft
// hygiene warnings at all.
// Used to short-circuit rendering the known-issues block when the
// wiki is clean — no point seeding the agent with an empty list.
func (f Findings) IsEmpty() bool {
	return !f.HasBlocking() && len(f.OverBudgetDocs) == 0
}

// HasBlocking reports whether f contains a structural finding that
// must be cleared before a reflect pass can seal. Soft budget warnings
// are deliberately excluded.
func (f Findings) HasBlocking() bool {
	return len(f.BrokenLinks) > 0 ||
		len(f.EmptyDocs) > 0 ||
		len(f.MissingManagedDocs) > 0 ||
		len(f.GlossaryOrphans) > 0 ||
		len(f.DanglingXrefs) > 0
}

// Scan walks the wiki content directory and returns the structural
// findings. The catalogue is cfg.ManagedDocs (flat, no topics/);
// MissingManagedDocs flags unstubbed docs. A missing ContentDir is not
// an error — it produces empty findings (a fresh-wiki scan has nothing
// to find).
//
// The scan is best-effort and does not fail on per-file read errors:
// a doc the engine couldn't read becomes an absence in the link checks
// rather than a hard error. Errors that would prevent any scan from
// completing propagate.
func Scan(cfg Config) (Findings, error) {
	var f Findings

	// Catalogue managed docs from cfg, not from disk — the schema
	// invariants forbid extras. Disk presence drives MissingManagedDocs.
	docs := map[string]bool{}
	var docList []string
	for _, d := range cfg.ManagedDocs {
		docs[d.Filename] = true
		docList = append(docList, d.Filename)
		info, err := os.Stat(filepath.Join(cfg.ContentDir, d.Filename))
		if err != nil {
			if os.IsNotExist(err) {
				f.MissingManagedDocs = append(f.MissingManagedDocs, d.Filename)
				continue
			}
			return f, fmt.Errorf("wiki: stat %s: %w", d.Filename, err)
		}
		if d.SoftBudgetKB > 0 && info.Size() > int64(d.SoftBudgetKB)*1024 {
			f.OverBudgetDocs = append(f.OverBudgetDocs, d.Filename)
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
		if md.IsEffectivelyEmpty(body) {
			f.EmptyDocs = append(f.EmptyDocs, name)
		}
		for _, link := range md.LocalLinks(body) {
			canon := md.ResolveLink(link, name)
			if docs[canon] {
				continue
			}
			f.BrokenLinks = append(f.BrokenLinks, BrokenLink{From: name, Target: canon})
		}
	}

	f.GlossaryOrphans = scanGlossaryOrphans(cfg)
	f.DanglingXrefs = scanDanglingXrefs(cfg)

	sort.Strings(f.MissingManagedDocs)
	sort.Strings(f.EmptyDocs)
	sort.Strings(f.GlossaryOrphans)
	sort.Strings(f.OverBudgetDocs)
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
	if len(f.DanglingXrefs) > 0 {
		b.WriteString("**Dangling cross-refs** (quoted heading not found in the named doc — repoint the citation or restore the heading):\n")
		for _, x := range f.DanglingXrefs {
			fmt.Fprintf(&b, "- %s: %s %q\n", x.From, x.Target, x.Span)
		}
		b.WriteString("\n")
	}
	if len(f.OverBudgetDocs) > 0 {
		b.WriteString("**Docs over their soft size budget** (compression is in scope this pass; this warning does not block finalize):\n")
		for _, d := range f.OverBudgetDocs {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}
	return b.String()
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

// xrefHeadingPattern matches a markdown heading logical line. The first
// capture is the `#` run (its length is the heading's nesting level),
// the second is the heading text.
var xrefHeadingPattern = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

// xrefBoldLeadPattern matches a bold lead — the `**…**` span opening a
// logical line, optionally behind a list marker. This is the anchor
// shape the twin's `- **Name** — prose` entries under `## Components`
// and `## Decisions` present, and citations point at them the same way
// they point at headings.
var xrefBoldLeadPattern = regexp.MustCompile(`^\s*(?:[-*]\s+)?\*\*(.+?)\*\*`)

// xrefPathSeparator is the segment separator inside a quoted citation
// span (`"Components → moe pulse"`). The corpus uses U+2192
// exclusively; an ASCII "->" reads as segment text and will flag,
// which is the right nudge back to the convention.
const xrefPathSeparator = "→"

// scanDanglingXrefs finds citations of the form `other.md "Some
// heading"` in managed docs whose quoted span names nothing in the
// cited doc. Twin docs cite each other by quoted section heading
// rather than line number, and a heading rename is the cheapest edit a
// reflect pass makes — nothing in that pass's own diff shows the
// pointers it stranded. This is the check that makes "a rename strands
// no pointers" enforceable — for renames and for section moves alike
// (see xrefResolves), and for continuation citations that name their
// doc only once (see xrefCitations).
//
// Citations into a managed doc that isn't on disk are skipped:
// MissingManagedDocs already reports that, and every citation into the
// absent doc would otherwise pile on as noise.
func scanDanglingXrefs(cfg Config) []DanglingXref {
	names := make([]string, 0, len(cfg.ManagedDocs))
	for _, d := range cfg.ManagedDocs {
		names = append(names, d.Filename)
	}
	if len(names) == 0 {
		return nil
	}

	bodies := map[string]string{}
	for _, name := range names {
		body, ok, err := readMaybe(filepath.Join(cfg.ContentDir, name))
		if err != nil || !ok {
			continue
		}
		bodies[name] = body
	}

	catalogues := map[string][]xrefEntry{}
	for name, body := range bodies {
		catalogues[name] = xrefCatalogue(body)
	}

	pattern := xrefCitationPattern(names)
	var out []DanglingXref
	for _, from := range names {
		body, ok := bodies[from]
		if !ok {
			continue
		}
		for _, line := range logicalLines(body) {
			for _, c := range xrefCitations(pattern, line) {
				catalogue, ok := catalogues[c.target]
				if !ok {
					continue
				}
				if xrefResolves(c.span, catalogue) {
					continue
				}
				out = append(out, DanglingXref{From: from, Target: c.target, Span: c.span})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Span < out[j].Span
	})
	return out
}

// xrefCitationPattern builds the citation matcher: a managed-doc
// filename, then the first double-quoted span reachable across a short,
// punctuation-free run. Quotes further along the line are xrefCitations'
// problem, not this pattern's.
//
// The run is what lets the twin's possessive citations bind —
// `[architecture](architecture.md)'s "Components"`, sometimes with a
// backticked component name interposed. Everything excluded from it
// earns its place:
//
//   - `"` is structural. With quotes out of the run the pattern can
//     never skip one to reach a later quote, so it always binds the
//     first quote after the token.
//   - `.` is the sentence boundary, and does double duty: since every
//     managed name ends in `.md`, it also stops the run crossing a
//     second doc token, so `glossary.md entry for operations.md "X"`
//     binds to operations.md.
//   - `,;:!?—` are clause breaks. A quote past one of them is prose,
//     not the mention's span.
//   - `()` mean the mention ended in a parenthetical; the one
//     legitimate `)` is the link close, which the explicit `\)?` takes.
//   - `→` only ever appears inside a quoted span, and `*` bounds an
//     emphasised anchor reference that must not reach past itself.
//
// The {0,48} bound is a backstop, sized off the longest separator the
// corpus writes (41 bytes) with headroom; the exclusions are the guard.
//
// The alternation is built from cfg.ManagedDocs rather than hardcoded,
// longest name first so a doc whose name is a suffix of another still
// matches the longer one.
func xrefCitationPattern(names []string) *regexp.Regexp {
	sorted := append([]string(nil), names...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	alts := make([]string, len(sorted))
	for i, n := range sorted {
		alts[i] = regexp.QuoteMeta(n)
	}
	return regexp.MustCompile(`(?:^|[\s(])(` + strings.Join(alts, "|") + `)\)?[^".,;:!?()—→*]{0,48}\s"([^"]+)"`)
}

// xrefQuotedSpanPattern matches any double-quoted span on a logical
// line. xrefCitationPattern only binds the first quote after a doc
// token; this finds the rest so continuation citations get a target.
var xrefQuotedSpanPattern = regexp.MustCompile(`"([^"]+)"`)

// xrefCitation is one quoted span bound to the doc it cites.
type xrefCitation struct {
	target string
	span   string
}

// xrefCitations extracts every quoted span on line that names a managed
// doc. The first quote after a doc token binds to it directly; a later
// quote with no doc token of its own binds to the nearest *preceding*
// bound citation, which is how the corpus writes continuations
// (`operations.md "Stage sandboxes" and "Hook chains"`).
//
// Requiring a preceding bound citation is what keeps quoted prose out.
// An earlier attempt bound every later quote to the most recent doc
// *name* and flagged prose; position separates the two cleanly — the
// twin's prose quotes ("operator", "What was verified") all sit ahead
// of any citation on their logical line, so they stay unbound and
// unchecked. A prose quote written *after* a citation on one logical
// line would flag; the finding lands in the reflect kickoff, where
// rephrasing it is an inline edit.
func xrefCitations(pattern *regexp.Regexp, line string) []xrefCitation {
	type bound struct {
		start, end int // byte range of the quoted span, quotes included
		xrefCitation
	}
	var bounds []bound
	for _, idx := range pattern.FindAllStringSubmatchIndex(line, -1) {
		bounds = append(bounds, bound{
			start:        idx[4] - 1,
			end:          idx[5] + 1,
			xrefCitation: xrefCitation{target: line[idx[2]:idx[3]], span: line[idx[4]:idx[5]]},
		})
	}
	out := make([]xrefCitation, 0, len(bounds))
	for _, b := range bounds {
		out = append(out, b.xrefCitation)
	}
	for _, q := range xrefQuotedSpanPattern.FindAllStringSubmatchIndex(line, -1) {
		// Both scans run left to right, so the last bound citation
		// ending before this quote is the one it continues. A quote
		// overlapping a citation *is* that citation, already emitted —
		// or, on a line with unbalanced quotes, a straddling match that
		// binds nothing.
		prev := -1
		overlaps := false
		for i, b := range bounds {
			if q[0] < b.end && b.start < q[1] {
				overlaps = true
				break
			}
			if b.end <= q[0] {
				prev = i
			}
		}
		if overlaps || prev < 0 {
			continue
		}
		out = append(out, xrefCitation{target: bounds[prev].target, span: line[q[2]:q[3]]})
	}
	return out
}

// xrefEntry is one anchor a citation may point at — a heading or a bold
// lead — with the nesting level that decides what sits inside it.
type xrefEntry struct {
	text  string
	level int
}

// boldLeadBase is the level floor for bold leads: deeper than any
// heading (h1–h6), so every lead nests inside the section it appears
// under. A lead's own level adds its leading indent, which makes a
// sub-bullet lead (`  - **moe sdlc reopen …**`) nest inside its parent
// bullet without parsing list structure.
const boldLeadBase = 10

// xrefCatalogue collects everything in body a citation may legitimately
// point at: markdown headings and bold leads, normalised, in document
// order and carrying their nesting level.
func xrefCatalogue(body string) []xrefEntry {
	var out []xrefEntry
	for _, line := range logicalLines(body) {
		if m := xrefHeadingPattern.FindStringSubmatch(line); m != nil {
			out = append(out, xrefEntry{text: normaliseXrefText(m[2]), level: len(m[1])})
			continue
		}
		if m := xrefBoldLeadPattern.FindStringSubmatch(line); m != nil {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			out = append(out, xrefEntry{text: normaliseXrefText(m[1]), level: boldLeadBase + indent})
		}
	}
	return out
}

// xrefResolves reports whether span names a real path through
// catalogue: the first segment resolves doc-wide, and each later
// segment must name an entry *inside* the previous segment's section.
//
// Section scoping is what catches the failure this check exists for. A
// reflect pass moved architecture.md's `internal/…` package leads out
// of `## Components` into a new `## Domain packages`; both sections
// survived, so twelve glossary pointers reading "Components →
// internal/dash" still resolved segment-by-segment against a doc where
// no such path existed. Doc-wide resolution was deferred as a case that
// hadn't shown up; it showed up.
//
// A segment matches exactly or as a prefix of an entry. Prefix is
// load-bearing — citations quote the first clause of a long bold lead
// ("The bets → Motion roots in operator verbs." against
// `**Motion roots in operator verbs; merges are hook-gated.**`).
// Substring would go too far: it would silently accept `intent`
// against a `moe intent` lead, an imprecise citation the pass should
// tighten instead.
func xrefResolves(span string, catalogue []xrefEntry) bool {
	var segs []string
	for seg := range strings.SplitSeq(span, xrefPathSeparator) {
		if norm := normaliseXrefText(seg); norm != "" {
			segs = append(segs, norm)
		}
	}
	return xrefMatch(segs, catalogue, 0, len(catalogue))
}

// xrefMatch resolves segs against catalogue[lo:hi], recursing into the
// section each candidate opens. It backtracks: a segment's text can
// name entries in several sections (two sections can carry the same
// bold lead), and the span resolves if *any* chain of matches nests all
// the way down.
//
// An entry's section runs from just past the entry to the next entry at
// an equal-or-shallower level — the same rule for headings and leads,
// since leads sit below every heading by construction.
func xrefMatch(segs []string, catalogue []xrefEntry, lo, hi int) bool {
	if len(segs) == 0 {
		return true
	}
	for i := lo; i < hi; i++ {
		if !strings.HasPrefix(catalogue[i].text, segs[0]) {
			continue
		}
		end := i + 1
		for end < hi && catalogue[end].level > catalogue[i].level {
			end++
		}
		if xrefMatch(segs[1:], catalogue, i+1, end) {
			return true
		}
	}
	return false
}

// normaliseXrefText canonicalises a citation segment or catalogue entry
// so the two compare on substance. Backticks go (a bare `internal/agent`
// citation should match a backticked bold lead), whitespace runs
// collapse (citations wrap across lines), trailing sentence punctuation
// goes (a citation ending `."` should match a heading without the
// period), and case is folded.
func normaliseXrefText(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, ".,;:")
	return strings.ToLower(strings.TrimSpace(s))
}

// logicalLines splits body into unwrapped logical lines: a blank line
// ends a block, a list item or heading starts (and, for a heading,
// also ends) its own, and any other line joins the block in progress
// with a single space.
//
// Citations and bold leads both wrap across source lines, so matching
// per physical line misses most of them. Plain paragraph-unwrap isn't
// enough either — consecutive list items have no blank line between
// them, so paragraph-joining merges a whole `Decisions` list into one
// line and hides every bold lead but the first.
func logicalLines(body string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "#"):
			// Headings stand alone: an unblanked line after a heading is
			// body text, not part of the heading.
			flush()
			out = append(out, line)
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
			flush()
			cur.WriteString(line)
		case cur.Len() > 0:
			cur.WriteString(" ")
			cur.WriteString(trimmed)
		default:
			cur.WriteString(line)
		}
	}
	flush()
	return out
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
