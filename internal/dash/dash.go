// Package dash assembles the home-screen dashboard: runs, ideas, and
// twin status, plus the factory-art header. The cli/dash.go entry
// point gathers inputs (run scan, journal index, queue membership,
// open-session list, per-run next-stage decisions, per-project twin
// configs) and hands them to BuildRows / BuildTwinRows / Render here.
//
// The package is pure over its inputs (with a couple of exceptions
// that shell out to git for journal queries — RecentTwinSessions,
// CountProjects, and a glob inside BuildTwinRows). Refactoring the
// cli command into thin glue lets a second caller (an HTTP shim, an
// IDE plugin, a screen-recording snapshot) compose the same data
// without going through `cli.Run`.
package dash

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// IdeaWorkflow is the workflow name that the idea workflow uses on
// disk. Duplicated here (rather than imported from cli) so dash
// doesn't depend on the cli package; the literal "idea" is the cross-
// cutting contract.
const IdeaWorkflow = "idea"

// DormantCutoff is the staleness threshold for the ACTIVE section.
// A run with no MoE-Run-scoped commit in this window is considered
// dormant and hidden unless the caller passes All=true.
const DormantCutoff = 30 * 24 * time.Hour

// CompletedCap bounds the COMPLETED section. Finished runs are
// history — useful as recent context, not as a backlog — so the dash
// shows the newest N and lets the bureaucracy repo itself be the
// archive.
const CompletedCap = 10

// TwinRecentCap is the per-project limit on the "recent: …" sub-line
// under each TWIN row.
const TwinRecentCap = 3

// Bucket labels a row's section. Active runs (next stage to run) and
// completed runs (pushed or terminal) live on different rails from
// backlog ideas; the operator's eye lands on active work first, so
// the iota order mirrors the on-screen order.
type Bucket int

const (
	BucketActiveRuns    Bucket = iota // in-progress runs with a next stage
	BucketBacklog                     // captured ideas, not yet promoted to a run
	BucketCompletedRuns               // pushed or terminal runs, shown as "done"
	BucketNone                        // filtered out entirely (dormant without --all)
)

// Row is one entry in the dashboard. Kept flat so tabwriter can
// render it without further computation — all the state machinery
// runs up front in BuildRows.
type Row struct {
	Project    string
	Run        string
	Note       string    // for runs: next stage name, or "done"; for backlog: idea title.
	Stage      string    // bare next-stage name for active runs (no workflow prefix); "" for backlog/completed. Drives the factory art's station glyph.
	RunningDoc string    // doc with an open session that "wins" the liveness slot; "" when no session is open. The factory art reads this to decide whether the station smokes and which doc's glyph to draw.
	When       time.Time // sort key within the section; most recent first.
	Bucket     Bucket
}

// QueueKey identifies a queued (workflow, project, run) triple — the
// shape `cli/queue.go` writes to .moe/queue.json. BuildRows uses it
// to mark active rows that sit on the operator's playlist.
type QueueKey struct {
	Workflow string
	Project  string
	Run      string
}

// NextDecision is the per-run "what's next" decision the caller
// pre-computes by asking its workflow registry. Stage is the bare
// stage name (e.g. "code") when Done is false; both fields are zero
// when the run has no next stage to run.
type NextDecision struct {
	Stage string
	Done  bool
}

// Inputs is everything BuildRows needs. The caller computes most of
// these once (run.Scan, run.BuildJournalIndex, the queue load, the
// session list, the workflow-resolution loop) and threads the same
// values into BuildTwinRows / Render to keep the hot path off-disk.
type Inputs struct {
	Now              time.Time
	All              bool
	ProjectFilter    string
	WorkflowFilter   string
	Runs             []*run.Metadata
	Index            *run.JournalIndex
	QueuedSet        map[QueueKey]struct{}
	SessionDocsByRun map[string][]string
	NextByRun        map[string]NextDecision // populated only for in-progress, non-idea runs.
}

// BuildRows maps scanned metadata to dashboard rows. Per-run journal
// reads come straight from in.Index (built once by the caller via
// run.BuildJournalIndex) instead of forking git per row. The caller
// has already pre-computed the next-stage decision for every
// in-progress non-idea run via in.NextByRun, so BuildRows doesn't
// need a workflow registry — keeping this package free of cli's
// per-workflow plumbing.
func BuildRows(in Inputs) ([]Row, error) {
	byRunKey := make(map[string]*run.Metadata, len(in.Runs))
	for _, md := range in.Runs {
		byRunKey[md.Project+"/"+md.ID] = md
	}
	rows := make([]Row, 0, len(in.Runs))
	for _, md := range in.Runs {
		if in.ProjectFilter != "" && md.Project != in.ProjectFilter {
			continue
		}
		if in.WorkflowFilter != "" && md.Workflow != in.WorkflowFilter {
			continue
		}
		last := in.Index.LastActivity[md.ID]
		b, note, stage, runningDoc := classify(md, last, in.Now, in.All, byRunKey, in.Index, in.SessionDocsByRun[md.ID], in.NextByRun)
		if b == BucketNone {
			continue
		}
		if b == BucketActiveRuns {
			if _, ok := in.QueuedSet[QueueKey{Workflow: md.Workflow, Project: md.Project, Run: md.ID}]; ok {
				note += " [queued]"
			}
		}
		rows = append(rows, Row{
			Project:    md.Project,
			Run:        md.ID,
			Note:       note,
			Stage:      stage,
			RunningDoc: runningDoc,
			When:       last,
			Bucket:     b,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Bucket != rows[j].Bucket {
			return rows[i].Bucket < rows[j].Bucket
		}
		return rows[i].When.After(rows[j].When)
	})
	return rows, nil
}

// classify decides which section a run lands in, what note to render,
// the bare next-stage name the factory art uses to pick a station
// glyph, and the doc with an open session that "wins" the liveness
// slot. Pure over its inputs — no disk I/O.
func classify(md *run.Metadata, last, now time.Time, includeDormant bool, byRunKey map[string]*run.Metadata, idx *run.JournalIndex, openSessionDocs []string, nextByRun map[string]NextDecision) (Bucket, string, string, string) {
	if !includeDormant && !last.IsZero() && now.Sub(last) > DormantCutoff {
		return BucketNone, "", "", ""
	}
	prefix := md.Workflow + ":"
	if md.Workflow == IdeaWorkflow {
		switch md.Status {
		case run.StatusInProgress:
			return BucketBacklog, prefix + "capture", "", ""
		case run.StatusPromoted:
			note := prefix + "promoted"
			if slug, ok := promotedToRun(idx, md.ID, byRunKey); ok {
				note += " → " + slug
			}
			return BucketCompletedRuns, note, "", ""
		case run.StatusClosed:
			return BucketCompletedRuns, prefix + "closed", "", ""
		}
		return BucketNone, "", "", ""
	}
	switch md.Status {
	case run.StatusPushed:
		note := "awaiting merge"
		if n, ok := prNumberForRun(idx, md.ID); ok {
			note = fmt.Sprintf("awaiting merge: #%s", n)
		}
		runningDoc := winningRunningDoc(openSessionDocs, "")
		return BucketActiveRuns, prefix + note + openSessionMarker(runningDoc, ""), "awaiting merge", runningDoc
	case run.StatusMerged:
		return BucketCompletedRuns, prefix + "merged", "", ""
	case run.StatusClosed:
		note := prefix + "closed"
		// sdlc-only: reopen is an sdlc verb, so marking non-sdlc closed
		// runs would advertise an action the operator can't take.
		// Closed runs whose MoE-Reopen-Of chain is unextended are the
		// candidates the operator might still want to carry forward —
		// reduxes that previously needed a fresh `*-redux` slug.
		if md.Workflow == "sdlc" && !hasBeenReopened(idx, md.ID) {
			note += " · reopen?"
		}
		return BucketCompletedRuns, note, "", ""
	case run.StatusPromoted:
		return BucketCompletedRuns, prefix + "promoted", "", ""
	}
	if md.Status != run.StatusInProgress {
		return BucketNone, "", "", ""
	}
	dec, ok := nextByRun[md.ID]
	if !ok {
		// Caller didn't compute a next-stage decision — treat as no
		// next stage. Shouldn't happen for in-progress non-idea runs
		// in practice; surfaces as bucketNone so the row drops out
		// rather than rendering a blank stage cell.
		return BucketNone, "", "", ""
	}
	if dec.Done {
		return BucketCompletedRuns, prefix + "done", "", ""
	}
	runningDoc := winningRunningDoc(openSessionDocs, dec.Stage)
	return BucketActiveRuns, prefix + dec.Stage + openSessionMarker(runningDoc, dec.Stage), dec.Stage, runningDoc
}

// winningRunningDoc picks the open-session doc that "wins" the row's
// liveness slot. parkedDoc is the docID the parking rule reports as
// next ("" for pushed runs, where the prefix names a state rather
// than a stage).
func winningRunningDoc(openDocs []string, parkedDoc string) string {
	if len(openDocs) == 0 {
		return ""
	}
	for _, d := range openDocs {
		if d != parkedDoc {
			return d
		}
	}
	return parkedDoc
}

// openSessionMarker renders the " [running]" / " [<doc> running]"
// suffix the dash glues onto an active-run note.
func openSessionMarker(runningDoc, parkedDoc string) string {
	if runningDoc == "" {
		return ""
	}
	if runningDoc != parkedDoc {
		return " [" + runningDoc + " running]"
	}
	return " [running]"
}

// promotedToRun returns the slug (run ID) of the successor run
// recorded on a promoted idea's MoE-Promoted-To trailer.
func promotedToRun(idx *run.JournalIndex, runID string, byRunKey map[string]*run.Metadata) (string, bool) {
	v := idx.PromotedTo[runID]
	if v == "" {
		return "", false
	}
	dest, ok := byRunKey[v]
	if !ok {
		return "", false
	}
	return dest.ID, true
}

// hasBeenReopened reports whether any run in the journal claims slug
// as its MoE-Reopen-Of prior. Scans ReopenedFrom's values rather than
// keying off them so the lookup matches the question dash actually
// asks ("is this prior the source of a reopen?"), and so a single
// reopen index serves both directions without a second map. O(n)
// scan; n is bounded by the number of reopens across the bureaucracy
// (small).
func hasBeenReopened(idx *run.JournalIndex, slug string) bool {
	if idx == nil {
		return false
	}
	for _, prior := range idx.ReopenedFrom {
		if prior == slug {
			return true
		}
	}
	return false
}

// prNumberForRun finds the PR number recorded for runID by pulling
// the MoE-PR URL from the journal index and reading the number off
// the end. Returns ("", false) when no MoE-PR trailer is on record.
func prNumberForRun(idx *run.JournalIndex, runID string) (string, bool) {
	url := idx.PRURL[runID]
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

// HumanAgo renders "Xd ago" / "Xh ago" / "just now". tabwriter-
// friendly (no multi-byte flourishes), and cheap to parse when
// reading the output back in logs.
func HumanAgo(now, t time.Time) string {
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

// CountProjects returns the number of registered projects in root —
// i.e. projects/<id>/project.json files.
func CountProjects(root string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "project.json"))
	if err != nil {
		return 0, fmt.Errorf("dash: glob projects: %w", err)
	}
	return len(matches), nil
}

// TwinRow is one project's twin status for the TWIN section. Kept
// flat (not part of Row) because the twin is project-scoped, not
// run-scoped.
type TwinRow struct {
	Project string
	Note    string
	Recents []TwinRecent // newest first, ≤ TwinRecentCap.
}

// TwinRecent is one twin session surfaced under a TwinRow. Sessions
// have no run.json — they're identified by a synthetic
// `<verb>-<timestamp>` slug recorded on commit trailers — so the dash
// reads them straight off the journal.
type TwinRecent struct {
	Verb string    // "reflect" | "claim" — the slug prefix.
	When time.Time // commit time of the latest commit in this session.
}

// BuildTwinRows scans the supplied per-project twin configs and emits
// one row per project whose twin earns surfacing. Projects without an
// on-disk twin contribute nothing — they're projects that haven't
// bootstrapped yet, and the dash shouldn't pester the operator about
// a feature they haven't opted into.
//
// configs is the cli's per-project wiki.Config (built via
// twinWikiBuilder). projectFilter narrows the view to a single
// project (empty = all); mds and idx are the cached scan + index
// produced once in cli's runDash.
func BuildTwinRows(root string, mds []*run.Metadata, idx *run.JournalIndex, projectFilter string, configs []wiki.Config) ([]TwinRow, error) {
	var rows []TwinRow
	for _, cfg := range configs {
		if projectFilter != "" && cfg.Project != projectFilter {
			continue
		}
		if _, err := os.Stat(cfg.ContentDir); err != nil {
			continue
		}
		// Two independent signals can earn a row: an attention note
		// (unrecorded edits / never reflected / staleness) and recent
		// twin activity. Compute both, then admit the row if either
		// has content — a healthy twin with recent reflects shouldn't
		// vanish just because nothing needs the operator's attention.
		recents, _ := RecentTwinSessions(root, cfg.Project, TwinRecentCap)
		note := TwinStatusNote(cfg, mds, idx)
		if note == "" && len(recents) == 0 {
			continue
		}
		rows = append(rows, TwinRow{Project: cfg.Project, Note: note, Recents: recents})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Project < rows[j].Project })
	return rows, nil
}

// TwinStatusNote inspects a twin's checkpoint and unrecorded-edits
// state and returns the line to render (or "" to suppress the row
// entirely when the twin is fresh and has no decided edits pending).
// Priority: unrecorded edits > never-reflected > stale > fresh.
func TwinStatusNote(cfg wiki.Config, mds []*run.Metadata, idx *run.JournalIndex) string {
	det, err := wiki.DetectUnrecordedEdits(cfg)
	if err == nil && len(det.UnrecordedDocs) > 0 {
		return fmt.Sprintf("unrecorded edits to %s — run `moe twin claim %s`",
			strings.Join(det.UnrecordedDocs, ", "), cfg.Project)
	}
	cp, ok, err := wiki.ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return ""
	}
	if !ok || cp.LastIngestAt == "" {
		return fmt.Sprintf("never reflected — run `moe twin reflect %s`", cfg.Project)
	}
	last, err := time.Parse(time.RFC3339, cp.LastIngestAt)
	if err != nil {
		return ""
	}
	since := ClosedRunsSinceCount(mds, idx, cfg.Project, last)
	if since == 0 {
		return ""
	}
	noun := "closed runs"
	if since == 1 {
		noun = "closed run"
	}
	return fmt.Sprintf("last reflected %s — %d %s since",
		last.Format("2006-01-02"), since, noun)
}

// ClosedRunsSinceCount counts the project's closed/merged/promoted
// runs whose last activity post-dates threshold. Pure over the cached
// metadata + journal index.
func ClosedRunsSinceCount(mds []*run.Metadata, idx *run.JournalIndex, projectID string, threshold time.Time) int {
	count := 0
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		switch md.Status {
		case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		default:
			continue
		}
		when := idx.LastActivity[md.ID]
		if when.IsZero() {
			continue
		}
		if when.After(threshold) {
			count++
		}
	}
	return count
}

// RecentTwinSessions reads twin sessions for a project off the journal
// and returns the most recent `limit`, newest first. A session is a
// group of commits sharing a `MoE-Run: <verb>-<timestamp>` slug; the
// session's time is the latest commit time in the group. Path-scoped
// to the project's twin dir so unrelated commits don't match.
func RecentTwinSessions(root, projectID string, limit int) ([]TwinRecent, error) {
	if limit <= 0 {
		return nil, nil
	}
	twinDir := filepath.Join("projects", projectID, wiki.TwinDirRel)
	out, err := git.Output(root, "log", "--all",
		"--all-match",
		"--grep", "MoE-Workflow: twin",
		"--grep", "MoE-Project: "+projectID,
		"--format=%ct%x00%B%x1e",
		"--", twinDir,
	)
	if err != nil {
		return nil, fmt.Errorf("dash: git log twin sessions: %w", err)
	}
	type group struct {
		when time.Time
		verb string
	}
	groups := make(map[string]group)
	for _, record := range strings.Split(out, "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		nul := strings.IndexByte(record, 0)
		if nul < 0 {
			continue
		}
		ts, err := strconv.ParseInt(record[:nul], 10, 64)
		if err != nil {
			continue
		}
		body := record[nul+1:]
		slug := ""
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok {
				slug = strings.TrimSpace(v)
				break
			}
		}
		if slug == "" {
			continue
		}
		verb := slug
		if i := strings.IndexByte(slug, '-'); i > 0 {
			verb = slug[:i]
		}
		when := time.Unix(ts, 0).UTC()
		if cur, ok := groups[slug]; !ok || when.After(cur.when) {
			groups[slug] = group{when: when, verb: verb}
		}
	}
	out2 := make([]TwinRecent, 0, len(groups))
	for _, g := range groups {
		out2 = append(out2, TwinRecent{Verb: g.verb, When: g.when})
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].When.After(out2[j].When) })
	if len(out2) > limit {
		out2 = out2[:limit]
	}
	return out2, nil
}

// FormatRecents renders the "recent: …" continuation cell for a
// TwinRow. Each entry is "<verb> <HumanAgo>"; entries are joined with
// ", " in the order passed in (caller guarantees newest-first).
func FormatRecents(now time.Time, recents []TwinRecent) string {
	if len(recents) == 0 {
		return ""
	}
	parts := make([]string, 0, len(recents))
	for _, r := range recents {
		parts = append(parts, fmt.Sprintf("%s %s", r.Verb, HumanAgo(now, r.When)))
	}
	return "recent: " + strings.Join(parts, ", ")
}

// FactoryStateFromRows folds dashboard rows into the data the art
// reads. Active stations come straight off the rows in their existing
// recency-sorted order, carrying both the parked stage and (when a
// stage session is open) the live doc; backlog and completed counts
// are simple bucket totals.
func FactoryStateFromRows(rows []Row) FactoryState {
	var state FactoryState
	for _, r := range rows {
		switch r.Bucket {
		case BucketActiveRuns:
			state.ActiveStages = append(state.ActiveStages, ActiveStation{
				Stage:      r.Stage,
				RunningDoc: r.RunningDoc,
			})
		case BucketBacklog:
			state.BacklogCount++
		case BucketCompletedRuns:
			state.CompletedCount++
		}
	}
	return state
}

// Render prints the full dashboard: header, factory art, three
// sections (ACTIVE, BACKLOG, COMPLETED), the optional TWIN section,
// and the footer. tabwriter aligns columns per section so a long
// idea title doesn't widen the run rows. COMPLETED is capped at
// CompletedCap unless showAll is set.
func Render(w io.Writer, now time.Time, rows []Row, twinRows []TwinRow, projectCount, activeCount int, showAll bool, state FactoryState, r *rand.Rand) {
	cliout.Printf(w, "Ministry of Everything %38s\n", now.Format("2006-01-02  15:04"))
	for _, line := range BuildFactoryArt(state, ArtWidth, r) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)

	var active, backlog, completed []Row
	for _, r := range rows {
		switch r.Bucket {
		case BucketActiveRuns:
			active = append(active, r)
		case BucketBacklog:
			backlog = append(backlog, r)
		case BucketCompletedRuns:
			completed = append(completed, r)
		}
	}

	cliout.Printf(w, "ACTIVE (%d)\n", len(active))
	if len(active) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range active {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "BACKLOG (%d)\n", len(backlog))
	if len(backlog) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range backlog {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	shown := completed
	if !showAll && len(completed) > CompletedCap {
		shown = completed[:CompletedCap]
	}
	if len(shown) < len(completed) {
		cliout.Printf(w, "COMPLETED (%d of %d)\n", len(shown), len(completed))
	} else {
		cliout.Printf(w, "COMPLETED (%d)\n", len(completed))
	}
	if len(shown) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range shown {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	if len(twinRows) > 0 {
		cliout.Printf(w, "TWIN (%d)\n", len(twinRows))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range twinRows {
			recentLine := FormatRecents(now, r.Recents)
			if r.Note == "" {
				fmt.Fprintf(tw, "  %s\t%s\n", r.Project, recentLine)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\n", r.Project, r.Note)
			if recentLine != "" {
				fmt.Fprintf(tw, "  %s\t%s\n", "", recentLine)
			}
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	cliout.Printf(w, "%d project(s) registered · %d active\n", projectCount, activeCount)
}
