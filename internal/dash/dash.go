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

// CompletedCutoff returns how many leading completed rows to render so
// that the newest CompletedCap top-level rows are shown, each dragging
// its nested member children (spawned pulses) along for free. Member
// rows never count against the cap, so a parent and its children are
// admitted or evicted as a unit. Returns n unchanged when showAll is
// set or the top-level count is already within the cap.
//
// Shared by the CLI Render and the serve dash view so the terminal and
// the web page cap identically — the completed slice each holds is
// parent-then-children contiguous (BuildRows nests them), so counting
// non-member rows is the same walk on both. isMember reports whether
// row i is a nested child.
func CompletedCutoff(n int, showAll bool, isMember func(i int) bool) int {
	if showAll {
		return n
	}
	tops := 0
	for i := range n {
		if isMember(i) {
			continue
		}
		if tops == CompletedCap {
			return i
		}
		tops++
	}
	return n
}

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
	// Member is true for a row that renders nested under a parent — an
	// active row that follows its chain parent in the grouped ACTIVE
	// order, or a completed spawned run (a tailed pulse) re-attached
	// under its spawner. The renderer draws a connector for it. Heads,
	// singletons, and backlog rows are false.
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
	SessionDocsByRun map[string][]string     // keyed "<project>/<slug>"
	NextByRun        map[string]NextDecision // keyed "<project>/<slug>"; populated only for in-progress, non-idea runs.
	Chores           []ChoreInput
	// PullNext is the latest pulse run's ranked backlog picks per
	// project, in report order. BuildRows floats the ones that still
	// name an open idea to the top of BACKLOG, each carrying its reason
	// as the row note. Picks whose idea has been promoted or closed have
	// no backlog row to float, so they simply drop out — a stale report
	// can only over-highlight, never resurrect. The caller (cli's
	// GatherDashSnapshot) does the disk work; dash stays pure.
	PullNext []PullNextPick
}

// PullNextPick is one ranked backlog pick parsed from a pulse run's
// "## Pull next" section: the project, the open-idea slug it names, and
// the one-line why-now reason.
type PullNextPick struct {
	Project string
	Slug    string
	Reason  string
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
		runKey := md.Project + "/" + md.ID
		last := in.Index.LastActivity[runKey]
		b, note, stage, runningDoc := classify(md, byRunKey, in.Index, in.SessionDocsByRun[runKey], in.NextByRun)
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
	rows = nestSpawnedRuns(rows, in.Index)
	floatPullNext(rows, in.PullNext)
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

// nestSpawnedRuns threads every completed machine-spawned run (one whose
// SpawnedBy resolves to another dashboard row) under its spawner, so a
// tailed pulse renders as lineage on the run that triggered it rather
// than as a standalone completed row that eats a history slot. General
// on the edge, not gated on workflow — pulse is just its first consumer.
//
//   - A parent with exactly one child keeps its single row and gains a
//     " · spawned → <slug>" hint (the chainHint arrow form); the child
//     row is dropped, so the pulse costs zero rows.
//   - A parent with two-plus children (an sdlc run pulses at push and at
//     close) renders like a chain: the parent followed by its children as
//     Member rows, newest first, each normalised into the parent's bucket
//     so a still-active parent and its already-closed pulse render in one
//     section.
//
// Only completed children nest. A spawned run that is still open — a
// broken sweep left open by design so a human escalates to it, the
// single-flight failure surface the design preserves — classifies into
// BucketActiveRuns and stays a top-level ACTIVE row; folding it under a
// (completed or pushed) parent would hide the very thing the operator is
// meant to see. A child whose SpawnedBy names a run that isn't on the
// board (spawner pruned, or a standalone `moe pulse new` with no spawner)
// is likewise left as a normal top-level row. Children never count
// against CompletedCap — the cap is applied over top-level rows in
// Render / the serve view.
//
// Returns a new slice: children are pulled from their natural slots and
// re-emitted under their parent, preserving the incoming bucket-then-
// recency order for every unaffected row.
func nestSpawnedRuns(rows []Row, idx *run.JournalIndex) []Row {
	if idx == nil || len(idx.SpawnedBy) == 0 {
		return rows
	}
	rowIdx := make(map[string]int, len(rows))
	for i := range rows {
		rowIdx[rows[i].Project+"/"+rows[i].Run] = i
	}
	childrenOf := make(map[string][]int)
	isChild := make(map[int]bool)
	for i := range rows {
		key := rows[i].Project + "/" + rows[i].Run
		spawner := idx.SpawnedBy[key]
		if spawner == "" {
			continue
		}
		if rows[i].Bucket != BucketCompletedRuns {
			continue // an open spawned run (a broken sweep left open for
			// failure-escalation) stays a top-level ACTIVE row
		}
		parentKey := rows[i].Project + "/" + spawner
		if _, ok := rowIdx[parentKey]; !ok {
			continue // unresolved spawner — render the child on its own
		}
		childrenOf[parentKey] = append(childrenOf[parentKey], i)
		isChild[i] = true
	}
	if len(isChild) == 0 {
		return rows
	}
	out := make([]Row, 0, len(rows))
	for i := range rows {
		if isChild[i] {
			continue // emitted under its parent below
		}
		parent := rows[i]
		kids := childrenOf[parent.Project+"/"+parent.Run]
		switch len(kids) {
		case 0:
			out = append(out, parent)
		case 1:
			// One child: a textual arrow on the parent, no extra row.
			parent.Note += " · spawned → " + rows[kids[0]].Run
			out = append(out, parent)
		default:
			// Rows arrive recency-sorted, so kids is already newest-first.
			out = append(out, parent)
			for _, ci := range kids {
				child := rows[ci]
				child.Member = true
				child.Bucket = parent.Bucket
				out = append(out, child)
			}
		}
	}
	return out
}

// floatPullNext reorders the BACKLOG bucket so the latest pulse's
// ranked picks lead it, each carrying its why-now reason as the row
// note. It touches only rows already classified into BucketBacklog
// (open ideas), so the open-idea intersection is automatic: a pick
// whose idea has been promoted or closed has no backlog row to float
// and drops out. Unmatched backlog rows keep their recency order after
// the picks.
//
// rows must be bucket-then-recency sorted, so BucketBacklog is a
// contiguous block (chore rows are spliced in later, after this runs).
func floatPullNext(rows []Row, picks []PullNextPick) {
	if len(picks) == 0 {
		return
	}
	reason := make(map[string]string, len(picks))
	rank := make(map[string]int, len(picks))
	for i, p := range picks {
		key := p.Project + "/" + p.Slug
		if _, seen := reason[key]; seen {
			continue // first mention wins the rank
		}
		reason[key] = p.Reason
		rank[key] = i
	}

	lo := 0
	for lo < len(rows) && rows[lo].Bucket != BucketBacklog {
		lo++
	}
	hi := lo
	for hi < len(rows) && rows[hi].Bucket == BucketBacklog {
		hi++
	}
	if lo == hi {
		return
	}

	backlog := rows[lo:hi]
	matched := make([]Row, 0, len(backlog))
	rest := make([]Row, 0, len(backlog))
	for _, r := range backlog {
		key := r.Project + "/" + r.Run
		if why, ok := reason[key]; ok {
			r.Note = "pull: " + why
			matched = append(matched, r)
		} else {
			rest = append(rest, r)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return rank[matched[i].Project+"/"+matched[i].Run] < rank[matched[j].Project+"/"+matched[j].Run]
	})
	copy(backlog, append(matched, rest...))
}

// classify decides which section a run lands in, what note to render,
// the bare next-stage name the factory art uses to pick a station
// glyph, and the doc with an open session that "wins" the liveness
// slot. Pure over its inputs — no disk I/O.
func classify(md *run.Metadata, byRunKey map[string]*run.Metadata, idx *run.JournalIndex, openSessionDocs []string, nextByRun map[string]NextDecision) (Bucket, string, string, string) {
	runKey := md.Project + "/" + md.ID
	prefix := md.Workflow + ":"
	if md.Workflow == IdeaWorkflow {
		switch md.Status {
		case run.StatusInProgress:
			return BucketBacklog, prefix + "capture", "", ""
		case run.StatusPromoted:
			note := prefix + "promoted"
			if slug, ok := promotedToRun(idx, runKey, byRunKey); ok {
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
		if n, ok := prNumberForRun(idx, runKey); ok {
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
		case md.Workflow == "sdlc" && !hasBeenReopened(idx, md.Project, md.ID):
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
	dec, ok := nextByRun[runKey]
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
// a promoted idea's MoE-Promoted-To trailer. runKey is the idea's
// qualified "<project>/<slug>" identity — the key PromotedTo is indexed
// by.
func promotedToRun(idx *run.JournalIndex, runKey string, byRunKey map[string]*run.Metadata) (string, bool) {
	v := idx.PromotedTo[runKey]
	if v == "" {
		return "", false
	}
	dest, ok := byRunKey[v]
	if !ok {
		return "", false
	}
	return dest.Project + "/" + dest.ID, true
}

// hasBeenReopened reports whether any run in projectID claims slug as
// its MoE-Reopen-Of prior. Scans ReopenedFrom's values rather than
// keying off them so the lookup matches the question dash actually
// asks ("is this prior the source of a reopen?"), and so a single
// reopen index serves both directions without a second map. The
// project leg keeps a same-slug prior in another project from
// suppressing this run's reopen hint — reopens are same-project, so a
// matching value only counts when its (qualified) key sits in the same
// project. O(n) scan; n is bounded by the number of reopens across the
// bureaucracy (small).
func hasBeenReopened(idx *run.JournalIndex, projectID, slug string) bool {
	if idx == nil {
		return false
	}
	for key, prior := range idx.ReopenedFrom {
		if prior != slug {
			continue
		}
		if proj, _, ok := strings.Cut(key, "/"); ok && proj == projectID {
			return true
		}
	}
	return false
}

// prNumberForRun finds the PR number recorded for the run keyed by
// runKey ("<project>/<slug>") by pulling the MoE-PR URL from the
// journal index and reading the number off the end. Returns ("", false)
// when no MoE-PR trailer is on record.
func prNumberForRun(idx *run.JournalIndex, runKey string) (string, bool) {
	url := idx.PRURL[runKey]
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
//
// histogram is the pre-rendered activity chart (dash.BuildActivityHistogram
// output); Render prints it between the upstream banner and the factory
// art, keeping the package pure over its inputs.
func Render(w io.Writer, now time.Time, histogram []string, rows []Row, projectCount, activeCount int, showAll bool, state FactoryState, r *rand.Rand) {
	fmt.Fprintln(w)
	for _, line := range histogram {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
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

	shown := completed[:CompletedCutoff(len(completed), showAll, func(i int) bool { return completed[i].Member })]
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
			slug := r.Project + "/" + r.Run
			if r.Member {
				slug = "↳ " + slug
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "%d project(s) registered · %d with active runs\n", projectCount, activeCount)
}
