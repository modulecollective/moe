package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/wiki"
)

// A run leaves traces beyond its stage canvases: followups.md entries
// that harvest into idea runs, feedback/lore.md entries that promote to
// lore/<slug>.md, and a feedback/twin.md note a later reflect pass folds
// into the digital twin. This file gathers all three for the run page,
// resolving each landed trace to the thing it became.
//
// Both link edges are derived on read rather than written forward. A
// harvested checklist line already carries the resolved slug — which
// *is* the promoted idea's run ID / the promoted lore file's name — so
// the followup and lore joins are O(1) lookups. Twin notes carry no
// such marker (ingestion is per-file, and nothing marks a file
// consumed), so the reflect attribution replays checkpoint history; see
// twinNoteStatus.

// displayEntry is one checklist line as the run page shows it. A line
// that matched the grammar fills slug/title/body; one that didn't fills
// raw and renders verbatim. Display never fails on a malformed file —
// the opposite posture from parseChecklist, whose job at harvest time
// is to refuse rather than silently drop the operator's intent.
type displayEntry struct {
	done  bool
	slug  string
	title string
	body  string
	raw   string
}

// scanChecklistDisplay is the lenient sibling of parseChecklist: same
// regexes, same body-attachment rule, no validation and no error
// return. It yields every checkbox line in file order — checked and
// unchecked alike, since the checked ones are the whole point of the
// run page's traces sections.
func scanChecklistDisplay(body []byte) []displayEntry {
	var out []displayEntry
	openIdx := -1
	var bodyLines []string

	finalize := func() {
		if openIdx >= 0 {
			out[openIdx].body = trimAndDedentBody(bodyLines)
			openIdx = -1
		}
		bodyLines = nil
	}

	for _, line := range strings.Split(string(body), "\n") {
		if followupCheckboxRE.MatchString(line) {
			finalize()
			done := followupDoneRE.MatchString(line)
			re := followupOpenRE
			if done {
				re = followupDoneCaptureRE
			}
			if m := re.FindStringSubmatch(line); m != nil {
				out = append(out, displayEntry{
					done:  done,
					slug:  m[2],
					title: strings.TrimSpace(m[4]),
				})
				openIdx = len(out) - 1
				continue
			}
			out = append(out, displayEntry{done: done, raw: strings.TrimSpace(line)})
			continue
		}
		if line == "" || isIndentedBody(line) {
			if openIdx >= 0 {
				bodyLines = append(bodyLines, line)
			}
			continue
		}
		// Headings and the editor-pop comment header land here and are
		// dropped — scaffolding, not traces.
		finalize()
	}
	finalize()
	return out
}

// readDisplayChecklist reads a run-scoped checklist file and scans it.
// An absent file is the common case (most runs leave no followups) and
// returns no entries, which renders as no section at all.
func readDisplayChecklist(root, rel string) ([]displayEntry, error) {
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return scanChecklistDisplay(body), nil
}

// checkpointSeal is one historical reflect pass: the LastIngestAt it
// sealed and the run that sealed it.
type checkpointSeal struct {
	sealedAt  time.Time
	ingestRun string
}

// checkpointHistory replays every committed revision of a project's
// digital-twin checkpoint.json, oldest first. Each revision is one
// reflect pass's seal, which is the only record of which pass ingested
// a given twin note — nothing writes that edge forward.
//
// Revisions that don't parse, or that predate the field, are skipped
// rather than fatal: a page that can't attribute one note should say
// "pending", not 500.
func checkpointHistory(root, projectID string) ([]checkpointSeal, error) {
	rel := filepath.Join("projects", projectID, wiki.TwinDirRel, "checkpoint.json")
	out, err := git.Output(root, "log", "--format=%H", "--", rel)
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rel, err)
	}
	var seals []checkpointSeal
	for _, sha := range strings.Fields(out) {
		blob, err := git.Output(root, "show", sha+":"+rel)
		if err != nil {
			continue
		}
		var cp wiki.Checkpoint
		if err := json.Unmarshal([]byte(blob), &cp); err != nil || cp.LastIngestAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, cp.LastIngestAt)
		if err != nil {
			continue
		}
		seals = append(seals, checkpointSeal{sealedAt: t, ingestRun: cp.LastIngestRun})
	}
	slices.Reverse(seals) // git log is newest-first; the walk wants oldest-first
	return seals, nil
}

// twinNoteStatus reports which reflect pass folded a run's twin note
// in, mirroring loadTwinFeedback's inclusion rule exactly: a pass
// ingests every note whose git time doesn't post-date the LastIngestAt
// it seals, so the *earliest* seal at-or-after the note is the pass
// that consumed it.
//
// One carve-out, same as the consumer's: a pass writes its own
// feedback/twin.md in the stage-exit commit that seals the checkpoint,
// so that note can't post-date the threshold it created — yet plainly
// wasn't ingested by the pass that filed it. When the covering seal
// names this run, attribution shifts to the next seal.
//
// reflected=false means pending — no seal covers the note yet, or it
// isn't committed (zero git time), matching loadTwinFeedback's skip.
// reflected=true with an empty run means a pass covered it but didn't
// record which; the page says "folded in" without a link.
func twinNoteStatus(root, projectID, runID string, noteAt time.Time) (reflectRun string, reflected bool, err error) {
	if noteAt.IsZero() {
		return "", false, nil
	}
	seals, err := checkpointHistory(root, projectID)
	if err != nil {
		return "", false, err
	}
	for _, s := range seals {
		if s.sealedAt.Before(noteAt) {
			continue
		}
		if s.ingestRun == runID {
			continue // carried to the next pass
		}
		return s.ingestRun, true, nil
	}
	return "", false, nil
}

// GatherRunTraces backs serve.Options.GatherRunTraces: the run page's
// followups / lore / twin-note sections for one run. Lives here because
// the checklist grammar is unexported cli state and serve can't import
// cli.
//
// Reads the canonical bureaucracy root, so a live run's in-flight
// traces only appear once its stage commits merge back. That's the
// accepted limitation — the sections earn their keep on closed runs,
// where everything has landed.
func GatherRunTraces(root, projectID, runID string) (serve.RunTraces, error) {
	var out serve.RunTraces

	followups, err := readDisplayChecklist(root, run.FollowupsPath(projectID, runID))
	if err != nil {
		return serve.RunTraces{}, err
	}
	for _, e := range followups {
		t := traceOf(e)
		if e.done && e.slug != "" {
			// The resolved slug on a harvested line is the idea run's ID,
			// optionally `<project>/`-prefixed when the followup routed
			// across projects.
			p, slug := projectID, e.slug
			if i := strings.IndexByte(e.slug, '/'); i >= 0 {
				p, slug = e.slug[:i], e.slug[i+1:]
			}
			// A missing target is normal: the operator can check a line by
			// hand to drop it. Render it checked, unlinked.
			if md, err := run.Load(root, p, slug); err == nil {
				t.TargetURL = "/run/" + p + "/" + slug
				t.TargetStatus = md.Status
			}
		}
		out.Followups = append(out.Followups, t)
	}

	lore, err := readDisplayChecklist(root, run.FeedbackPath(projectID, runID, "lore"))
	if err != nil {
		return serve.RunTraces{}, err
	}
	for _, e := range lore {
		t := traceOf(e)
		// Lore slugs are bare filenames (parseLore rejects a `/`), so a
		// prefixed one never promoted and has nothing to link to.
		if e.done && e.slug != "" && !strings.Contains(e.slug, "/") {
			if st, err := os.Stat(filepath.Join(root, wiki.LoreDirRel, e.slug+".md")); err == nil && !st.IsDir() {
				t.TargetURL = "/lore/" + e.slug
			}
		}
		out.Lore = append(out.Lore, t)
	}

	note, err := gatherTwinNote(root, projectID, runID)
	if err != nil {
		return serve.RunTraces{}, err
	}
	out.TwinNote = note
	return out, nil
}

func traceOf(e displayEntry) serve.RunTrace {
	return serve.RunTrace{
		Done:  e.done,
		Raw:   e.raw,
		Slug:  e.slug,
		Title: e.title,
		Body:  e.body,
	}
}

// gatherTwinNote reads a run's feedback/twin.md and dates its
// ingestion. The whole file is one trace: reflect's granularity is the
// file, not the note inside it, so a per-note status would be fiction.
func gatherTwinNote(root, projectID, runID string) (*serve.TwinNoteTrace, error) {
	rel := run.FeedbackPath(projectID, runID, "twin")
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, nil
	}
	when, err := run.LastFileActivity(root, rel)
	if err != nil {
		return nil, fmt.Errorf("git time %s: %w", rel, err)
	}
	reflectRun, reflected, err := twinNoteStatus(root, projectID, runID, when)
	if err != nil {
		return nil, err
	}
	return &serve.TwinNoteTrace{
		Body:       string(body),
		Reflected:  reflected,
		ReflectRun: reflectRun,
	}, nil
}
