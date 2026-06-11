// Package dash assembles the home-screen dashboard: runs, ideas, and
// the factory-art header. The cli/dash.go entry point gathers inputs
// (run scan, journal index, open-session list, per-run next-stage
// decisions) and hands them to BuildRows / Render here.
//
// The package is pure over its inputs except for CountProjects, which
// globs the projects/ tree. Refactoring the cli command into thin glue
// lets a second caller (an HTTP shim, an IDE plugin, a screen-
// recording snapshot) compose the same data without going through
// `cli.Run`.
package dash

import (
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/run"
)

// IdeaWorkflow and IdeaDocID are the cross-cutting contract for the
// idea workflow: the workflow name written to run.json and the doc id
// for the single canvas stage. Every non-test caller (cli, runopen,
// serve) routes through these two symbols on purpose — they live in
// dash because dash is the one package every caller already depends
// on (it owns the home-screen render and is the lowest leaf that
// names the idea workflow). The cli package can't host them without
// pulling its dependency edge into runopen and serve.
const (
	IdeaWorkflow = "idea"
	IdeaDocID    = "idea"
)

// ChatWorkflow and ChatDocID are the chat workflow's cross-cutting
// contract, the same shape as the idea pair: the workflow name written
// to run.json and the doc id for the single chat stage. dash needs the
// workflow name to recognise chat runs in classify (chat is never
// "done" — see there), and the doc id as the bare stage name for the
// factory-art glyph and the open-session marker. The cli chat consts
// alias these so the string lives in exactly one place.
const (
	ChatWorkflow = "chat"
	ChatDocID    = "chat"
)

// CompletedCap bounds the COMPLETED section. Finished runs are
// history — useful as recent context, not as a backlog — so the dash
// shows the newest N and lets the bureaucracy repo itself be the
// archive.
const CompletedCap = 10

// Bucket labels a row's section. Active runs (next stage to run) and
// completed runs (pushed or terminal) live on different rails from
// backlog ideas; the operator's eye lands on active work first, so
// the iota order mirrors the on-screen order.
type Bucket int

const (
	BucketActiveRuns    Bucket = iota // in-progress runs with a next stage
	BucketChores                      // due project chores, before they become runs
	BucketBacklog                     // captured ideas, not yet promoted to a run
	BucketCompletedRuns               // pushed or terminal runs, shown as "done"
	BucketNone                        // not placed in any section (idea with an unrecognised status; in-progress non-idea run with no next-stage decision)
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
	// Member is true for an active row that follows its chain parent in
	// the grouped ACTIVE order — the renderer draws a connector for it.
	// Heads and singletons (and every backlog/completed row) are false.
	Member bool
}

// NextDecision is the per-run "what's next" decision the caller
// pre-computes by asking its workflow registry. Stage is the bare
// stage name (e.g. "code"); for a perpetual Done run, callers set it
// to the repeatable stage dash should keep showing.
type NextDecision struct {
	Stage     string
	Done      bool
	Perpetual bool
}

type ChoreInput struct {
	Project string
	Name    string
	Reason  string
	When    time.Time
}

// Inputs is everything BuildRows needs. The caller computes most of
// these once (run.Scan, run.BuildJournalIndex, the session list, the
// workflow-resolution loop) and threads the same values into Render
// to keep the hot path off-disk.
type Inputs struct {
	Now              time.Time
	ProjectFilter    string
	WorkflowFilter   string
	Runs             []*run.Metadata
	Index            *run.JournalIndex
	SessionDocsByRun map[string][]string
	NextByRun        map[string]NextDecision // populated only for in-progress, non-idea runs.
	Chores           []ChoreInput
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
		b, note, stage, runningDoc := classify(md, byRunKey, in.Index, in.SessionDocsByRun[md.ID], in.NextByRun)
		if b == BucketNone {
			continue
		}
		// A run bound to a named workspace surfaces it as "@<name>" on
		// the active row so the operator can see at a glance which
		// workspace the row iterates against. For sdlc that's the run's
		// working tree; for hooks it's a no-claim label. Either way the
		// label is the cwd the operator's about to type into.
		if md.Workspace != "" && b == BucketActiveRuns {
			note = note + " @" + md.Workspace
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
	groupActiveChains(rows, in.Index, byRunKey)
	var choreRows []Row
	for _, c := range in.Chores {
		if in.ProjectFilter != "" && c.Project != in.ProjectFilter {
			continue
		}
		choreRows = append(choreRows, Row{
			Project: c.Project,
			Run:     c.Name,
			Note:    c.Reason,
			When:    c.When,
			Bucket:  BucketChores,
		})
	}
	sort.SliceStable(choreRows, func(i, j int) bool {
		return choreRows[i].When.After(choreRows[j].When)
	})
	if len(choreRows) > 0 {
		insert := 0
		for insert < len(rows) && rows[insert].Bucket == BucketActiveRuns {
			insert++
		}
		rows = append(rows[:insert], append(choreRows, rows[insert:]...)...)
	}
	return rows, nil
}

// groupActiveChains reorders the ACTIVE bucket so each chain renders as
// a contiguous, head-first block, marks the following members so the
// renderer can draw a connector, and reattaches the textual
// "· chained → X" hint only for edges adjacency doesn't already show
// (a fan-in's second parent, or a child that fell out of the active
// set). BACKLOG and COMPLETED rows keep their recency order.
//
// rows must already be bucket-then-recency sorted, so the ACTIVE bucket
// is the leading run of BucketActiveRuns rows.
func groupActiveChains(rows []Row, idx *run.JournalIndex, byKey map[string]*run.Metadata) {
	n := 0
	for n < len(rows) && rows[n].Bucket == BucketActiveRuns {
		n++
	}
	if n == 0 {
		return
	}
	active := rows[:n]
	keyOf := func(r Row) string { return r.Project + "/" + r.Run }

	// The unit ordering is shared with `chain edit` via run.OrderChainUnits
	// — active rows arrive recency-sorted, so the items feed it in order.
	items := make([]run.ChainOrderItem, n)
	rowByKey := make(map[string]Row, n)
	for i, r := range active {
		k := keyOf(r)
		items[i] = run.ChainOrderItem{Key: k, When: r.When}
		rowByKey[k] = r
	}
	units := run.OrderChainUnits(items, idx, byKey)

	// Emit units head-first. A run past the head of a multi-run unit is a
	// Member (the renderer draws its connector). A parent whose live child
	// follows it in the unit has that edge shown adjacently, so it skips
	// the textual hint below; every other consecutive pair came from the
	// childOf walk, so "has a successor in its unit" is exactly that set.
	shownEdge := make(map[string]bool)
	i := 0
	for _, u := range units {
		for pos, k := range u {
			row := rowByKey[k]
			row.Member = len(u) >= 2 && pos > 0
			active[i] = row
			i++
			if pos+1 < len(u) {
				shownEdge[k] = true
			}
		}
	}

	// Reattach the textual hint for every active parent whose edge
	// adjacency doesn't already show.
	for i := range active {
		if k := keyOf(active[i]); !shownEdge[k] {
			active[i].Note += chainHint(idx, byKey[k], byKey)
		}
	}
}

// classify decides which section a run lands in, what note to render,
// the bare next-stage name the factory art uses to pick a station
// glyph, and the doc with an open session that "wins" the liveness
// slot. Pure over its inputs — no disk I/O.
func classify(md *run.Metadata, byRunKey map[string]*run.Metadata, idx *run.JournalIndex, openSessionDocs []string, nextByRun map[string]NextDecision) (Bucket, string, string, string) {
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
		switch {
		// sdlc-only: reopen is an sdlc verb, so marking non-sdlc closed
		// runs would advertise an action the operator can't take.
		// Closed runs whose MoE-Reopen-Of chain is unextended are the
		// candidates the operator might still want to carry forward —
		// reduxes that previously needed a fresh `*-redux` slug.
		case md.Workflow == "sdlc" && !hasBeenReopened(idx, md.ID):
			note += " · reopen?"
		// chat close is a soft archive: re-entering a closed chat
		// reopens-and-continues the same thread (see classify's
		// in-progress chat note and openChat). Always advertise it —
		// chat has no reopen-chain to exhaust, so no hasBeenReopened gate.
		case md.Workflow == ChatWorkflow:
			note += " · resume?"
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
	if dec.Perpetual && md.Workflow == ChatWorkflow {
		// Chat is the single-stage perpetual workflow whose operator
		// action is always "resume the open thread", not "run chat"
		// or "close". The perpetual bit keeps this in the shared
		// policy path; the wording stays chat-specific.
		runningDoc := winningRunningDoc(openSessionDocs, ChatDocID)
		return BucketActiveRuns, prefix + "open · resume?" + openSessionMarker(runningDoc, ChatDocID), ChatDocID, runningDoc
	}
	if dec.Done {
		if dec.Perpetual && dec.Stage != "" {
			runningDoc := winningRunningDoc(openSessionDocs, dec.Stage)
			return BucketActiveRuns, prefix + dec.Stage + openSessionMarker(runningDoc, dec.Stage), dec.Stage, runningDoc
		}
		// The run has walked every stage but isn't terminal yet — it
		// still needs an operator action (`moe <wf> close`) to land in
		// COMPLETED. Keep it in ACTIVE with a `· close?` action hint,
		// same shape as the `· reopen?` hint on closed sdlc runs.
		// Twin is the canonical case (`done → close` is the only path);
		// sdlc-without-push and kb hit the same shape.
		return BucketActiveRuns, prefix + "done · close?", "done", ""
	}
	runningDoc := winningRunningDoc(openSessionDocs, dec.Stage)
	return BucketActiveRuns, prefix + dec.Stage + openSessionMarker(runningDoc, dec.Stage), dec.Stage, runningDoc
}

// chainHint renders the trailing " · chained → <project>/<slug>"
// action hint for an in-progress run whose live chain edge points at
// an unresolved child. Returns "" when there's no edge, the child slug
// is malformed, the child is missing from disk, or the child is in a
// terminal state (Decision 1 — terminal children are filtered at read
// time).
func chainHint(idx *run.JournalIndex, md *run.Metadata, byRunKey map[string]*run.Metadata) string {
	childKey := idx.ChainedChild[md.Project+"/"+md.ID]
	if !run.ChainChildLive(childKey, byRunKey) {
		return ""
	}
	return " · chained → " + childKey
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

// promotedToRun returns project/slug of the successor run recorded on
// a promoted idea's MoE-Promoted-To trailer.
func promotedToRun(idx *run.JournalIndex, runID string, byRunKey map[string]*run.Metadata) (string, bool) {
	v := idx.PromotedTo[runID]
	if v == "" {
		return "", false
	}
	dest, ok := byRunKey[v]
	if !ok {
		return "", false
	}
	return dest.Project + "/" + dest.ID, true
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
		case BucketChores:
			// Chores are pre-run work and don't drive a station glyph.
		case BucketBacklog:
			state.BacklogCount++
		case BucketCompletedRuns:
			state.CompletedCount++
		}
	}
	return state
}

// Render prints the full dashboard: factory art, three sections
// (ACTIVE, BACKLOG, COMPLETED), and the footer. tabwriter aligns
// columns per section so a long idea title doesn't widen the run
// rows. COMPLETED is capped at CompletedCap unless showAll is set.
//
// The dash banner (rendered upstream of Render by the CLI handler)
// carries the render timestamp; Render itself no longer prints a
// title line. `now` is still threaded through for HumanAgo inside
// the per-row decoration.
//
// activeCount is the number of *projects* with at least one active
// run — not the count of active rows. The footer reads "N project(s)
// registered · M with active runs", so both numbers count projects.
// The ACTIVE section header already exposes the row count.
func Render(w io.Writer, now time.Time, rows []Row, projectCount, activeCount int, showAll bool, state FactoryState, r *rand.Rand) {
	for _, line := range BuildFactoryArt(state, ArtWidth, r) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)

	var active, chores, backlog, completed []Row
	for _, r := range rows {
		switch r.Bucket {
		case BucketActiveRuns:
			active = append(active, r)
		case BucketChores:
			chores = append(chores, r)
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
			slug := r.Project + "/" + r.Run
			if r.Member {
				slug = "↳ " + slug
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "CHORES (%d)\n", len(chores))
	if len(chores) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range chores {
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
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
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
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
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "%d project(s) registered · %d with active runs\n", projectCount, activeCount)
}
