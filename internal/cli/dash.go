package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (backlog / runs)",
		Run:     runDash,
	})
}

// dormantCutoff is the staleness threshold for the RUNS section. A
// run with no MoE-Run-scoped commit in this window is considered
// dormant and hidden unless --all is passed. Matches README §"The
// attention filter": "Dormant runs (no activity in 30+ days)
// collapse out of the default view".
const dormantCutoff = 30 * 24 * time.Hour

// completedCap bounds the COMPLETED section. Finished runs are
// history — useful as recent context, not as a backlog — so we show
// the newest N and let the bureaucracy repo itself be the archive.
const completedCap = 10

// bucket labels a row's section. Active runs (next stage to run) and
// completed runs (pushed or terminal) live on different rails from
// backlog ideas, and the operator's eye lands on active work first,
// so the iota order mirrors the on-screen order.
type bucket int

const (
	bucketActiveRuns    bucket = iota // in-progress runs with a next stage
	bucketBacklog                     // captured ideas, not yet promoted to a run
	bucketCompletedRuns               // pushed or terminal runs, shown as "done"
	bucketNone                        // filtered out entirely (dormant without --all)
)

// dashRow is one entry in the dashboard. Kept flat so tabwriter can
// render it without further computation — all the state machinery
// runs up front in buildDashRows.
type dashRow struct {
	project string
	run     string
	note    string    // for runs: next stage name, or "done"; for backlog: idea title.
	when    time.Time // sort key within the section; most recent first.
	bucket  bucket
}

func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "include dormant runs (no activity in 30+ days)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	now := time.Now().UTC()
	rows, err := buildDashRows(root, mds, now, *all)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	projectCount, err := countProjects(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Count active from buckets rather than md.Status so the footer
	// matches what the ACTIVE section actually shows. A KB run past
	// its terminal stage is Status=in_progress on disk but lives in
	// COMPLETED here — counting it as active would mislead.
	activeCount := 0
	for _, r := range rows {
		if r.bucket == bucketActiveRuns {
			activeCount++
		}
	}

	renderDash(stdout, now, rows, projectCount, activeCount)
	return 0
}

// buildDashRows maps scanned metadata to dashboard rows. Per-run
// git queries live here so renderDash stays a pure printer. Ideas are
// just runs with workflow=idea; classify routes open ones to the
// backlog bucket and closed/promoted ones to completed, so there's no
// separate scan of a markdown-file shelf.
func buildDashRows(root string, mds []*run.Metadata, now time.Time, includeDormant bool) ([]dashRow, error) {
	// byRunKey lets the promoted-idea branch resolve a successor run's
	// workflow from its MoE-Promoted-To trailer (`<project>/<id>`)
	// without a second disk scan.
	byRunKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byRunKey[md.Project+"/"+md.ID] = md
	}
	rows := make([]dashRow, 0, len(mds))
	for _, md := range mds {
		last, err := run.LastActivity(root, md.ID)
		if err != nil {
			return nil, err
		}
		b, note, err := classify(root, md, last, now, includeDormant, byRunKey)
		if err != nil {
			return nil, err
		}
		if b == bucketNone {
			continue
		}
		rows = append(rows, dashRow{
			project: md.Project,
			run:     md.ID,
			note:    note,
			when:    last,
			bucket:  b,
		})
	}
	// Within a section, most-recent activity first. Secondary sort on
	// bucket keeps sections grouped if the caller ever mixes them.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].bucket != rows[j].bucket {
			return rows[i].bucket < rows[j].bucket
		}
		return rows[i].when.After(rows[j].when)
	})
	return rows, nil
}

// classify decides which section a run lands in and what note to
// render. In-progress runs with more work to do land in ACTIVE;
// pushed runs land in ACTIVE too with "awaiting merge: #<n>" since
// the operator still owes a click on GitHub; merged and closed runs
// land in COMPLETED. Dormant runs are dropped unless the caller asked
// for --all.
func classify(root string, md *run.Metadata, last, now time.Time, includeDormant bool, byRunKey map[string]*run.Metadata) (bucket, string, error) {
	if !includeDormant && !last.IsZero() && now.Sub(last) > dormantCutoff {
		return bucketNone, "", nil
	}
	// Every note is prefixed with the workflow name so the dashboard
	// says "where" a run lives, not just "what's next". Two workflows
	// can share a stage name (sdlc and kb both have generic stages in
	// flight), so the prefix is what makes the cell self-describing.
	prefix := md.Workflow + ":"
	// Idea runs have their own lane: open ones are backlog, closed /
	// promoted ones go to completed with a distinguishing label so the
	// operator can tell "handed off to another run" from "dropped".
	if md.Workflow == ideaWorkflow {
		switch md.Status {
		case run.StatusInProgress:
			return bucketBacklog, prefix + "capture", nil
		case run.StatusPromoted:
			note := prefix + "promoted"
			if slug, ok := promotedToRun(root, md.ID, byRunKey); ok {
				note += " → " + slug
			}
			return bucketCompletedRuns, note, nil
		case run.StatusClosed:
			return bucketCompletedRuns, prefix + "closed", nil
		}
		return bucketNone, "", nil
	}
	switch md.Status {
	case run.StatusPushed:
		note := "awaiting merge"
		if n, ok := prNumberForRun(root, md.ID); ok {
			note = fmt.Sprintf("awaiting merge: #%s", n)
		}
		return bucketActiveRuns, prefix + note, nil
	case run.StatusMerged:
		return bucketCompletedRuns, prefix + "merged", nil
	case run.StatusClosed:
		return bucketCompletedRuns, prefix + "closed", nil
	case run.StatusPromoted:
		// Non-idea runs shouldn't wear StatusPromoted, but if one
		// ever does (future --from-run promotion), surface it as
		// completed with the same label as the idea case.
		return bucketCompletedRuns, prefix + "promoted", nil
	}
	if md.Status != run.StatusInProgress {
		// Unknown/future status values (e.g., a "scrapped" lane once
		// `moe scrap` lands). Leave them off the dashboard rather than
		// guess a label — they'll surface via `moe history` when that
		// ships.
		return bucketNone, "", nil
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return 0, "", err
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		return 0, "", err
	}
	if kind == NextKindDone {
		// Terminal stage satisfied but no push transition — e.g. KB
		// workflow, which ends at `summarize` and has no push. Treat
		// the same as StatusPushed for dashboard purposes.
		return bucketCompletedRuns, prefix + "done", nil
	}
	return bucketActiveRuns, prefix + next.Name, nil
}

// promotedToRun returns the slug (run ID) of the successor run recorded
// on a promoted idea's MoE-Promoted-To trailer (`<project>/<runID>`).
// Returns ("", false) when the trailer is missing, malformed, or the
// destination run is no longer in the scanned set — caller falls back
// to the bare "promoted" label so the arrow only appears when we can
// name where it went.
func promotedToRun(root, runID string, byRunKey map[string]*run.Metadata) (string, bool) {
	v := trailerValue(root, runID, "MoE-Promoted-To")
	if v == "" {
		return "", false
	}
	dest, ok := byRunKey[v]
	if !ok {
		return "", false
	}
	return dest.ID, true
}

// prNumberForRun finds the PR number recorded for runID by pulling
// the MoE-PR URL from commit trailers and reading the number off the
// end. Returns ("", false) when no MoE-PR trailer is on record — dash
// then falls back to an unnumbered "awaiting merge" label.
func prNumberForRun(root, runID string) (string, bool) {
	url := trailerValue(root, runID, "MoE-PR")
	if url == "" {
		return "", false
	}
	if i := strings.LastIndex(url, "/"); i >= 0 {
		n := strings.TrimSpace(url[i+1:])
		if n != "" {
			return n, true
		}
	}
	return "", false
}

// humanAgo renders "Xd ago" / "Xh ago" / "just now". tabwriter-friendly
// (no multi-byte flourishes), and cheap to parse when reading the
// output back in logs.
func humanAgo(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// countProjects returns the number of registered projects, i.e. the
// number of projects/<id>/project.json files. Matches how
// moe project add writes them.
func countProjects(root string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "project.json"))
	if err != nil {
		return 0, fmt.Errorf("dash: glob projects: %w", err)
	}
	return len(matches), nil
}

// renderDash prints the header, three sections (ACTIVE, BACKLOG,
// COMPLETED), and footer. Order is operator-first: what you can
// act on now, then what's queued, then what's already done. tabwriter
// aligns columns per section so a long idea title doesn't widen the
// run rows. COMPLETED is capped at completedCap — older finished
// runs still live in the bureaucracy repo, they just don't clutter
// the dashboard. Section headings use the cyan-moe style from
// output.go; rows stay plain so tabwriter's byte-counting aligns
// correctly (ANSI codes would skew column widths).
func renderDash(w io.Writer, now time.Time, rows []dashRow, projectCount, activeCount int) {
	moePrintf(w, "Ministry of Everything %38s\n\n", now.Format("2006-01-02  15:04"))

	var active, backlog, completed []dashRow
	for _, r := range rows {
		switch r.bucket {
		case bucketActiveRuns:
			active = append(active, r)
		case bucketBacklog:
			backlog = append(backlog, r)
		case bucketCompletedRuns:
			completed = append(completed, r)
		}
	}

	moePrintf(w, "ACTIVE (%d)\n", len(active))
	if len(active) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range active {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.project, r.run, humanAgo(now, r.when), r.note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	moePrintf(w, "BACKLOG (%d)\n", len(backlog))
	if len(backlog) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range backlog {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.project, r.run, humanAgo(now, r.when), r.note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	shown := completed
	if len(completed) > completedCap {
		shown = completed[:completedCap]
	}
	if len(shown) < len(completed) {
		moePrintf(w, "COMPLETED (%d of %d)\n", len(shown), len(completed))
	} else {
		moePrintf(w, "COMPLETED (%d)\n", len(completed))
	}
	if len(shown) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range shown {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.project, r.run, humanAgo(now, r.when), r.note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	moePrintf(w, "%d project(s) registered · %d active\n", projectCount, activeCount)
}
