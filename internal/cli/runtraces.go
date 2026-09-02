package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/wiki"
)

// A run leaves traces beyond its stage canvases: followups.md entries
// that harvest into idea runs, feedback/lore.md entries that promote to
// lore/<slug>.md, and feedback/twin.md entries that harvest into idea
// runs of their own. This file gathers all three for the run page,
// resolving each landed trace to the thing it became.
//
// Every link edge is derived on read rather than written forward: a
// harvested checklist line already carries the resolved slug — which
// *is* the promoted idea's run ID / the promoted lore file's name — so
// each join is an O(1) lookup.

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

	for line := range strings.SplitSeq(string(body), "\n") {
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

// GatherRunTraces backs serve.Options.GatherRunTraces: the run page's
// followups / lore / twin sections for one run. Lives here because
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

	twin, err := readDisplayChecklist(root, run.FeedbackPath(projectID, runID, "twin"))
	if err != nil {
		return serve.RunTraces{}, err
	}
	for _, e := range twin {
		t := traceOf(e)
		// A harvested twin note's resolved slug is an idea run in this
		// project — parseTwinFeedback rejects a cross-project prefix, so
		// there is no other project to look in.
		if e.done && e.slug != "" {
			if md, err := run.Load(root, projectID, e.slug); err == nil {
				t.TargetURL = "/run/" + projectID + "/" + e.slug
				t.TargetStatus = md.Status
			}
		}
		out.Twin = append(out.Twin, t)
	}
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
