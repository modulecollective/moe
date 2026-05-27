package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

type checklistEntry struct {
	lineIdx int
	slug    string
	title   string
	body    string
}

func parseChecklist(body []byte, noun, slugNoun string) (lines []string, todo []checklistEntry, err error) {
	lines = strings.Split(string(body), "\n")
	seen := map[string]int{}

	openIdx := -1
	var bodyLines []string

	finalize := func() {
		if openIdx >= 0 {
			todo[openIdx].body = trimAndDedentBody(bodyLines)
			openIdx = -1
		}
		bodyLines = nil
	}

	for i, line := range lines {
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
			title := strings.TrimSpace(m[3])
			if title == "" {
				return nil, nil, fmt.Errorf("line %d: %s title is empty", i+1, noun)
			}
			if prev, dup := seen[slug]; dup {
				return nil, nil, fmt.Errorf("line %d: %s slug %q duplicates line %d", i+1, slugNoun, slug, prev+1)
			}
			seen[slug] = i
			todo = append(todo, checklistEntry{lineIdx: i, slug: slug, title: title})
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
	}
	finalize()
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
	markLine        func(line, baseSlug, resolvedSlug string) string
	writeErrPrefix  string
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
			if hi > 0 {
				if perr := commitScratchProgress(root, projectID, runID, workflow, spec, lines); perr != nil {
					return fmt.Errorf("%s %s (then progress commit failed: %v): %w", spec.writeErrPrefix, item.slug, perr, werr)
				}
			}
			return fmt.Errorf("%s %s (after harvesting %d): %w", spec.writeErrPrefix, item.slug, hi, werr)
		}
		lines[item.lineIdx] = spec.markLine(lines[item.lineIdx], item.slug, resolved)
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
