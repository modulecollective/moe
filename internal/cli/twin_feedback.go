package cli

import (
	"fmt"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// feedback/twin.md is the run-scoped scratch file for twin edits an
// agent can see but shouldn't make. It used to be a mailbox: notes
// accumulated until the next `moe twin reflect` pass read them as
// kickoff context. With the twin an ordinary sdlc write target, a note
// worth acting on is worth scheduling, so the channel harvests into
// ideas at terminal — the third pipeline beside followups and lore,
// built on the same grammar and the same parser.
//
//	- [ ] `architecture-owns-clone-gc` (sdlc) — architecture.md still says reflect owns clone gc
//
//	  operations.md §"Sandbox clones" moved gc to `moe clone gc` in
//	  2026-07; architecture's component list never caught up.
//
// Each unchecked entry becomes one idea whose canvas opens with a
// provenance line naming the source run and the dir acting on it would
// edit. The tag rule is the followups rule verbatim: `(sdlc)` when the
// fix is mechanical, bounded, and verifiable; untagged means operator
// triage. Twin-ness lives in the idea's body, not in a tag —
// validatePromoteTag accepts only staged, chainable workflows, and
// there is no twin workflow to name.

// twinFeedbackHeader is the editor-pop banner auto-injected onto
// feedback/twin.md before $EDITOR opens. Third of the trio with
// followupsHeader and loreHeader, same convention: HTML comment,
// file-specific phrasing, closing "remove freely" line.
const twinFeedbackHeader = `<!--
feedback/twin.md — digital-twin edits spotted this run but left undone.
Save this file to spin each unchecked ` + "`- [ ]`" + ` entry into a new idea
run. Delete the line to skip. Lines marked ` + "`- [x]`" + ` are already
promoted; leave them alone.
This header is auto-injected on editor pop; remove it freely.
-->`

// parsedTwinNote is one harvest candidate plucked from feedback/twin.md.
type parsedTwinNote struct {
	lineIdx   int
	slug      string
	promoteTo string
	title     string
	body      string
}

// parseTwinFeedback scans body into raw lines plus the unchecked
// entries to harvest. Same contract as parseFollowups and parseLore:
// upfront, total validation with 1-based line numbers, and the shared
// stray-content backstop that fails loud on a file written in the wrong
// shape rather than silently dropping it.
//
// Narrower than followups in one respect: parseChecklist's shared regex
// permits a `<project>/` slug prefix for cross-project followup
// routing, and a twin note has nowhere to route — it is an observation
// about *this* project's twin, and the provenance line the harvest
// writes says so. Reject the prefix here rather than fork a parser.
func parseTwinFeedback(body []byte) (lines []string, todo []parsedTwinNote, err error) {
	lines, entries, err := parseChecklist(body, "twin observation", "twin observation")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if strings.Contains(e.slug, "/") {
			return nil, nil, fmt.Errorf("line %d: twin observation slug must not contain '/' (a twin note is about this project's twin)", e.lineIdx+1)
		}
		if e.promoteTo != "" {
			if err := validatePromoteTag(e.promoteTo); err != nil {
				return nil, nil, fmt.Errorf("line %d: twin observation %w", e.lineIdx+1, err)
			}
		}
		todo = append(todo, parsedTwinNote{
			lineIdx:   e.lineIdx,
			slug:      e.slug,
			promoteTo: e.promoteTo,
			title:     e.title,
			body:      e.body,
		})
	}
	return lines, todo, nil
}

// twinNoteProvenance is the line every harvested twin idea opens with.
// It is what carries twin-ness now that no tag can: an operator
// triaging the backlog, or a design stage picking the idea up weeks
// later, learns from the first line both where the observation came
// from and which tree acting on it edits.
func twinNoteProvenance(projectID, runID string) string {
	return fmt.Sprintf("Twin observation from run `%s/%s`. Acting on it edits `projects/%s/digital-twin/`.",
		projectID, runID, projectID)
}

// harvestTwinFeedback is the twin counterpart of harvestFollowups.
// Same flow, same idempotency, same destination (an idea run) — the
// only differences are the header, the provenance line prepended to
// each seed canvas, and the narrower slug grammar.
func harvestTwinFeedback(root, projectID, runID, workflow string, skipEdit bool) error {
	relPath := run.FeedbackPath(projectID, runID, "twin")
	openTrailers := trailers.Block{FromRun: projectID + "/" + runID, Consent: walkConsent()}

	spec := scratchHarvestSpec[parsedTwinNote]{
		relPath:         relPath,
		header:          twinFeedbackHeader,
		progressSubject: fmt.Sprintf("harvest: capture twin observations for %s/%s", projectID, runID),
		writeErrPrefix:  "create idea",
		parse: func(body []byte) ([]string, []scratchItem[parsedTwinNote], error) {
			lines, todo, err := parseTwinFeedback(body)
			if err != nil {
				return nil, nil, err
			}
			items := make([]scratchItem[parsedTwinNote], 0, len(todo))
			for _, n := range todo {
				items = append(items, scratchItem[parsedTwinNote]{lineIdx: n.lineIdx, slug: n.slug, entry: n})
			}
			return lines, items, nil
		},
		write: func(n parsedTwinNote) (string, error) {
			canvasBody := fmt.Sprintf("# %s\n\n%s\n", n.title, twinNoteProvenance(projectID, runID))
			if n.body != "" {
				canvasBody += "\n" + n.body + "\n"
			}
			md, err := createIdea(root, projectID, n.slug, canvasBody, n.promoteTo, openTrailers)
			if err != nil {
				return "", err
			}
			return md.ID, nil
		},
	}
	return harvestScratchTyped(root, projectID, runID, workflow, skipEdit, spec)
}
