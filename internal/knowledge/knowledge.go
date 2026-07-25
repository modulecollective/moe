// Package knowledge is the structural hygiene scan over a project's
// `projects/<p>/knowledge/` doc tree.
//
// The tree is a plain doc tree, not an engine-backed wiki: `index.md` at
// the top as the catalog, one file per topic flat under `topics/`. Any
// sdlc stage may write it (see projectCommitDirs), which is what this
// scan exists for — the checks that used to run as the kb workflow's
// lint pre-scan now gate the close of the turn that did the writing, so
// the agent that broke the index fixes it in the same context.
//
// Deliberately narrow: only deterministic, file-shape problems land
// here. Whether a topic is well-written, well-scoped, or worth keeping
// is prose discipline, not something a scan can judge.
package knowledge

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/md"
)

// topicsSubdir is the basename of the directory under the knowledge dir
// that holds topic docs, so the corpus catalog (index.md) sits clean
// above the corpus itself.
const topicsSubdir = "topics"

// indexName is the corpus catalog at the top of the knowledge dir.
const indexName = "index.md"

// Findings is the scan result. Each field is a sorted list so a rendered
// report is deterministic.
type Findings struct {
	// Orphans are topic docs present on disk but not referenced from
	// index.md. Paths are relative to the knowledge dir (e.g.
	// "topics/dns.md").
	Orphans []string
	// MissingFromIndex are paths named in index.md links that don't
	// resolve to a topic doc on disk, as the as-written link target
	// resolves it (an entry "[X](topics/missing.md)" surfaces as
	// "topics/missing.md").
	MissingFromIndex []string
	// BrokenLinks are internal links from topic docs that point at files
	// that don't exist.
	BrokenLinks []BrokenLink
	// EmptyDocs are docs with no meaningful content — zero-byte,
	// whitespace-only, or just a title heading.
	EmptyDocs []string
}

// BrokenLink is one cross-link a topic doc makes that doesn't resolve.
type BrokenLink struct {
	From   string // doc containing the link, relative to the knowledge dir
	Target string // path the link resolves to, relative to the knowledge dir
}

// IsEmpty reports whether the scan found nothing.
func (f Findings) IsEmpty() bool { return f.Count() == 0 }

// Count is the rolled-up finding total across categories, for the
// one-line "found N issues" summary a caller prints above the report.
func (f Findings) Count() int {
	return len(f.Orphans) + len(f.MissingFromIndex) + len(f.BrokenLinks) + len(f.EmptyDocs)
}

// Scan walks dir — a project's knowledge directory — and returns the
// structural findings. A missing dir (or missing topics/ subdir) is not
// an error: it produces empty findings, because a project with no
// knowledge tree has nothing to get wrong.
//
// Best-effort per file: a doc the scan can't read becomes an absence in
// the orphan / link checks rather than a hard error. Errors that would
// prevent any scan from completing propagate.
//
// The catalogue is keyed by path relative to dir (e.g. "topics/dns.md")
// so it matches index.md link targets verbatim, and so a topic doc using
// a flat sibling reference like [other](other.md) resolves correctly
// relative to its own directory.
func Scan(dir string) (Findings, error) {
	var f Findings

	topics := filepath.Join(dir, topicsSubdir)
	entries, err := os.ReadDir(topics)
	if err != nil && !os.IsNotExist(err) {
		return f, fmt.Errorf("knowledge: read %s: %w", topics, err)
	}

	known := map[string]bool{}
	var topicList []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		rel := path.Join(topicsSubdir, e.Name())
		known[rel] = true
		topicList = append(topicList, rel)
	}

	indexBody, indexExists, err := readMaybe(filepath.Join(dir, indexName))
	if err != nil {
		return f, err
	}
	indexed := map[string]bool{}
	for _, link := range md.LocalLinks(indexBody) {
		canon := md.ResolveLink(link, indexName)
		indexed[canon] = true
		if !known[canon] {
			f.MissingFromIndex = append(f.MissingFromIndex, canon)
		}
	}

	// Orphans are only meaningful when index.md exists. Without one,
	// every topic doc would be flagged, which says nothing the operator
	// doesn't already know.
	if indexExists {
		for _, t := range topicList {
			if !indexed[t] {
				f.Orphans = append(f.Orphans, t)
			}
		}
	}

	for _, t := range topicList {
		body, _, err := readMaybe(filepath.Join(dir, t))
		if err != nil {
			return f, err
		}
		if md.IsEffectivelyEmpty(body) {
			f.EmptyDocs = append(f.EmptyDocs, t)
		}
		for _, link := range md.LocalLinks(body) {
			canon := md.ResolveLink(link, t)
			if known[canon] || canon == indexName {
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

// Render formats f as a markdown block the caller can print verbatim or
// splice into a prompt. Returns "" when f is empty so callers can drop
// the heading on a clean tree.
func Render(f Findings) string {
	if f.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Knowledge tree findings\n\n")
	if len(f.Orphans) > 0 {
		b.WriteString("**Topic docs missing from index.md**:\n")
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
	return b.String()
}

// readMaybe reads path, returning ("", false, nil) when the file is
// absent so callers can branch on existence without two filesystem
// trips. Other I/O errors propagate.
func readMaybe(p string) (string, bool, error) {
	body, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("knowledge: read %s: %w", p, err)
	}
	return string(body), true, nil
}
