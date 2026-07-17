package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
)

// pullNextLineRE matches one Pull next entry — the house checklist
// grammar minus the checkbox: `- \`slug\` — reason`. Backtick-wrapped
// slug in run-id shape, em-dash (U+2014) separator, terse reason. The
// parse is lenient by contract (a malformed line is skipped, not an
// error), so this only recognises the well-formed shape and lets the
// section scanner drop everything else.
var pullNextLineRE = regexp.MustCompile("^\\s*-\\s+`([a-z0-9][a-z0-9-]*)`\\s+—\\s*(\\S.*?)\\s*$")

// atxHeadingLineRE matches any ATX heading and captures its text, so
// the section scanner can tell when the `## Pull next` block starts
// (heading text is "Pull next") and when it ends (any other heading).
var atxHeadingLineRE = regexp.MustCompile(`^\s*#{1,6}\s+(.*?)\s*$`)

// parsePullNext extracts the ranked backlog picks from a pulse survey
// canvas. It finds the `## Pull next` section and returns its
// well-formed `- \`slug\` — reason` entries in file order (which is
// rank order). Lenient throughout: a missing section, a missing file,
// or a malformed line yields no pick rather than an error — the
// consumer is a render, and a stale or half-written report must never
// break the dash. Picks carry no Project (the caller stamps it).
func parsePullNext(content []byte) []dash.PullNextPick {
	lines := strings.Split(string(content), "\n")
	var picks []dash.PullNextPick
	inSection := false
	for _, line := range lines {
		if m := atxHeadingLineRE.FindStringSubmatch(line); m != nil {
			// A heading ends the Pull next section (and starts it when it
			// names "pull next"). Comparing the trimmed, lowered heading
			// text keeps the match tolerant of extra spacing.
			inSection = strings.EqualFold(strings.TrimSpace(m[1]), "Pull next")
			continue
		}
		if !inSection {
			continue
		}
		if m := pullNextLineRE.FindStringSubmatch(line); m != nil {
			picks = append(picks, dash.PullNextPick{Slug: m[1], Reason: strings.TrimSpace(m[2])})
		}
	}
	return picks
}

// gatherPullNext resolves each project's latest pulse run (any status —
// a closed sweep's report stays the latest word until the next pulse),
// reads its survey canvas, and parses the Pull next picks. Returns a
// flat slice in a deterministic order (projects sorted, picks in report
// order) so the dash render is stable. dash.BuildRows does the
// intersection with still-open ideas and the reorder.
func gatherPullNext(root string, mds []*run.Metadata, idx *run.JournalIndex) []dash.PullNextPick {
	latest := map[string]*run.Metadata{}
	for _, md := range mds {
		if md.Workflow != pulseWorkflow {
			continue
		}
		cur := latest[md.Project]
		if cur == nil || idx.LastActivity[md.Project+"/"+md.ID].After(idx.LastActivity[cur.Project+"/"+cur.ID]) {
			latest[md.Project] = md
		}
	}
	projects := make([]string, 0, len(latest))
	for project := range latest {
		projects = append(projects, project)
	}
	sort.Strings(projects)

	var picks []dash.PullNextPick
	for _, project := range projects {
		md := latest[project]
		content, err := os.ReadFile(filepath.Join(root, run.ContentPath(project, md.ID, pulseDoc)))
		if err != nil {
			continue // no readable canvas → no picks for this project
		}
		for _, p := range parsePullNext(content) {
			p.Project = project
			picks = append(picks, p)
		}
	}
	return picks
}
