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
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"slices"
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

// IntentWorkflow and IntentDocID are the intent workflow's cross-cutting
// contract, the same shape as the idea pair: the workflow name written
// to run.json and the doc id for the single canvas stage. Intents are
// operator-authored standing direction parked on a project — runs in a
// single-stage workflow whose verbs never launch an agent, exactly like
// ideas. dash names them because it owns the INTENTS render and is the
// lowest leaf every caller (cli, serve) already depends on.
const (
	IntentWorkflow = "intent"
	IntentDocID    = "intent"
)

// IsCapture reports whether a workflow is one of the cheap
// operator-authored capture surfaces — ideas and intents. Both are
// single-stage workflows whose run *is* the capture: the canvas is the
// whole artifact, no agent ever runs, and edit/close are plain journal
// commits. Callers that treat the pair alike (cli's close pipeline,
// runopen's edit/close helpers, serve's action chips and edit form)
// route through here rather than re-spelling the two-way comparison.
func IsCapture(workflow string) bool {
	return workflow == IdeaWorkflow || workflow == IntentWorkflow
}

// CaptureDocID returns the single canvas doc id for a capture
// workflow, ok=false for anything else. The two ids happen to equal
// their workflow names, but the switch keeps the constants
// load-bearing: a caller that derived the doc id by reusing the
// workflow string would silently keep working if either pair diverged.
func CaptureDocID(workflow string) (string, bool) {
	switch workflow {
	case IdeaWorkflow:
		return IdeaDocID, true
	case IntentWorkflow:
		return IntentDocID, true
	}
	return "", false
}

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

// ChainWorkflow is the chain workflow's name as written to run.json.
// dash needs it to recognise a chain head in classify: a head has no
// stages, so it arrives trivially "done", but the operator verb is
// kick, not close (see there). The cli chain const aliases this so the
// string lives in exactly one place.
const ChainWorkflow = "chain"

// CompletedCap bounds the COMPLETED section. Finished runs are
// history — useful as recent context, not as a backlog — so the dash
// shows the newest N and lets the bureaucracy repo itself be the
// archive.
const CompletedCap = 5

// CompletedStep is how many more top-level completed runs each "show
// more" click on the web dash asks for. Fixed rather than doubling:
// the operator knows what the next click costs. It lives here beside
// the cap it extends even though only the web dash pages — the two
// numbers are one decision.
const CompletedStep = 25

// CompletedCutoff returns how many leading completed rows to render so
// that the newest topCap top-level rows are shown, each dragging its
// nested descendants (spawned runs) along for free. Nested rows never
// count against the cap, so a parent and its subtree are admitted or
// evicted as a unit. A negative topCap means unlimited; n comes back
// unchanged there and whenever the top-level count already fits.
//
// Shared by the CLI Render and the serve dash view so the terminal and
// the web page cap identically — the completed slice each holds is
// parent-then-descendants contiguous (BuildRows nests them), so counting
// top-level rows is the same walk on both. isNested reports whether
// row i renders under a parent.
func CompletedCutoff(n, topCap int, isNested func(i int) bool) int {
	if topCap < 0 {
		return n
	}
	tops := 0
	for i := range n {
		if isNested(i) {
			continue
		}
		if tops == topCap {
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
	BucketActiveRuns Bucket = iota // in-progress runs with a next stage
	BucketIntents                  // open intents: operator-authored standing direction, above the backlog
	// BucketChores holds all standing registrations, independent of their runs.
	// They have their own section and count neither as active work nor backlog.
	BucketChores
	BucketBacklog       // captured ideas, not yet promoted to a run
	BucketCompletedRuns // pushed or terminal runs, shown as "done"
	BucketNone          // not placed in any section (idea with an unrecognised status; in-progress non-idea run with no next-stage decision)
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
	// Depth is how many levels the row renders nested under a parent —
	// 0 for a top-level row, 1 for a completed spawned run (a tailed
	// pulse) re-attached under its spawner, 2+ for deeper spawn lineage
	// (a reflect under a pulse under its sdlc run). The renderer indents
	// by Depth and draws a "↳" connector for Depth ≥ 1.
	Depth int
	// Agent is true when the run was machine-opened — it carries a
	// MoE-Spawned-By edge. Independent of Depth: nesting disappears when
	// the spawner is off-board (pruned, capped out of COMPLETED), but the
	// run was still the machine's doing, and the operator's headline
	// question is "what did the agent add". Historically sound from day
	// one, because only machine verbs have ever written spawned_by.
	Agent bool
	// Control is the live and control overlay for a run row: owner phase,
	// hold and pending request ("handed off · waiting for operator"),
	// observed when the row was gathered. Empty when there is nothing
	// beyond the journal row to say.
	Control string
}

// NextDecision is the per-run "what's next" decision the caller
// pre-computes by asking its workflow registry. Stage is the bare
// stage name (e.g. "build"); for a perpetual Done run, callers set it
// to the repeatable stage dash should keep showing.
type NextDecision struct {
	Stage     string
	Done      bool
	Perpetual bool
}

type ChoreInput struct {
	Project string
	Name    string
	Status  string
	Summary string
	Due     bool
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
	// SessionWhenByRun dates an open session from its branch tip, keyed
	// "<project>/<slug>" like the maps above. Only capture runs read it
	// (see captureSessionRow): a session's commits land on the session
	// branch, so Index.LastActivity — which reads main — stays stale for
	// the whole open window and would date a minutes-old session in
	// days. Best-effort: a missing entry falls back to LastActivity.
	SessionWhenByRun map[string]time.Time
	Chores           []ChoreInput
	// Intents is the set of open intents (slug + title) to render in the
	// INTENTS section. Titles come off each canvas's first heading, so
	// the caller (GatherDashSnapshot) does the disk read; dash stays
	// pure. BuildRows filters by ProjectFilter and sorts by project then
	// slug so the render is stable.
	Intents []IntentInput
	// AwaitingAnswer and PendingInput hold the "<project>/<slug>" of the
	// runs with, respectively, a question waiting on the operator and
	// prose waiting on a turn. The caller reads the records (dash stays
	// off-disk); BuildRows turns each into a hint on the run's ACTIVE row
	// — `· ask?` for the half that needs a human, `· input` for the half
	// already in flight. Neither means the run is held: nothing in the
	// input record stops work.
	AwaitingAnswer map[string]bool
	PendingInput   map[string]bool
}

// IntentInput is one open intent for the INTENTS section: the project
// it's parked on, its slug, and the title read off the canvas's first
// heading (falls back to the slug when the canvas has no heading).
type IntentInput struct {
	Project string
	Slug    string
	Title   string
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
		if row, ok := captureSessionRow(md, in.SessionDocsByRun[runKey], in.SessionWhenByRun[runKey], last); ok {
			rows = append(rows, row)
		}
		b, note, stage, runningDoc := classify(md, byRunKey, in.Index, in.SessionDocsByRun[runKey], in.NextByRun)
		if b == BucketNone {
			continue
		}
		// An unanswered question is an action hint like `· close?`, so it
		// reads in the same slot — after the stage, before the workspace
		// label, which is a location rather than a thing to do. Pending
		// prose sits beside it without the question mark: it is state the
		// operator caused, not something asked of them.
		if b == BucketActiveRuns {
			if in.AwaitingAnswer[runKey] {
				note = note + " · ask?"
			}
			if in.PendingInput[runKey] {
				note = note + " · input"
			}
			// The reap's tombstone, and the sweep will not re-offer the
			// thread while it stands: the note is the kick floor's brake
			// (see reapHeldThread), released by any touch of the
			// operator's own on the thread. Still no `?`, because the
			// mark is state rather than a question — the same slot
			// `· input` sits in. It is here because a row that dropped a
			// turn and a row that was never reached look identical
			// otherwise. It describes the unfinished outcome, not its cause;
			// the run page points to the evidence.
			if md.Reaped != nil {
				note = note + " · unfinished"
			}
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
			Agent:      in.Index.SpawnedBy[runKey] != "",
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Bucket != rows[j].Bucket {
			return rows[i].Bucket < rows[j].Bucket
		}
		return rows[i].When.After(rows[j].When)
	})
	rows = nestSpawnedRuns(rows, in.Index)
	rows = nestPromotedIdeas(rows, in.Index)
	// Intents ride in from Inputs (not classify — they carry a title read
	// off-disk and never join the run board). Splice them after the ACTIVE
	// prefix so INTENTS sits above chores and backlog, sorted by project
	// then slug for a stable render.
	var intentRows []Row
	for _, it := range in.Intents {
		if in.ProjectFilter != "" && it.Project != in.ProjectFilter {
			continue
		}
		intentRows = append(intentRows, Row{
			Project: it.Project,
			Run:     it.Slug,
			Note:    it.Title,
			Bucket:  BucketIntents,
		})
	}
	sort.SliceStable(intentRows, func(i, j int) bool {
		if intentRows[i].Project != intentRows[j].Project {
			return intentRows[i].Project < intentRows[j].Project
		}
		return intentRows[i].Run < intentRows[j].Run
	})
	if len(intentRows) > 0 {
		insert := 0
		for insert < len(rows) && rows[insert].Bucket == BucketActiveRuns {
			insert++
		}
		rows = append(rows[:insert], append(intentRows, rows[insert:]...)...)
	}

	// Registrations sit between standing direction and captured ideas.
	chores := slices.Clone(in.Chores)
	sort.SliceStable(chores, func(i, j int) bool {
		if chores[i].Due != chores[j].Due {
			return chores[i].Due
		}
		if chores[i].Project != chores[j].Project {
			return chores[i].Project < chores[j].Project
		}
		return chores[i].Name < chores[j].Name
	})
	var choreRows []Row
	for _, c := range chores {
		if in.WorkflowFilter != "" {
			continue
		}
		if in.ProjectFilter != "" && c.Project != in.ProjectFilter {
			continue
		}
		choreRows = append(choreRows, Row{
			Project: c.Project,
			Run:     c.Name,
			Note:    c.Status + " · " + c.Summary,
			Bucket:  BucketChores,
		})
	}
	if len(choreRows) > 0 {
		insert := 0
		for insert < len(rows) && (rows[insert].Bucket == BucketActiveRuns || rows[insert].Bucket == BucketIntents) {
			insert++
		}
		rows = append(rows[:insert], append(choreRows, rows[insert:]...)...)
	}
	return rows, nil
}

func nestSpawnedRuns(rows []Row, idx *run.JournalIndex) []Row {
	if idx == nil || len(idx.SpawnedBy) == 0 {
		return rows
	}
	rowIdx := make(map[string]int, len(rows))
	for i := range rows {
		rowIdx[rows[i].Project+"/"+rows[i].Run] = i
	}
	// spawnerIdx returns the row index of row i's direct spawner, or -1 if
	// it has no SpawnedBy edge or that edge isn't on the board.
	spawnerIdx := func(i int) int {
		// SpawnedBy values are always qualified "<project>/<slug>" (the index
		// builder normalizes legacy bare values), so the spawner can live in a
		// different project than its child — look it up by the qualified key
		// directly rather than re-qualifying with the child's project.
		spawner := idx.SpawnedBy[rows[i].Project+"/"+rows[i].Run]
		if spawner == "" {
			return -1
		}
		if j, ok := rowIdx[spawner]; ok {
			return j
		}
		return -1
	}
	// folds reports whether row i is pulled under an ancestor: it is
	// completed, has an on-board direct spawner, and its upward SpawnedBy
	// walk is acyclic (so it terminates at a top-level ancestor). A cycle
	// in the journal-derived edges degrades to a top-level row, never a
	// hang or a dropped row.
	folds := func(i int) bool {
		if rows[i].Bucket != BucketCompletedRuns {
			return false
		}
		p := spawnerIdx(i)
		if p < 0 {
			return false
		}
		visited := map[int]bool{i: true}
		for p >= 0 {
			if visited[p] {
				return false // cycle — treat i as unresolved, top-level
			}
			visited[p] = true
			if rows[p].Bucket != BucketCompletedRuns {
				return true // reached a top-level ancestor, no cycle
			}
			p = spawnerIdx(p)
		}
		return true // chain ran off the board — top-level ancestor reached
	}
	// directChildren maps a spawner's row index to its folding children,
	// in incoming row order (recency-sorted, so newest-first).
	directChildren := make(map[int][]int)
	child := make([]bool, len(rows))
	for i := range rows {
		if folds(i) {
			child[i] = true
			directChildren[spawnerIdx(i)] = append(directChildren[spawnerIdx(i)], i)
		}
	}
	// openKids maps a spawner's row index to its open (non-completed)
	// children, in incoming row order (recency-sorted, so newest-first).
	// These rows never move — they are hinted on the spawner, not folded —
	// so this is a direct-edge lookup with no upward walk and no cycle
	// concern.
	openKids := make(map[int][]int)
	for i := range rows {
		if rows[i].Bucket == BucketCompletedRuns {
			continue
		}
		if p := spawnerIdx(i); p >= 0 {
			openKids[p] = append(openKids[p], i)
		}
	}
	// openHints is the note suffix row i carries for its open children.
	openHints := func(i int) string {
		var b strings.Builder
		for _, ci := range openKids[i] {
			b.WriteString(" · spawned → ")
			b.WriteString(rows[ci].Project)
			b.WriteString("/")
			b.WriteString(rows[ci].Run)
		}
		return b.String()
	}
	if len(directChildren) == 0 && len(openKids) == 0 {
		return rows
	}
	out := make([]Row, 0, len(rows))
	var emitSubtree func(i int, bucket Bucket, depth int)
	emitSubtree = func(i int, bucket Bucket, depth int) {
		for _, ci := range directChildren[i] {
			m := rows[ci]
			m.Depth = depth
			m.Bucket = bucket
			m.Note += openHints(ci)
			out = append(out, m)
			emitSubtree(ci, bucket, depth+1)
		}
	}
	for i := range rows {
		if child[i] {
			continue // emitted under its ancestor below
		}
		parent := rows[i]
		parent.Note += openHints(i)
		out = append(out, parent)
		emitSubtree(i, parent.Bucket, 1) // no-op when there are no folding children
	}
	return out
}

// nestPromotedIdeas folds a settled promotion edge into the same
// two-rung ladder spawn and chain lineage already render: the idea on
// top, the run it was promoted to nested beneath it as a "↳" row,
// instead of two unrelated flat COMPLETED rows with the relationship
// squeezed into the idea's "idea:promoted → x/y" note.
//
// The idea goes on top because that is what "↳" means everywhere else
// on the board — the ancestor above, later work beneath. The idea is
// the origin; the run is the continuation.
//
// The pair is anchored at the *successor's* recency slot, not the
// idea's. This is the one deliberate deviation from nestSpawnedRuns,
// and it is load-bearing: COMPLETED shows only the newest CompletedCap
// top-level rows, and an idea's last activity is its promote commit, so
// folding at the idea's slot would bury a run that merged weeks after
// promotion below the cap — the fresh merge would vanish from the
// default dash, a regression against the flat rendering. A spawner's
// activity tracks its children's, so spawn doesn't have this problem;
// promotion does, because the parent is prehistory rather than an
// ongoing story. The idea row keeps its own (older) When — the pair
// moves, the ages stay honest.
//
// Only a pair with both ends completed and top-level folds. A reopened
// idea is back in BACKLOG, and folding there would drag its dead
// destination run out of COMPLETED and into the backlog; the
// both-completed gate handles that without a workflow special case. An
// open successor keeps today's rendering — a top-level ACTIVE row, with
// the inline "idea:promoted → x/y" hint left on the idea, matching the
// spawn open-hint convention. An off-board successor keeps the bare
// "idea:promoted" fallback.
//
// Running after nestSpawnedRuns means an already-attached spawn subtree
// rides along: an idea → run → tailed-pulse lineage composes into a
// three-rung ladder for free, because the successor moves as a block —
// its own row plus the nested tail already hanging off it. The idea
// side needs no such handling: only pulse, reflect and chain runs ever
// write SpawnedBy, so nothing nests under an idea.
func nestPromotedIdeas(rows []Row, idx *run.JournalIndex) []Row {
	if idx == nil || len(idx.PromotedTo) == 0 {
		return rows
	}
	rowIdx := make(map[string]int, len(rows))
	for i := range rows {
		rowIdx[rows[i].Project+"/"+rows[i].Run] = i
	}
	// blockEnd returns one past the last row of top-level row i's block:
	// i itself plus the contiguous run of nested rows nestSpawnedRuns
	// attached beneath it.
	blockEnd := func(i int) int {
		e := i + 1
		for e < len(rows) && rows[e].Depth > 0 {
			e++
		}
		return e
	}
	// ideaOf maps a successor's row index to its promoting idea's row
	// index. Built by walking rows in order rather than ranging over the
	// PromotedTo map so a destination claimed by two ideas resolves to
	// the same one every render.
	ideaOf := make(map[int]int)
	claimed := make(map[int]bool)
	for i := range rows {
		if rows[i].Bucket != BucketCompletedRuns || rows[i].Depth != 0 {
			continue
		}
		dest := idx.PromotedTo[rows[i].Project+"/"+rows[i].Run]
		if dest == "" {
			continue
		}
		d, ok := rowIdx[dest]
		if !ok || d == i || claimed[d] {
			continue
		}
		if rows[d].Bucket != BucketCompletedRuns || rows[d].Depth != 0 {
			continue
		}
		ideaOf[d] = i
		claimed[d] = true
	}
	if len(ideaOf) == 0 {
		return rows
	}
	// moved marks each idea row skipped at its natural slot, because the
	// successor's slot re-emits it above the successor.
	moved := make([]bool, len(rows))
	for _, i := range ideaOf {
		moved[i] = true
	}
	out := make([]Row, 0, len(rows))
	for i := 0; i < len(rows); i++ {
		if moved[i] {
			continue
		}
		if ideaIdx, ok := ideaOf[i]; ok {
			idea := rows[ideaIdx]
			// The destination is a row of its own now, so the note's
			// " → <key>" suffix would say it twice.
			idea.Note = strings.TrimSuffix(idea.Note, " → "+rows[i].Project+"/"+rows[i].Run)
			out = append(out, idea)
			end := blockEnd(i)
			for k := i; k < end; k++ {
				m := rows[k]
				m.Depth++
				out = append(out, m)
			}
			i = end - 1
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

// captureSessionRow emits the extra ACTIVE row a capture run earns
// while an `edit --chat` session is open on its canvas. It is additive:
// the run keeps its standing row too — an intent stays in INTENTS via
// the Inputs.Intents splice, an in-progress idea still classifies to
// BACKLOG — because the parked entry and the live session are two true
// things at once: the direction is still parked, and someone is working
// on it right now.
//
// The note doubles as the resume instruction: "<workflow>:edit --chat"
// spells the exact verb that re-enters the session, and [running]
// carries the same "branch + worktree exist" liveness the rest of the
// dash means by it.
//
// In-progress only. A stray session branch on a closed or promoted
// capture run gets no row: an ACTIVE entry inviting a resume on a
// settled run advertises an action the operator shouldn't take.
func captureSessionRow(md *run.Metadata, openSessionDocs []string, sessionWhen, last time.Time) (Row, bool) {
	if !IsCapture(md.Workflow) || md.Status != run.StatusInProgress {
		return Row{}, false
	}
	docID, ok := CaptureDocID(md.Workflow)
	if !ok {
		return Row{}, false
	}
	if !slices.Contains(openSessionDocs, docID) {
		return Row{}, false
	}
	when := sessionWhen
	if when.IsZero() {
		when = last
	}
	return Row{
		Project:    md.Project,
		Run:        md.ID,
		Note:       md.Workflow + ":edit --chat [running]",
		Stage:      docID,
		RunningDoc: docID,
		When:       when,
		Bucket:     BucketActiveRuns,
	}, true
}

// classify decides which section a run lands in, what note to render,
// the bare next-stage name the factory art uses to pick a station
// glyph, and the doc with an open session that "wins" the liveness
// slot. Pure over its inputs — no disk I/O.
func classify(md *run.Metadata, byRunKey map[string]*run.Metadata, idx *run.JournalIndex, openSessionDocs []string, nextByRun map[string]NextDecision) (Bucket, string, string, string) {
	runKey := md.Project + "/" + md.ID
	prefix := md.Workflow + ":"
	if md.Workflow == IntentWorkflow {
		// Intents never join the run board. Open ones render in the
		// INTENTS section (fed from Inputs.Intents); closed ones drop out
		// entirely — an intent is standing direction, not run traffic, so
		// a retired intent is not history worth a COMPLETED row.
		return BucketNone, "", "", ""
	}
	if md.Workflow == IdeaWorkflow {
		switch md.Status {
		case run.StatusInProgress:
			note := prefix + "capture"
			if md.PromoteTo != "" {
				// The tag is the machine's license to start this idea, so
				// it belongs where the operator's eye already lands. Same
				// " → destination" grammar the promoted note below uses:
				// there the arrow points at the run that happened, here at
				// the workflow that may.
				note += " → " + md.PromoteTo
				// A design-only tag is a narrower licence — one design
				// turn, then a hold — and the board is where a survey
				// reads which bar to apply. Comma form matches the kick
				// line's "dynamic, design only".
				if md.DesignOnly {
					note += ", design only"
				}
			}
			return BucketBacklog, note, "", ""
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
		note := prefix + "merged"
		// Merged sdlc runs are reopen candidates for the same reason
		// closed ones are, and it matters more here: the merged row is
		// where the operator looks for "how do I keep going", and the
		// answer they used to reach for — re-typing the stage verb — is
		// now a refusal. Same unextended-chain gate as closed rows.
		if md.Workflow == "sdlc" && !hasBeenReopened(idx, md.Project, md.ID) {
			note += " · reopen?"
		}
		return BucketCompletedRuns, note, "", ""
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
		// sdlc-without-push hits the same shape.
		return BucketActiveRuns, prefix + "done · close?", "done", ""
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

// HumanDuration spells a duration as a length rather than an age: "12m",
// "3h 5m", "3d 2h". HumanAgo's suffix is baked ("3d ago"), which is the
// wrong tense for an uptime or a countdown, and the serve activity strip
// on both dashes needs all three of them side by side.
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}

// ModeCounts is how many projects sit at each non-default mode. Zero
// values mean an all-auto board, which is the norm and spends nothing on
// the cluster line.
type ModeCounts struct {
	Paused int
	Safe   int
}

// ServeCluster spells the live serve status both dashes carry —
// the CLI banner's tail and the web header's link — as one dotted
// cluster:
//
//	serve armed · up 4m · rex · next 15m · 1 failing
//	serve armed · up 4m · local pty · next 15m · 1 paused · 2 safe
//	serve browse-only · up 4m
//
// The two surfaces are contractually the same line, so the text is
// built once here rather than twice in a printf and a template. next
// is empty for a serve with no tick scheduled (an unarmed one never
// has); failing is dropped at zero, so a healthy board spends nothing
// on it. host is where jobs run ("rex", "local pty", "rex unusable"),
// empty on an unarmed serve, which runs none. The CLI's dead-serve
// state has no web counterpart (a dead serve serves no pages) and stays
// in the CLI.
//
// The mode counts are compact by exception for the same reason: they say
// how much of the board the clock is holding back, and on the ordinary
// all-auto board they say nothing at all. They ride here rather than on
// the run rows because the mode is a property of the clock, and the
// clock is what this cluster is about.
func ServeCluster(armed bool, up, host, next string, modes ModeCounts, failing int) string {
	cluster := "serve browse-only · up " + up
	if armed {
		cluster = "serve armed · up " + up
		if host != "" {
			cluster += " · " + host
		}
		if next != "" {
			cluster += " · next " + next
		}
	}
	if modes.Paused > 0 {
		cluster += fmt.Sprintf(" · %d paused", modes.Paused)
	}
	if modes.Safe > 0 {
		cluster += fmt.Sprintf(" · %d safe", modes.Safe)
	}
	if failing > 0 {
		cluster += fmt.Sprintf(" · %d failing", failing)
	}
	return cluster
}

// PushItem spells the origin item both dashes append to the serve
// cluster: whether the journal has reached GitHub, and what is wrong if
// it hasn't. "" when there is nothing to say, which keeps a board with
// no serve and nothing waiting exactly as quiet as it was.
//
//	3 unpushed
//	push failing 12m · 3 unpushed
//	pushed 2m ago
//
// Ahead-first, because the live count outranks anything recorded. A
// serve that pushed at 10:00 and has been refused since 10:05 has a
// perfectly true "pushed 5m ago" and three commits nobody can see;
// reading git at render is what stops the item from being confidently
// wrong. Conversely a recorded push is the only thing that can say a
// sync *happened* — an ahead-count of zero is silence, not confirmation.
//
// serve reports whether a live serve vouches for lastPush and
// failingSince. Without one they are stale or absent, so only the
// ahead-count is trustworthy and a quiet board stays silent.
//
// failing only shows while commits are actually waiting: once a landed
// push or a manual `moe sync` empties the queue the failure is history,
// and the loop's own recovery line has the log covered.
func PushItem(now time.Time, serve bool, ahead int, lastPush, failingSince time.Time) string {
	if ahead > 0 {
		item := fmt.Sprintf("%d unpushed", ahead)
		if serve && !failingSince.IsZero() {
			return "push failing " + HumanDuration(now.Sub(failingSince)) + " · " + item
		}
		return item
	}
	if !serve || lastPush.IsZero() {
		// Nothing waiting and nothing pushed yet in this serve's life. The
		// tempting item here is a standing "in sync", and it is the one
		// this deliberately doesn't render: an ahead-count of zero also
		// reads zero on a bureaucracy with no origin configured at all, so
		// the claim would be confident and, on a local-only box,
		// permanently false. A push that landed is the only positive this
		// can actually stand behind — and the first journal commit of the
		// session turns silence into one.
		return ""
	}
	return "pushed " + HumanAgo(now, lastPush)
}

// Plural spells "1 tick" / "2 ticks". Both dashes count down the
// heartbeat's cool-off in ticks, and "(1 tick(s) left)" is the kind of
// line that reads as unfinished.
func Plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
		case BucketBacklog:
			state.BacklogCount++
		case BucketCompletedRuns:
			state.CompletedCount++
		}
	}
	return state
}

// nestPrefix is the indent a row of the given Depth carries in front of
// its slug: nothing at top level, then a connector per level, each
// generation two columns further right so a reflect under a pulse under
// its sdlc run reads as a ladder rather than a flat list.
func nestPrefix(depth int) string {
	if depth < 1 {
		return ""
	}
	return strings.Repeat("  ", depth-1) + "↳ "
}

// rowPrefix is what a row carries in front of its slug: the lineage ladder.
func rowPrefix(r Row) string {
	return nestPrefix(r.Depth)
}

// Render prints the full dashboard: factory art, board sections
// (ACTIVE, INTENTS, CHORES, BACKLOG, COMPLETED), and the footer. tabwriter aligns
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
//
// Both art blocks arrive as plain runes (the same lines the web dash
// serves) and get their terminal colour here, behind the same TTY /
// NO_COLOR gate cliout.Printf uses — see style.go.
func Render(w io.Writer, now time.Time, histogram []string, rows []Row, projectCount, activeCount int, showAll bool, state FactoryState, r *rand.Rand) {
	RenderIndexed(w, now, histogram, rows, projectCount, activeCount, showAll, state, r)
}

// RenderIndexed is Render that also says where each row landed: lines[i]
// is the zero-based output line rows[i] was printed on, or -1 when the
// row is not shown (BucketNone, or a completed row past the cap). Every
// row renders as exactly one line, so a caller can select rows on the
// painted board without reading them back out of the text.
func RenderIndexed(w io.Writer, now time.Time, histogram []string, rows []Row, projectCount, activeCount int, showAll bool, state FactoryState, r *rand.Rand) (lines []int) {
	lines, _ = RenderIndexedArt(w, now, histogram, rows, projectCount, activeCount, showAll, StyledFactoryFrames(w, state, 1, r)[0])
	return lines
}

// RenderIndexedArt renders supplied, already styled factory art and returns
// its explicit line span's start, alongside the selectable row positions.
// The UI caches the styled frames and swaps only this span between gathers.
func RenderIndexedArt(w io.Writer, now time.Time, histogram []string, rows []Row, projectCount, activeCount int, showAll bool, art []string) (lines []int, artStart int) {
	lw := &lineWriter{w: w}
	fmt.Fprintln(lw)
	start := lw.n
	artStart = start + RenderOverview(lw, histogram, art)
	fmt.Fprintln(lw)
	offset := lw.n
	lines = RenderIndexedRows(lw, now, rows, projectCount, activeCount, showAll)
	for i := range lines {
		if lines[i] >= 0 {
			lines[i] += offset
		}
	}
	return lines, artStart
}

// RenderOverview renders the complete histogram and factory without outer
// blank padding. It returns the factory's zero-based line start.
func RenderOverview(w io.Writer, histogram, art []string) (artStart int) {
	if cliout.Enabled(w) {
		histogram = styleHistogramANSI(histogram)
	}
	for _, line := range histogram {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
	artStart = len(histogram) + 1
	for _, line := range art {
		fmt.Fprintln(w, line)
	}
	return artStart
}

// RenderIndexedRows renders only run sections and project totals. Positions
// are relative to this output; omitted rows have position -1.
func RenderIndexedRows(w io.Writer, now time.Time, rows []Row, projectCount, activeCount int, showAll bool) (lines []int) {
	lw := &lineWriter{w: w}
	w = lw
	lines = make([]int, len(rows))
	for i := range lines {
		lines[i] = -1
	}
	// place records that the next len(at) lines are those rows.
	place := func(at []int) {
		for k, i := range at {
			lines[i] = lw.n + k
		}
	}
	var active, intents, chores, backlog, completed []Row
	var activeAt, intentsAt, choresAt, backlogAt, completedAt []int // each row's index in rows
	for i, r := range rows {
		switch r.Bucket {
		case BucketActiveRuns:
			active, activeAt = append(active, r), append(activeAt, i)
		case BucketIntents:
			intents, intentsAt = append(intents, r), append(intentsAt, i)
		case BucketChores:
			chores, choresAt = append(chores, r), append(choresAt, i)
		case BucketBacklog:
			backlog, backlogAt = append(backlog, r), append(backlogAt, i)
		case BucketCompletedRuns:
			completed, completedAt = append(completed, r), append(completedAt, i)
		}
	}

	cliout.Printf(w, "ACTIVE (%d)\n", len(active))
	if len(active) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(activeAt)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range active {
			slug := rowPrefix(r) + r.Project + "/" + r.Run
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), noteWithControl(r))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// INTENTS renders even at zero: an empty list is itself a signal
	// (the robots are running unaimed), and the standing heading keeps
	// the noun discoverable. slug + title, no timestamp — intents are
	// parked, not dated.
	cliout.Printf(w, "INTENTS (%d)\n", len(intents))
	if len(intents) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(intentsAt)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range intents {
			fmt.Fprintf(tw, "  %s/%s\t%s\n", r.Project, r.Run, r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "CHORES (%d)\n", len(chores))
	if len(chores) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(choresAt)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range chores {
			fmt.Fprintf(tw, "  %s/%s\t%s\n", r.Project, r.Run, r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "BACKLOG (%d)\n", len(backlog))
	if len(backlog) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(backlogAt)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range backlog {
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s\n", r.Project, r.Run, HumanAgo(now, r.When), r.Note)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	// The terminal dash pages by scrollback, so it has only the two
	// stops the --all flag names: the default cap, or everything.
	topCap := CompletedCap
	if showAll {
		topCap = -1
	}
	shown := completed[:CompletedCutoff(len(completed), topCap, func(i int) bool { return completed[i].Depth > 0 })]
	if len(shown) < len(completed) {
		cliout.Printf(w, "COMPLETED (%d of %d)\n", len(shown), len(completed))
	} else {
		cliout.Printf(w, "COMPLETED (%d)\n", len(completed))
	}
	if len(shown) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(completedAt[:len(shown)])
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range shown {
			slug := rowPrefix(r) + r.Project + "/" + r.Run
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), noteWithControl(r))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "%d project(s) registered · %d with active runs\n", projectCount, activeCount)
	return lines
}

// Keep the control before the note so a narrow watch viewport preserves
// the operator's hold before clipping the workflow detail.
func noteWithControl(r Row) string {
	if r.Control == "" {
		return r.Note
	}
	if r.Note == "" {
		return "[" + r.Control + "]"
	}
	return "[" + r.Control + "]  " + r.Note
}

// lineWriter counts the lines written through it. Unwrap keeps the
// colour gates seeing the terminal behind it.
type lineWriter struct {
	w io.Writer
	n int
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	l.n += bytes.Count(p[:n], []byte{'\n'})
	return n, err
}

func (l *lineWriter) Unwrap() io.Writer { return l.w }
