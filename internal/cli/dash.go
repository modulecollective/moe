package cli

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/wiki"
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

// twinRecentCap is the per-project limit on the "recent: …" sub-line
// under each TWIN row. Twin sessions aren't dormant the way runs are,
// so there's no `--all` gate — older sessions stay in `git log`.
const twinRecentCap = 3

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
	stage   string    // bare next-stage name for active runs (no workflow prefix); "" for backlog/completed. Drives the factory art's station glyph.
	when    time.Time // sort key within the section; most recent first.
	bucket  bucket
}

func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show everything (no dormancy filter, no completed-run cap)")
	project := fs.String("project", "", "show only rows whose run belongs to this project")
	workflow := fs.String("workflow", "", "show only rows whose run uses this workflow")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all] [--project <id>] [--workflow <name>]")
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
	// One batched git log covers every run's last activity. The map
	// then threads through buildDashRows and buildTwinRows so per-run
	// and per-project paths reuse it instead of forking git per run.
	acts, err := run.LastActivityMap(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	now := time.Now().UTC()
	// Queue membership is best-effort decoration: a corrupt or missing
	// queue.json silently yields no markers and the dash still renders.
	// Loud errors on bad queue state belong in queue add/list/run, where
	// the operator can act.
	queuedSet := make(map[queueItem]struct{})
	if items, err := loadQueue(root); err == nil {
		for _, it := range items {
			queuedSet[it] = struct{}{}
		}
	}
	// Open-session liveness is best-effort the same way: a `git worktree
	// list` failure silently yields no markers. The signal closes the
	// gap between "what the parking rule says is next" (correct for
	// resume) and "what's actually running right now" (the operator's
	// off-screen claude session). session.List captures that window
	// because Open registers the worktree at session start and Close
	// tears it down at session end.
	sessionDocsByRun := make(map[string][]string)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionDocsByRun[s.Run] = append(sessionDocsByRun[s.Run], s.Doc)
		}
	}
	rows, err := buildDashRows(root, mds, acts, now, *all, *project, *workflow, queuedSet, sessionDocsByRun)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	projectCount, err := countProjects(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Twin status — per-project freshness and unrecorded-edits
	// banner. Filtered by --project (matches the workflow filter
	// behavior: empty projectFilter shows every project's twin).
	twinRows, err := buildTwinRows(root, mds, acts, *project)
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

	state := factoryStateFromRows(rows)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	renderDash(stdout, now, rows, twinRows, projectCount, activeCount, *all, state, r)
	return 0
}

// factoryStateFromRows folds dashboard rows into the data the art
// reads. Active stages come straight off the rows in their existing
// recency-sorted order; backlog and completed counts are simple
// bucket totals. Pure over rows so the art renderer stays testable.
func factoryStateFromRows(rows []dashRow) factoryState {
	var state factoryState
	for _, r := range rows {
		switch r.bucket {
		case bucketActiveRuns:
			state.ActiveStages = append(state.ActiveStages, r.stage)
		case bucketBacklog:
			state.BacklogCount++
		case bucketCompletedRuns:
			state.CompletedCount++
		}
	}
	return state
}

// buildDashRows maps scanned metadata to dashboard rows. Per-run
// git queries live here so renderDash stays a pure printer. Ideas are
// just runs with workflow=idea; classify routes open ones to the
// backlog bucket and closed/promoted ones to completed, so there's no
// separate scan of a markdown-file shelf.
//
// projectFilter and workflowFilter narrow the view; empty string means
// no filter. Last-activity for each run is read from acts (built once by
// the caller via run.LastActivityMap) instead of forking git per row.
// queuedSet marks active runs that sit on the operator's playlist —
// rows whose (workflow, project, run) identity is in the set get a
// "[queued]" suffix on the note column. sessionDocsByRun is the
// per-run list of documents with an open stage session; classify
// renders that as a "[running]" / "[<doc> running]" suffix in front
// of "[queued]" so the liveness signal lands first when both apply.
func buildDashRows(root string, mds []*run.Metadata, acts map[string]time.Time, now time.Time, includeDormant bool, projectFilter, workflowFilter string, queuedSet map[queueItem]struct{}, sessionDocsByRun map[string][]string) ([]dashRow, error) {
	// byRunKey lets the promoted-idea branch resolve a successor run's
	// workflow from its MoE-Promoted-To trailer (`<project>/<id>`)
	// without a second disk scan.
	byRunKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byRunKey[md.Project+"/"+md.ID] = md
	}
	rows := make([]dashRow, 0, len(mds))
	for _, md := range mds {
		if projectFilter != "" && md.Project != projectFilter {
			continue
		}
		if workflowFilter != "" && md.Workflow != workflowFilter {
			continue
		}
		last := acts[md.ID]
		b, note, stage, err := classify(root, md, last, now, includeDormant, byRunKey, sessionDocsByRun[md.ID])
		if err != nil {
			return nil, err
		}
		if b == bucketNone {
			continue
		}
		if b == bucketActiveRuns {
			if _, ok := queuedSet[queueItem{Workflow: md.Workflow, Project: md.Project, Run: md.ID}]; ok {
				note += " [queued]"
			}
		}
		rows = append(rows, dashRow{
			project: md.Project,
			run:     md.ID,
			note:    note,
			stage:   stage,
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

// classify decides which section a run lands in, what note to render,
// and (for active runs) the bare next-stage name the factory art uses
// to pick a station glyph. In-progress runs with more work to do land
// in ACTIVE; pushed runs land in ACTIVE too with "awaiting merge: #<n>"
// since the operator still owes a click on GitHub; merged and closed
// runs land in COMPLETED. Dormant runs are dropped unless the caller
// asked for --all.
func classify(root string, md *run.Metadata, last, now time.Time, includeDormant bool, byRunKey map[string]*run.Metadata, openSessionDocs []string) (bucket, string, string, error) {
	if !includeDormant && !last.IsZero() && now.Sub(last) > dormantCutoff {
		return bucketNone, "", "", nil
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
			return bucketBacklog, prefix + "capture", "", nil
		case run.StatusPromoted:
			note := prefix + "promoted"
			if slug, ok := promotedToRun(root, md.ID, byRunKey); ok {
				note += " → " + slug
			}
			return bucketCompletedRuns, note, "", nil
		case run.StatusClosed:
			return bucketCompletedRuns, prefix + "closed", "", nil
		}
		return bucketNone, "", "", nil
	}
	switch md.Status {
	case run.StatusPushed:
		note := "awaiting merge"
		if n, ok := prNumberForRun(root, md.ID); ok {
			note = fmt.Sprintf("awaiting merge: #%s", n)
		}
		// Pushed runs have no parked doc — the prefix names a state,
		// not a stage — so any open session is "different doc" by
		// construction and renders as "[<doc> running]".
		return bucketActiveRuns, prefix + note + openSessionMarker(openSessionDocs, ""), "awaiting merge", nil
	case run.StatusMerged:
		return bucketCompletedRuns, prefix + "merged", "", nil
	case run.StatusClosed:
		return bucketCompletedRuns, prefix + "closed", "", nil
	case run.StatusPromoted:
		// Non-idea runs shouldn't wear StatusPromoted, but if one
		// ever does (future --from-run promotion), surface it as
		// completed with the same label as the idea case.
		return bucketCompletedRuns, prefix + "promoted", "", nil
	}
	if md.Status != run.StatusInProgress {
		// Unknown/future status values (e.g., a "scrapped" lane once
		// `moe scrap` lands). Leave them off the dashboard rather than
		// guess a label — they'll surface via `moe history` when that
		// ships.
		return bucketNone, "", "", nil
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return 0, "", "", err
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		return 0, "", "", err
	}
	if kind == NextKindDone {
		// Terminal stage satisfied but no push transition — e.g. KB
		// workflow, which ends at `summarize` and has no push. Treat
		// the same as StatusPushed for dashboard purposes.
		return bucketCompletedRuns, prefix + "done", "", nil
	}
	return bucketActiveRuns, prefix + next.Name + openSessionMarker(openSessionDocs, next.Name), next.Name, nil
}

// openSessionMarker returns the " [running]" suffix the dash glues onto
// an active-run note when a stage session is open against the run.
// parkedDoc is the docID the parking rule reports as next ("" for
// pushed runs, where the prefix names a state rather than a stage).
//
// A session on a different doc than the parked one wins front position:
// it's the more interesting signal, since the operator is mid-edit on a
// document the parking rule won't surface until the session commits.
// Multiple open sessions on one run are unexpected (sessions are
// sequential by design) — when they do show up, the same "non-parked
// wins" rule applies, since the parked one is implicit in the prefix.
func openSessionMarker(openDocs []string, parkedDoc string) string {
	if len(openDocs) == 0 {
		return ""
	}
	for _, d := range openDocs {
		if d != parkedDoc {
			return " [" + d + " running]"
		}
	}
	return " [running]"
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
// run rows. COMPLETED is capped at completedCap unless showAll is set
// — older finished runs still live in the bureaucracy repo, they just
// don't clutter the dashboard. Section headings use the cyan-moe
// style from output.go; rows stay plain so tabwriter's byte-counting
// aligns correctly (ANSI codes would skew column widths).
// twinRow is one project's twin status for the TWIN section. Kept
// flat (not part of dashRow) because the twin is project-scoped, not
// run-scoped — a different rail than the runs/ideas the rest of the
// dashboard tracks.
type twinRow struct {
	project string
	note    string
	recents []twinRecent // newest first, ≤ twinRecentCap.
}

// twinRecent is one twin session surfaced under a TWIN row. Sessions
// have no run.json — they're identified by a synthetic
// `<verb>-<timestamp>` slug recorded on commit trailers — so the
// dashboard reads them straight off the journal.
type twinRecent struct {
	verb string    // "reflect" | "claim" — the slug prefix. Older history may also surface "plan" / "lint" verbs from before those commands were folded into reflect; the dash renders whatever prefix git returns.
	when time.Time // commit time of the latest commit in this session.
}

// buildTwinRows scans the bureaucracy for projects whose twin is on
// disk and emits one row per project naming the most useful status
// signal: unrecorded edits beat staleness beat "fresh." Projects
// without a digital-twin/ dir don't appear — they're projects that
// haven't bootstrapped their twin yet, and the dash shouldn't pester
// the operator about a feature they haven't opted into.
//
// projectFilter narrows the view to a single project (empty = all).
// mds and acts are the cached scan + last-activity map produced once in
// runDash; buildTwinRows threads them into twinStatusNote so the twin
// path doesn't re-scan or re-query git per project.
func buildTwinRows(root string, mds []*run.Metadata, acts map[string]time.Time, projectFilter string) ([]twinRow, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "project.json"))
	if err != nil {
		return nil, fmt.Errorf("dash: glob projects: %w", err)
	}
	var rows []twinRow
	for _, m := range matches {
		projectID := filepath.Base(filepath.Dir(m))
		if projectFilter != "" && projectID != projectFilter {
			continue
		}
		cfg, err := twinWikiBuilder(root, projectID)
		if err != nil || cfg == nil {
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
		// Best-effort on recents: a git log error shouldn't suppress
		// the row. Mirrors closedRunsSinceCount's silent-fallback shape.
		recents, _ := recentTwinSessions(root, projectID, twinRecentCap)
		note := twinStatusNote(*cfg, mds, acts)
		if note == "" && len(recents) == 0 {
			continue
		}
		rows = append(rows, twinRow{project: projectID, note: note, recents: recents})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].project < rows[j].project })
	return rows, nil
}

// twinStatusNote inspects a twin's checkpoint and unrecorded-edits
// state and returns the line to render (or "" to suppress the row
// entirely when the twin is fresh and has no decided edits pending).
// Priority: unrecorded edits > never-reflected > stale > fresh.
func twinStatusNote(cfg wiki.Config, mds []*run.Metadata, acts map[string]time.Time) string {
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
	since := closedRunsSinceCount(mds, acts, cfg.Project, last)
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

// closedRunsSinceCount counts the project's closed/merged/promoted
// runs whose last activity post-dates threshold. Used by the dash
// twin row to surface freshness in operator-meaningful units ("3
// closed runs since reflect" reads better than "23 days since"). Pure
// over the cached metadata + activity map — both come from runDash.
func closedRunsSinceCount(mds []*run.Metadata, acts map[string]time.Time, projectID string, threshold time.Time) int {
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
		when := acts[md.ID]
		if when.IsZero() {
			continue
		}
		if when.After(threshold) {
			count++
		}
	}
	return count
}

// recentTwinSessions reads twin sessions for a project off the journal
// and returns the most recent `limit`, newest first. A session is a
// group of commits sharing a `MoE-Run: <verb>-<timestamp>` slug; the
// session's time is the latest commit time in the group. Path-scoped
// to the project's twin dir so unrelated commits don't match. Mirrors
// the `--all-match` + multi-`--grep` shape of trailerValue in push.go.
func recentTwinSessions(root, projectID string, limit int) ([]twinRecent, error) {
	if limit <= 0 {
		return nil, nil
	}
	twinDir := filepath.Join("projects", projectID, wiki.TwinDirRel)
	cmd := exec.Command("git", "-C", root, "log", "--all",
		"--all-match",
		"--grep", "MoE-Workflow: twin",
		"--grep", "MoE-Project: "+projectID,
		"--format=%ct%x00%B%x1e",
		"--", twinDir,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dash: git log twin sessions: %w", err)
	}
	// Group by MoE-Run slug; keep the newest commit time per group.
	type group struct {
		when time.Time
		verb string
	}
	groups := make(map[string]group)
	for _, record := range strings.Split(string(out), "\x1e") {
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
	out2 := make([]twinRecent, 0, len(groups))
	for _, g := range groups {
		out2 = append(out2, twinRecent{verb: g.verb, when: g.when})
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].when.After(out2[j].when) })
	if len(out2) > limit {
		out2 = out2[:limit]
	}
	return out2, nil
}

// formatRecents renders the "recent: …" continuation cell. Each entry
// is "<verb> <humanAgo>"; entries are joined with ", " in the order
// passed in (caller guarantees newest-first).
func formatRecents(now time.Time, recents []twinRecent) string {
	if len(recents) == 0 {
		return ""
	}
	parts := make([]string, 0, len(recents))
	for _, r := range recents {
		parts = append(parts, fmt.Sprintf("%s %s", r.verb, humanAgo(now, r.when)))
	}
	return "recent: " + strings.Join(parts, ", ")
}

func renderDash(w io.Writer, now time.Time, rows []dashRow, twinRows []twinRow, projectCount, activeCount int, showAll bool, state factoryState, r *rand.Rand) {
	moePrintf(w, "Ministry of Everything %38s\n", now.Format("2006-01-02  15:04"))
	for _, line := range buildFactoryArt(state, artWidth, r) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)

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
	if !showAll && len(completed) > completedCap {
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

	if len(twinRows) > 0 {
		moePrintf(w, "TWIN (%d)\n", len(twinRows))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range twinRows {
			recentLine := formatRecents(now, r.recents)
			// A row with no attention note collapses to a single line:
			// project + recents in the note column. The recents line
			// already implies freshness (a reflect 2 minutes ago is the
			// signal), so a synthetic "fresh — last reflected …" prefix
			// would be ceremony. Healthy twins should occupy as little
			// vertical space as possible while still being visible.
			if r.note == "" {
				fmt.Fprintf(tw, "  %s\t%s\n", r.project, recentLine)
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\n", r.project, r.note)
			// Continuation row keeps the project column blank so the
			// recent-line aligns under the note column without inventing
			// a new format.
			if recentLine != "" {
				fmt.Fprintf(tw, "  %s\t%s\n", "", recentLine)
			}
		}
		tw.Flush()
		fmt.Fprintln(w)
	}

	moePrintf(w, "%d project(s) registered · %d active\n", projectCount, activeCount)
}
