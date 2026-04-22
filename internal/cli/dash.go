package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// bucket labels a row's section. Runs and backlog ideas live on
// different rails — runs have a lifecycle stage, ideas don't — so
// they render in separate sections.
type bucket int

const (
	bucketBacklog bucket = iota // captured ideas, not yet promoted to a run
	bucketRuns                  // everything with a lifecycle: in-progress and terminal
	bucketNone                  // filtered out entirely (dormant without --all)
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
	activeCount := 0
	for _, md := range mds {
		if md.Status == run.StatusInProgress {
			activeCount++
		}
	}

	renderDash(stdout, now, rows, projectCount, activeCount)
	return 0
}

// buildDashRows maps scanned metadata to dashboard rows. Per-run
// git queries live here so renderDash stays a pure printer.
func buildDashRows(root string, mds []*run.Metadata, now time.Time, includeDormant bool) ([]dashRow, error) {
	rows := make([]dashRow, 0, len(mds))
	for _, md := range mds {
		last, err := run.LastActivity(root, md.ID)
		if err != nil {
			return nil, err
		}
		b, note, err := classify(root, md, last, now, includeDormant)
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
	ideas, err := scanAllIdeas(root)
	if err != nil {
		return nil, err
	}
	for _, e := range ideas {
		when := time.Time{}
		if info, err := os.Stat(e.path); err == nil {
			when = info.ModTime().UTC()
		}
		rows = append(rows, dashRow{
			project: e.project,
			run:     e.slug,
			note:    e.title,
			when:    when,
			bucket:  bucketBacklog,
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
// render. In-progress runs show their next stage (from workflow.Next);
// runs past their terminal stage show "done". Dormant runs are
// dropped unless the caller asked for --all.
func classify(root string, md *run.Metadata, last, now time.Time, includeDormant bool) (bucket, string, error) {
	if !includeDormant && !last.IsZero() && now.Sub(last) > dormantCutoff {
		return bucketNone, "", nil
	}
	if md.Status == run.StatusPushed {
		return bucketRuns, "done", nil
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
		return bucketRuns, "done", nil
	}
	return bucketRuns, next.Name, nil
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

// renderDash prints the header, two sections (BACKLOG, RUNS), and
// footer. tabwriter aligns columns per section so a long idea title
// doesn't widen the runs rows above it. Section headings use the
// cyan-moe style from output.go; rows stay plain so tabwriter's
// byte-counting aligns correctly (ANSI codes would skew column widths).
func renderDash(w io.Writer, now time.Time, rows []dashRow, projectCount, activeCount int) {
	moePrintf(w, "Ministry of Everything %38s\n\n", now.Format("2006-01-02  15:04"))

	var backlog, runs []dashRow
	for _, r := range rows {
		switch r.bucket {
		case bucketBacklog:
			backlog = append(backlog, r)
		case bucketRuns:
			runs = append(runs, r)
		}
	}

	moePrintf(w, "BACKLOG (%d)\n", len(backlog))
	if len(backlog) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range backlog {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.project, r.run, r.note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	moePrintf(w, "RUNS (%d)\n", len(runs))
	if len(runs) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range runs {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.project, r.run, humanAgo(now, r.when), r.note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	moePrintf(w, "%d project(s) registered · %d active\n", projectCount, activeCount)
}
