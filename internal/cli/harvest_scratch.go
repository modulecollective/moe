package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

type checklistEntry struct {
	lineIdx   int
	slug      string
	promoteTo string
	title     string
	body      string
}

// atxHeadingRE matches a markdown ATX heading (`# ` … `###### `). A
// heading like "# Follow-ups" is legitimate scaffolding the agent or
// the editor-pop wrote, not lost content, so the stray-content guard
// below skips it.
var atxHeadingRE = regexp.MustCompile(`^#{1,6}\s`)

// parseChecklist scans a followups/lore scratch file into its raw lines
// plus the unchecked entries to harvest. Beyond the per-line validation
// (malformed checkbox, empty title, duplicate slug — each fatal with a
// 1-based line number), it carries a whole-file backstop: if the scan
// finds zero open entries but at least one *substantive* line — prose or
// plain bullets, the shape an agent reaches for when it forgets the
// `- [ ]` grammar — it fails loud instead of returning a clean empty
// result that would silently drop the content. Headings and the
// multi-line `<!-- … -->` editor-pop header are scaffolding, not
// content, so they don't trip the guard; an absent / header-only /
// heading-only / all-`[x]` file stays a clean no-op.
func parseChecklist(body []byte, noun, slugNoun string) (lines []string, todo []checklistEntry, err error) {
	lines = strings.Split(string(body), "\n")
	seen := map[string]int{}

	openIdx := -1
	var bodyLines []string

	// strayIdx remembers the first substantive line dropped without ever
	// attaching to a checkbox. inComment tracks the editor-pop header so
	// its lines are never counted as stray.
	strayIdx := -1
	inComment := false

	finalize := func() {
		if openIdx >= 0 {
			todo[openIdx].body = trimAndDedentBody(bodyLines)
			openIdx = -1
		}
		bodyLines = nil
	}

	for i, line := range lines {
		// Track the HTML comment block (the editor-pop header) so its
		// lines don't read as stray content. Computed before the branches
		// below and consulted only by the stray check; a marker only ever
		// rides a non-blank line, but tracking every line keeps an
		// indented `<!--` from slipping through.
		lineInComment := inComment
		if !inComment && strings.Contains(line, "<!--") {
			lineInComment = true
			inComment = true
		}
		if inComment && strings.Contains(line, "-->") {
			inComment = false
		}

		if followupCheckboxRE.MatchString(line) {
			finalize()
			if followupDoneRE.MatchString(line) {
				continue
			}
			m := followupOpenRE.FindStringSubmatch(line)
			if m == nil {
				return nil, nil, fmt.Errorf("line %d: malformed %s %q (expected: - [ ] `slug` — Title)", i+1, noun, line)
			}
			slug := m[2]
			promoteTo := m[3]
			title := strings.TrimSpace(m[4])
			if title == "" {
				return nil, nil, fmt.Errorf("line %d: %s title is empty", i+1, noun)
			}
			if prev, dup := seen[slug]; dup {
				return nil, nil, fmt.Errorf("line %d: %s slug %q duplicates line %d", i+1, slugNoun, slug, prev+1)
			}
			seen[slug] = i
			todo = append(todo, checklistEntry{lineIdx: i, slug: slug, promoteTo: promoteTo, title: title})
			openIdx = len(todo) - 1
			continue
		}
		if line == "" || isIndentedBody(line) {
			if openIdx >= 0 {
				bodyLines = append(bodyLines, line)
			}
			continue
		}
		finalize()
		// A non-blank, non-indented, non-checkbox line dropped here.
		// Headings and the editor-pop comment are scaffolding; anything
		// else is content written in the wrong shape — remember the first
		// one so the post-scan guard can fail loud if nothing valid
		// parsed.
		if strayIdx < 0 && !lineInComment && !atxHeadingRE.MatchString(line) {
			strayIdx = i
		}
	}
	finalize()
	if len(todo) == 0 && strayIdx >= 0 {
		return nil, nil, fmt.Errorf("has content but no `- [ ]` checklist entries — did the %s get written in the wrong format? (expected: - [ ] `slug` — Title)", noun)
	}
	return lines, todo, nil
}

type scratchItem[T any] struct {
	lineIdx int
	slug    string
	entry   T
}

type scratchHarvestSpec[T any] struct {
	relPath         string
	header          string
	parse           func([]byte) ([]string, []scratchItem[T], error)
	write           func(T) (string, error)
	progressSubject string
	progressPaths   []string
	// writeErrorLeavesProgress identifies writer errors returned after
	// an on-disk mutation. Lore promotion can fail while deleting
	// superseded entries after it has written the replacement.
	writeErrorLeavesProgress func(error) bool
	writeErrPrefix           string
}

func harvestScratchTyped[T any](root, projectID, runID, workflow string, skipEdit bool, spec scratchHarvestSpec[T]) error {
	absPath := filepath.Join(root, spec.relPath)

	if !skipEdit && hasUncheckedEntry(absPath) {
		if err := injectEditorPopHeader(absPath, spec.header); err != nil {
			return err
		}
		if err := launchEditorOrFail(absPath); err != nil {
			return err
		}
	}

	body, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", spec.relPath, err)
	}

	lines, todo, err := spec.parse(body)
	if err != nil {
		return fmt.Errorf("%s: %w", spec.relPath, err)
	}
	if len(todo) == 0 {
		return nil
	}

	for hi, item := range todo {
		resolved, werr := spec.write(item.entry)
		if werr != nil {
			leavesProgress := spec.writeErrorLeavesProgress != nil && spec.writeErrorLeavesProgress(werr)
			if hi > 0 || leavesProgress {
				if perr := commitScratchProgress(root, projectID, runID, workflow, spec, lines); perr != nil {
					return fmt.Errorf("%s %s (then progress commit failed: %v): %w", spec.writeErrPrefix, item.slug, perr, werr)
				}
			}
			return fmt.Errorf("%s %s (after harvesting %d): %w", spec.writeErrPrefix, item.slug, hi, werr)
		}
		marked, ok := markHarvested(lines[item.lineIdx], item.slug, resolved)
		if !ok {
			// Unreachable in normal operation: markHarvested rewrites via
			// the same regex that parsed this line moments ago, and only
			// *other* indices are mutated in between. This is an invariant
			// guard, not a recovery path — the idea/lore file already
			// exists, so fail loud (naming the file + 1-based line) rather
			// than leave a silent `- [ ]` a later harvest would re-promote.
			// Commit the lines marked so far through the same partial-failure
			// machinery a write error uses.
			if hi > 0 {
				if perr := commitScratchProgress(root, projectID, runID, workflow, spec, lines); perr != nil {
					return fmt.Errorf("%s: harvested %q but could not mark line %d %q (then progress commit failed: %v)", spec.relPath, item.slug, item.lineIdx+1, lines[item.lineIdx], perr)
				}
			}
			return fmt.Errorf("%s: harvested %q but could not mark line %d %q", spec.relPath, item.slug, item.lineIdx+1, lines[item.lineIdx])
		}
		lines[item.lineIdx] = marked
	}

	if err := os.WriteFile(absPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", spec.relPath, err)
	}
	return nil
}

func commitScratchProgress[T any](root, projectID, runID, workflow string, spec scratchHarvestSpec[T], lines []string) error {
	if err := os.WriteFile(filepath.Join(root, spec.relPath), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write progress %s: %w", spec.relPath, err)
	}
	msg := spec.progressSubject + "\n\n" +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
		}.String()
	paths := append([]string{spec.relPath}, spec.progressPaths...)
	return run.StageAndCommit(root, msg, paths...)
}
