package wiki

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoreDirRel is the bureaucracy-root-relative path where cross-project
// "lore" lives: portable, hard-won operational facts that don't belong
// to any one project. Sibling of projects/ and .moe/.
const LoreDirRel = "lore"

// LoreSoftCap is the per-bureaucracy count past which the injected
// catalog carries a "consider pruning" warning. Soft — the block still
// renders fully; the cap nudges, doesn't gate.
const LoreSoftCap = 20

// LoreDir returns the absolute path to the bureaucracy's lore/ dir.
func LoreDir(root string) string {
	return filepath.Join(root, LoreDirRel)
}

// loreEntry is one parsed entry from lore/. Title and AppliesWhen come
// from YAML frontmatter; Filename is the leaf name relative to lore/.
type loreEntry struct {
	Filename    string
	Title       string
	AppliesWhen string
}

// LoreReferenceSection emits a system-prompt block that catalogs the
// bureaucracy's lore/ entries: one line per entry with title and
// "applies-when" hint. The agent opens a specific file only when its
// hint matches the current task — bodies stay on disk, not in the
// prompt budget.
//
// Mirrors TwinReferenceSection's shape. Returns "" when lore/ doesn't
// exist or has zero entries, so the empty case slots cleanly into the
// section join in buildSystemPrompt.
func LoreReferenceSection(cfg Config) string {
	return LoreReferenceSectionAt(cfg.BureaucracyPath)
}

// LoreReferenceSectionAt is the path-driven variant of
// LoreReferenceSection. Useful for callers (stage_prompt) that don't
// hold a wiki Config.
func LoreReferenceSectionAt(root string) string {
	if root == "" {
		return ""
	}
	dir := LoreDir(root)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	raw, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var entries []loreEntry
	for _, e := range raw {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		// Hidden / underscore-prefixed files are escape hatches for
		// drafts and stash entries — skip them so the operator can
		// stage a file without it landing in every prompt.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		title, appliesWhen := readLoreFrontmatter(filepath.Join(dir, name))
		if title == "" {
			title = strings.TrimSuffix(name, ".md")
		}
		if appliesWhen == "" {
			appliesWhen = "(missing)"
		}
		entries = append(entries, loreEntry{
			Filename:    name,
			Title:       title,
			AppliesWhen: appliesWhen,
		})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Filename < entries[j].Filename
	})

	var b strings.Builder
	b.WriteString("## Lore (cross-project)\n\n")
	fmt.Fprintf(&b, "The bureaucracy carries a small set of portable, hard-won facts under\n  %s\n\n", dir)
	b.WriteString(`Each entry is one fact discovered in one project but true across many.
Open a file only when its "applies when" hint matches the task you're
doing — the bodies stay on disk; this catalog is the budgeted summary.

`)
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s](%s) — %s\n",
			e.Title, filepath.Join(dir, e.Filename), e.AppliesWhen)
	}
	b.WriteString("\nIf you discover a portable fact worth adding to this catalog, leave\nan entry via the `moe-bureaucracy` skill.\n")
	if len(entries) > LoreSoftCap {
		fmt.Fprintf(&b, "\n> ⚠ %d lore entries (soft cap %d) — consider pruning or splitting.\n",
			len(entries), LoreSoftCap)
	}
	return b.String()
}

// readLoreFrontmatter pulls `title:` and `applies-when:` out of a
// lore entry's YAML frontmatter. Fail-soft: any I/O error or malformed
// frontmatter yields ("", "") and lets the caller fall back to the
// filename + "(missing)" placeholders — a half-written entry shouldn't
// break every stage prompt.
//
// Stdlib only: we read just enough lines to clear the second `---`
// fence, scan for the two keys, and stop. No YAML parser needed for a
// fixed two-field schema.
func readLoreFrontmatter(path string) (title, appliesWhen string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Cap line length so a misshapen file doesn't blow the buffer.
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)

	if !sc.Scan() {
		return "", ""
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, val, ok := splitFrontmatterLine(line)
		if !ok {
			continue
		}
		switch key {
		case "title":
			title = val
		case "applies-when":
			appliesWhen = val
		}
	}
	return title, appliesWhen
}

// splitFrontmatterLine splits "key: value" into (key, value, true).
// Strips surrounding whitespace and matched quotes around the value.
// Returns ok=false for lines that don't look like a key/value pair.
func splitFrontmatterLine(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	value = strings.TrimSpace(line[i+1:])
	if key == "" {
		return "", "", false
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}
