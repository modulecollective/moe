package md

import (
	"path"
	"regexp"
	"strings"
)

// This file is the corpus-analysis half of the package: the helpers that
// read a doc's link topology and emptiness rather than rendering it.
// Both structural scans in the bureaucracy — the twin's closed-schema
// hygiene pass (internal/wiki) and the knowledge tree's
// (internal/knowledge) — resolve links and detect stubs the same way, so
// the rules live here once instead of once per corpus.

// linkPattern matches `[text](target)` markdown inline links
// permissively. The capture is the target as authored — callers
// canonicalise from there. Reference-style links and image embeds are
// not in scope; the bureaucracy's own docs use inline links.
var linkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)`)

// LocalLinks returns the targets of every same-repo `.md` link in body,
// in document order, including duplicates. External URLs, mailto:, and
// bare anchors are filtered out — only links that name a file the
// caller can check for existence survive.
//
// Anchors on a local target ("topic.md#heading") qualify: the file
// portion is what gets validated, and ResolveLink strips the fragment.
func LocalLinks(body string) []string {
	matches := linkPattern.FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 && isLocalLink(m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// isLocalLink reports whether link is a same-repo .md reference.
func isLocalLink(link string) bool {
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
	target := link
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	return strings.HasSuffix(target, ".md")
}

// ResolveLink canonicalises a markdown link target into a path relative
// to the corpus root, given the file the link appears in (also
// root-relative). Fragments are stripped; "./" and ".." segments resolve
// against the link source's directory, so a doc linking to a sibling
// ("other.md") and one linking up ("../index.md") both produce paths
// that match the caller's catalogue keys.
func ResolveLink(link, fromRel string) string {
	target := link
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target = target[:i]
	}
	// path.Clean uses forward slashes throughout — markdown link targets
	// are always slash-separated, and catalogue keys use slashes too, so
	// resolution stays in one namespace regardless of host OS.
	return path.Clean(path.Join(path.Dir(fromRel), target))
}

// IsEffectivelyEmpty reports whether body has no meaningful content.
// A zero-byte file is empty; a file with only whitespace is empty; a
// file with only a title heading (one `# Title` line) and nothing else
// is a stub. Anything past the title — even one paragraph — counts as
// content.
func IsEffectivelyEmpty(body string) bool {
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
