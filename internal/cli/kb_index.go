package cli

import (
	"regexp"
	"strings"
)

// Shape of knowledge/index.md: a top-level `# knowledge` heading,
// `## <category>` sections, one `- [title](path) — hook` bullet per
// shelved topic. Order is insertion order — shelve never re-sorts.
//
// The patcher does three jobs in one pass:
//
//   1. If an existing bullet names this topic (regardless of category),
//      remove it.
//   2. Append the fresh bullet under the target category's section,
//      creating the section at the end of the file if it doesn't exist.
//   3. Leave everything else — other categories, other bullets, blank
//      lines between sections — untouched.
//
// The patcher intentionally leaves an emptied `## <category>` heading
// behind when its last bullet moves away. Rebalancing the shelf as a
// whole is out of scope; the heading will get a new bullet next time
// something lands there.

// emptyIndexTemplate is what the patcher produces when applied to an
// empty index.md. Written out verbatim so a freshly shelved project
// gets the canonical shape on its first turn.
const emptyIndexTemplate = "# knowledge\n"

// bulletPrefix matches a `- [anything](path/<topic>.md) — ...` line,
// where <topic> is the run slug we're filing under. Used to detect
// and remove the current topic's existing bullet regardless of
// category or title drift. The path component matches non-whitespace
// up to `/<topic>.md` so we can find the old filing location too —
// though we capture that separately in findExistingBulletPath.
var bulletTopicPattern = regexp.MustCompile(`^- \[[^\]]*\]\(([^)]+)\) `)

// applyIndexPatch takes the current index body (may be empty),
// removes any existing bullet for `topic`, and inserts `newBullet`
// under `## <category>`. Returns the patched body. Idempotent: if the
// inputs produce a body identical to the current one, the output is
// bit-for-bit equal.
func applyIndexPatch(body, topic, category, newBullet string) string {
	if strings.TrimSpace(body) == "" {
		body = emptyIndexTemplate
	}
	body = removeTopicBullet(body, topic)
	body = upsertBulletUnderCategory(body, category, newBullet)
	return body
}

// findExistingBulletPath returns the path from the current index's
// bullet for topic, or "" if no such bullet exists. Used by runShelve
// to know which old file to rm on a category change. The path is
// whatever the bullet's link target was — typically
// `<old-category>/<topic>.md`. No validation; caller is responsible
// for sanity-checking before rm.
func findExistingBulletPath(body, topic string) string {
	suffix := "/" + topic + ".md"
	for _, line := range strings.Split(body, "\n") {
		m := bulletTopicPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if strings.HasSuffix(m[1], suffix) {
			return m[1]
		}
	}
	return ""
}

// removeTopicBullet drops any bullet whose link target ends with
// `/<topic>.md`. Surrounding blank lines are left alone — the shape
// of the rest of the document is the caller's contract to keep.
func removeTopicBullet(body, topic string) string {
	suffix := "/" + topic + ".md"
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		m := bulletTopicPattern.FindStringSubmatch(line)
		if m != nil && strings.HasSuffix(m[1], suffix) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// upsertBulletUnderCategory appends bullet under the `## <category>`
// heading, creating the section at the end of the file when it
// doesn't exist. Matches a heading by case-insensitive comparison of
// the visible category text, so the model's display casing drives the
// name while existing shelves stay stable.
//
// Insertion point within an existing section: after the last
// consecutive bullet line (or the heading itself, if the section has
// no bullets yet). This preserves the insertion-order invariant —
// newest bullet ends up at the bottom of its section.
func upsertBulletUnderCategory(body, category, bullet string) string {
	body = ensureTrailingNewline(body)
	lines := strings.Split(body, "\n")
	// strings.Split on a trailing-newline string produces a phantom ""
	// final element. Drop it so line indexes line up with visible rows,
	// then put it back before joining.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	headingIdx := findCategoryHeading(lines, category)
	if headingIdx < 0 {
		// New category: append a blank line (if needed), the heading,
		// a blank line, and the bullet. Preserves the visual shape of
		// the existing index.
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "## "+category, "", bullet)
		return strings.Join(lines, "\n") + "\n"
	}

	// Walk from heading to end-of-section (next `## ` or end of file),
	// remembering the last bullet we saw. If we saw one, insert the
	// new bullet right after it; otherwise insert after the heading's
	// blank-line separator.
	lastBullet := -1
	sectionEnd := len(lines)
	for i := headingIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			sectionEnd = i
			break
		}
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
			lastBullet = i
		}
	}
	insertAt := lastBullet + 1
	if lastBullet < 0 {
		// No existing bullets — insert just before sectionEnd, trimming
		// back past trailing blank lines in the section so we don't
		// leave two blank lines between the heading and the bullet.
		insertAt = sectionEnd
		for insertAt > headingIdx+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
			insertAt--
		}
		// After the heading there should be one blank line, then the
		// bullet. If that blank line isn't there, add it.
		if insertAt == headingIdx+1 {
			lines = insertAt_(lines, insertAt, "", bullet)
		} else {
			lines = insertAt_(lines, insertAt, bullet)
		}
	} else {
		lines = insertAt_(lines, insertAt, bullet)
	}
	return strings.Join(lines, "\n") + "\n"
}

// findCategoryHeading returns the line index of the `## <category>`
// heading matching category case-insensitively, or -1 if none
// matches.
func findCategoryHeading(lines []string, category string) int {
	want := strings.ToLower(strings.TrimSpace(category))
	for i, line := range lines {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		got := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
		if got == want {
			return i
		}
	}
	return -1
}

// insertAt_ splices new lines into slice at index i. Named with a
// trailing underscore to sidestep the stdlib's slices.Insert — this
// package still targets the Go 1.20-ish baseline the rest of the
// module uses, so we don't import slices.
func insertAt_(s []string, i int, items ...string) []string {
	if i < 0 {
		i = 0
	}
	if i > len(s) {
		i = len(s)
	}
	out := make([]string, 0, len(s)+len(items))
	out = append(out, s[:i]...)
	out = append(out, items...)
	out = append(out, s[i:]...)
	return out
}

func ensureTrailingNewline(s string) string {
	if s == "" {
		return s
	}
	if !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}
