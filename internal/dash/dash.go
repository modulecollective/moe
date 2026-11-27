// Package dash assembles the home-screen dashboard: runs, ideas, and
// the activity-histogram header. The cli/dash.go entry point gathers inputs
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
// open-session marker. The cli chat consts
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

// CompletedStep is how many more completed runs each "show
// more" click on the web dash asks for. Fixed rather than doubling:
// the operator knows what the next click costs. It lives here beside
// the cap it extends even though only the web dash pages — the two
// numbers are one decision.
const CompletedStep = 25

// CompletedCutoff returns the number of leading completed rows to render.
// A negative cap means unlimited.
func CompletedCutoff(n, cap int) int {
	if cap < 0 {
		return n
	}
	return min(n, cap)
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
	Workflow   string    // the run's workflow; empty on intent and chore rows, which are not runs on the board
	Note       string    // for runs: next stage name, or "done"; for backlog: idea title.
	Stage      string    // bare next-stage name for active runs (no workflow prefix); "" for backlog/completed. Serve's run page reads it.
	RunningDoc string    // doc with an open session that "wins" the liveness slot; "" when no session is open. Live reads it to decide whether today's histogram bar wears the star.
	When       time.Time // sort key within the section; most recent first.
	Bucket     Bucket
	// Agent is true when the run was machine-opened — it carries a
	// MoE-Spawned-By edge, even when the spawner is off the board.
	Agent bool
	// Control is the live and control overlay for a run row: owner phase,
	// hold and pending request ("handed off · waiting for operator"),
	// observed when the row was gathered. Empty when there is nothing
	// beyond the journal row to say.
	Control string
	// StageBlocked is supplied by workflow observation, after live suppression.
	StageBlocked bool
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
	// Pulse marks an intent a pulse opened (its run carries spawned_by).
	Pulse bool
	// Open and Landed count the work linked to the intent: live runs
	// and ideas, and merged runs. Both zero renders no counts.
	Open, Landed int
}

// intentNote is an INTENTS row's note: the title, the pulse mark, and
// the linked-work counts.
func intentNote(it IntentInput) string {
	note := it.Title
	if it.Pulse {
		note += " · pulse"
	}
	if it.Open > 0 || it.Landed > 0 {
		note += fmt.Sprintf("  (%d open · %d landed)", it.Open, it.Landed)
	}
	return note
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
			Workflow:   md.Workflow,
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
	annotateSpawnLineage(rows, in.Index)
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
			Note:    intentNote(it),
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

// annotateSpawnLineage keeps lineage in notes without changing row order or
// buckets. Completed children name their origin even when it is off-board;
// open children are hinted on their visible spawner, newest first.
func annotateSpawnLineage(rows []Row, idx *run.JournalIndex) {
	rowIdx := make(map[string]int, len(rows))
	for i := range rows {
		key := rows[i].Project + "/" + rows[i].Run
		rowIdx[key] = i
		if spawner := idx.SpawnedBy[key]; spawner != "" && rows[i].Bucket == BucketCompletedRuns {
			rows[i].Note += " · from " + spawner
		}
	}
	for i := range rows {
		key := rows[i].Project + "/" + rows[i].Run
		spawner := idx.SpawnedBy[key]
		if spawner == "" {
			continue
		}
		if rows[i].Bucket == BucketCompletedRuns {
			continue
		}
		if p, ok := rowIdx[spawner]; ok {
			rows[p].Note += " · spawned → " + key
		}
	}
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
// the bare next-stage name, and the doc with an open session that "wins" the liveness
// slot. Pure over its inputs — no disk I/O.
func classify(md *run.Metadata, byRunKey map[string]*run.Metadata, idx *run.JournalIndex, openSessionDocs []string, nextByRun map[string]NextDecision) (Bucket, string, string, string) {
	runKey := md.Project + "/" + md.ID
	prefix := md.Workflow + ":"
	if md.Workflow == IntentWorkflow {
		// Open intents render in the INTENTS section (fed from
		// Inputs.Intents), not here. A closed one is a finished goal,
		// and that is history: it gets a COMPLETED row, which is also
		// where the operator finds a pulse's close to undo it.
		if md.Status == run.StatusClosed {
			note := prefix + "closed"
			if md.ClosedBy != "" {
				note += " by pulse"
			}
			return BucketCompletedRuns, note, "", ""
		}
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
//	serve armed · up 4m · local pty · next 15m · 1 paused
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
// how much of the board is paused; safe mode remains in project detail.
// They ride here because the mode is a property of the clock.
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
	if failing > 0 {
		cluster += fmt.Sprintf(" · %d failing", failing)
	}
	return cluster
}

// PushItem spells the origin exceptions both dashes append to the serve
// cluster. Successful pushes remain in the activity record and logs.
// Failure only shows while commits are waiting; recovery empties the display.
func PushItem(now time.Time, serve bool, ahead int, failingSince time.Time) string {
	if ahead > 0 {
		item := fmt.Sprintf("%d unpushed", ahead)
		if serve && !failingSince.IsZero() {
			return "push failing " + HumanDuration(now.Sub(failingSince)) + " · " + item
		}
		return item
	}
	return ""
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

// Live reports whether any active run in rows has an open session — the
// test that caps today's histogram bar with a star. A project hub passes
// project-scoped rows, so its star is that project's liveness.
func Live(rows []Row) bool {
	for _, r := range rows {
		if r.Bucket == BucketActiveRuns && r.RunningDoc != "" {
			return true
		}
	}
	return false
}

// RenderIndexedRows renders only run sections and project totals. Positions
// are relative to this output; omitted rows have position -1.
func RenderIndexedRows(w io.Writer, now time.Time, rows []Row, projectCount, activeCount int) (lines []int) {
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
			slug := r.Project + "/" + r.Run
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), NoteWithControl(r))
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

	// The terminal board scrolls, so it shows every completed run.
	cliout.Printf(w, "COMPLETED (%d)\n", len(completed))
	if len(completed) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		place(completedAt)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range completed {
			slug := r.Project + "/" + r.Run
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", slug, HumanAgo(now, r.When), NoteWithControl(r))
		}
		tw.Flush()
	}
	fmt.Fprintln(w)

	cliout.Printf(w, "%d project(s) registered · %d with active runs\n", projectCount, activeCount)
	return lines
}

// NoteWithControl is a run row's note as the board prints it, with the
// blocked marker and control overlay. The control goes before the note so
// a narrow viewport keeps the operator's hold and clips the workflow detail.
func NoteWithControl(r Row) string {
	if r.StageBlocked {
		r.Note += " [blocked]"
	}
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
