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
	"github.com/modulecollective/moe/internal/request"
	"github.com/modulecollective/moe/internal/stage"
)

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (needs-attention / active / recent)",
		Run:     runDash,
	})
}

// dormantCutoff is the staleness threshold for the ACTIVE bucket. A
// request with no MoE-Request-scoped commit in this window is considered
// dormant and hidden unless --all is passed. Matches README §"The
// attention filter": "Dormant requests (no activity in 30+ days)
// collapse out of the default view".
const dormantCutoff = 30 * 24 * time.Hour

// recentWindow is how far back the RECENT bucket looks for approved
// requests. README mock uses "RECENT (last 7 days)".
const recentWindow = 7 * 24 * time.Hour

// bucket labels a request's slot in the dashboard. Ordered so that the
// most actionable sits at the top (needs attention) and historical
// context sits at the bottom (recent).
type bucket int

const (
	bucketNeedsAttention bucket = iota
	bucketActive
	bucketRecent
	bucketNone // filtered out entirely (dormant without --all)
)

// dashRow is one entry in the dashboard. Kept flat so tabwriter can
// render it without further computation — all the state machinery
// runs up front in buildDashRows.
type dashRow struct {
	project string
	request string
	note    string
	when    time.Time // sort key within the bucket; most recent first
	bucket  bucket
}

func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "include dormant requests (no activity in 30+ days)")
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

	mds, err := request.Scan(root)
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
		if md.Status == "in_progress" {
			activeCount++
		}
	}

	renderDash(stdout, now, rows, projectCount, activeCount)
	return 0
}

// buildDashRows maps scanned metadata to dashboard rows. Per-request
// git queries live here so renderDash stays a pure printer.
func buildDashRows(root string, mds []*request.Metadata, now time.Time, includeDormant bool) ([]dashRow, error) {
	rows := make([]dashRow, 0, len(mds))
	for _, md := range mds {
		last, err := request.LastActivity(root, md.ID)
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
			request: md.ID,
			note:    note,
			when:    last,
			bucket:  b,
		})
	}
	// Within a bucket, most-recent activity first. Across buckets, the
	// renderer walks buckets in order, so a secondary sort on bucket
	// keeps rows grouped even if the caller ever mixes them.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].bucket != rows[j].bucket {
			return rows[i].bucket < rows[j].bucket
		}
		return rows[i].when.After(rows[j].when)
	})
	return rows, nil
}

// classify decides which bucket a request lands in. See designs/dash.md
// for which attention-filter rules are live today versus deferred.
func classify(root string, md *request.Metadata, last, now time.Time, includeDormant bool) (bucket, string, error) {
	if md.Status == "approved" {
		if !last.IsZero() && now.Sub(last) <= recentWindow {
			return bucketRecent, fmt.Sprintf("approved %s", humanAgo(now, last)), nil
		}
		return bucketNone, "", nil
	}

	if md.Status != "in_progress" {
		// Unknown/future status values (e.g., a "scrapped" lane once
		// `moe scrap` lands). Leave them off the dashboard rather than
		// guess a bucket — they'll surface via `moe history` when that
		// ships.
		return bucketNone, "", nil
	}

	ready, readyNote, err := readyToSign(root, md)
	if err != nil {
		return 0, "", err
	}
	if ready {
		return bucketNeedsAttention, readyNote, nil
	}

	if !includeDormant && !last.IsZero() && now.Sub(last) > dormantCutoff {
		return bucketNone, "", nil
	}
	return bucketActive, activeNote(root, md), nil
}

// readyToSign reports whether the request's active stage is a
// reasonable sign target right now: the canonical document for the
// stage has a non-empty content.md, and no prerequisite was re-signed
// since that document's last work turn. Same "readiness" the
// upstream-change banner uses, inverted.
func readyToSign(root string, md *request.Metadata) (bool, string, error) {
	active, ok, err := stage.Active(root, md.ID)
	if err != nil || !ok {
		return false, "", err
	}
	contentPath := filepath.Join(root, request.ContentPath(md.Project, md.ID, active.Name))
	info, err := os.Stat(contentPath)
	if err != nil || info.Size() == 0 {
		return false, "", nil
	}

	_, lastWhen, err := request.LatestWorkTurnSHA(root, md.ID, active.Name)
	if err != nil {
		return false, "", err
	}
	if lastWhen.IsZero() {
		// content.md exists but no work-turn commit recorded it (stray
		// edit, hand-authored file). Treat as "not yet ready" — the
		// operator should run a turn so the document is on the journal.
		return false, "", nil
	}
	for _, dep := range active.Requires {
		_, signedWhen, err := stage.LatestSign(root, md.ID, dep)
		if err != nil {
			return false, "", err
		}
		if !signedWhen.IsZero() && signedWhen.After(lastWhen) {
			return false, "", nil
		}
	}
	return true, fmt.Sprintf("%s signed, ready to sign %s", joinOrNone(active.Requires), active.Name), nil
}

// activeNote is the short status blurb shown in the ACTIVE bucket. It
// names the active stage so the operator can see at a glance where the
// request is without running `moe status`.
func activeNote(root string, md *request.Metadata) string {
	active, ok, err := stage.Active(root, md.ID)
	if err != nil || !ok {
		return ""
	}
	return fmt.Sprintf("stage %s", active.Name)
}

// joinOrNone formats the Requires list for the NEEDS-ATTENTION note.
// The design stage has no prereqs; saying "design signed, ready to
// sign design" would be nonsense, so collapse to a cleaner phrasing.
func joinOrNone(reqs []string) string {
	if len(reqs) == 0 {
		return "ready"
	}
	s := reqs[0]
	for _, r := range reqs[1:] {
		s += ", " + r
	}
	return s
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
// number of requests/<id>/project.json files. Matches how
// moe project add writes them.
func countProjects(root string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(root, "requests", "*", "project.json"))
	if err != nil {
		return 0, fmt.Errorf("dash: glob projects: %w", err)
	}
	return len(matches), nil
}

// renderDash prints the header, three bucket sections, and footer.
// tabwriter aligns columns per section rather than across the whole
// output so a tight NEEDS ATTENTION block isn't widened by a long
// ACTIVE row beneath it. Section headings use the cyan-moe style from
// output.go; rows stay plain so tabwriter's byte-counting aligns
// correctly (ANSI codes would skew column widths).
func renderDash(w io.Writer, now time.Time, rows []dashRow, projectCount, activeCount int) {
	moePrintf(w, "Ministry of Everything %38s\n\n", now.Format("2006-01-02  15:04"))

	sections := []struct {
		label  string
		bucket bucket
	}{
		{"NEEDS ATTENTION", bucketNeedsAttention},
		{"ACTIVE", bucketActive},
		{"RECENT (last 7 days)", bucketRecent},
	}
	for _, sec := range sections {
		var section []dashRow
		for _, r := range rows {
			if r.bucket == sec.bucket {
				section = append(section, r)
			}
		}
		moePrintf(w, "%s (%d)\n", sec.label, len(section))
		if len(section) == 0 {
			fmt.Fprintln(w, "  (none)")
			fmt.Fprintln(w)
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range section {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.project, r.request, r.note)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	moePrintf(w, "%d project(s) registered · %d active\n", projectCount, activeCount)
}
