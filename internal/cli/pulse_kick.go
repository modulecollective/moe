package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// The harness-owned step that follows grooming: whether the pulse kicks
// anything itself. Where a spawned run *lands* is not here — that is a
// `chain` claim the survey makes, twin reflects included.

// pulseSelfKick is the last step of a pulse: under a dynamic sweep,
// start every kickable parked thread on the board — the ones this sweep
// groomed first, then the rest. This is the only door to machine-rooted
// motion, and the dynamic rung is what holds it shut everywhere else.
//
// Kicking is the default and *parking* is the marked case, because the
// error ledger only ever ran one way. Three strandings in four days —
// an operator-advanced run, a judged chore, and a six-run thread a
// dynamic sweep groomed and silently left — and not one instance of a
// survey starting work it shouldn't have. Two prompt fixes each closed
// the previous miss and the drift came back, because a default that
// needs the survey to spend confidence to cause motion drifts toward
// stillness. So the survey no longer asks to move; it writes a `park`
// reason when it wants the operator to look first, and the reason is
// mandatory by shape — parking costs a generation, so it owes a
// sentence.
//
// The candidate set is the *board*, not the gate's `threads` list, and
// that is the fourth stranding's fix. A dynamic pulse is the retry
// vehicle the heartbeat re-offers when a ride fails; a thread that is
// already correctly ordered gives the survey nothing to groom, so a
// gate-keyed kick made the retry a sweep that structurally could not
// perform it. Two pulses surveyed the same stalled thread on
// 2026-08-13, each wrote that this sweep was itself a plausible
// vehicle, and each closed clean with the thread still sitting. So
// parking now means what the heartbeat's clean-sweep guard already
// believed it meant — a thread the survey held *with a reason* — and a
// thread the gate never mentions is started if it clears the floor.
//
// The guard is **dynamic consent upstream**. A hand-typed `moe pulse
// new` grooms and parks; only `--dynamic` — the rung `moe serve
// --dynamic` stands behind — licenses the machine to start something.
// That is what makes the surprise ride impossible by construction rather
// than by restraint: nothing but the clock the operator armed can turn a
// sweep into motion, and no other verb sweeps at all.
//
// There is deliberately no bound on *how many* generations this can run
// for. Each ride's commits move the journal tip, so the next heartbeat
// tick sweeps again and the machine walks until a survey finds nothing
// worth chaining. The rides run inside this child, so their commits land
// below the tip the sweep's exit records — what keeps that sentence true
// is the gate refusing both cursors when its window holds a ride commit
// (heartbeatGate.Swept, rideAuthored). Without that refusal the sweep
// stamps the post-ride board as already surveyed and the walk stops at
// its first generation, waiting on a human. Growth is clock-paced
// rather than recursive: a kicked
// ride no longer sweeps at its own tail, so this loop is the only walker
// and the board it snapshotted stays the board it walks. What holds the
// open-ended part safe is the guard above plus the ladder itself — each
// generation is real shipped work behind review and test, it shows up on
// the dash as it lands, and a Ctrl-C halts the ride. Escalation by
// visibility, not by counting.
//
// Kicks that do fire are stamped as dynamic rides, which is what marks
// their commits as the machine's (see walkConsent).
//
// And the thread's root must have **a settled design** — a disk fact
// about the work, not about who opened it. Lineage was the wrong proxy
// and said so twice in two days: keying on SpawnedBy alone stranded an
// operator-advanced run on 2026-07-22, and widening to "or advanced"
// stranded a pulse-nominated judged chore the same evening (chore opens
// are the one machine path that mints no SpawnedBy). Both runs were
// ready; neither had the lineage the guard wanted. So the question the
// admit asks is whether the design is settled and whether anyone is
// inside the run — see rootDesignSettled and openSessionStage.
//
// What stays with the operator is a root sitting at its first stage,
// in either of two shapes, and the skip line names which: nothing has
// run there yet — a promoted sketch, a hand-minted chain head composed
// over an afternoon — or the turn closed and nobody advanced it, which
// is the reopened run and the re-edit that out-dated its advance
// marker. Both are held; only one of them means the design never ran.
// Those have `!` and `!!!`. The survey's park decides what is
// *reasonable* to start — this is only the floor of what is safe.
//
// Under dynamic consent this step always reports: every kick, every
// park with its reason, every harness hold, and a line for the sweep
// that groomed nothing. It used to return before its first stderr line
// when no thread asked, so a sweep could end with no account of
// why nothing started — that silence is what made the 2026-07-25 park
// invisible. Every hold is one line, warn-only ethos.
//
// Every fact this step keys on comes out of the groom's final in-memory
// graph (see groomResult) — thread roots, the board's parked threads,
// and whether a root is still kickable.
// Re-reading the journal here would answer the same questions a second
// time against a state the sweep had already moved, and the enumeration
// makes that sharper rather than looser: it walks the same scan the
// groom placed against, so a root it names is a root the sweep just
// stamped. The one live read is the root's
// session branches, and that is the point: it asks whether the operator
// has a stage open *right now*, which no snapshot can say.
//
// The walk itself lives in planKick, so the same decisions the loop
// executes are the ones a dynamic sweep stamps onto its own canvas
// before this step runs. What stays here is execution: the stderr
// account, the floor's live re-check as each root is reached, and the
// child exits.
//
// The return is the first ordinary child failure, after every other
// eligible root has been offered, or exitInterrupted immediately. Zero
// means no ride started or every ride finished cleanly.
func pulseSelfKick(root string, groomed groomResult, stdout, stderr io.Writer) int {
	// No dynamic consent, nothing to say. Absence of a `park` is not a
	// request, so a hand-typed sweep parking everything it groomed is the
	// norm it has always been.
	if currentRideMode != rideDynamic {
		return 0
	}
	plan := planKick(root, groomed)
	if len(plan.Steps) == 0 {
		moePrintf(stderr, "pulse: kick: nothing parked — nothing to start\n")
		return 0
	}
	// The survey's own vetoes first, in plan order, so the account of what
	// the sweep held reads before the first ride's output starts
	// streaming through this same writer.
	var wanted []kickStep
	for _, step := range plan.Steps {
		if step.Park != "" {
			moePrintf(stderr, "pulse: kick: %s parked by the survey — %s\n", step.Root, step.Park)
			continue
		}
		wanted = append(wanted, step)
	}
	if len(wanted) == 0 {
		return 0
	}
	firstFailure := 0
	for _, step := range wanted {
		proj, runID, err := splitProjectRun(step.Root)
		if err != nil {
			moePrintf(stderr, "pulse: kick: malformed thread root %q: %v\n", step.Root, err)
			continue
		}
		if step.Hold != "" {
			moePrintf(stderr, "pulse: kick: %s %s\n", step.Root, step.Hold)
			continue
		}
		// Re-ask the floor. A ride earlier in this same loop runs for as
		// long as it takes, and the one question the floor asks that a
		// snapshot cannot answer — is a session open right now — can have
		// flipped by the time the loop arrives here. The stamped section
		// says "queued", not "will start", for exactly this window.
		if hold := kickFloorHold(root, step.Root, groomed); hold != "" {
			moePrintf(stderr, "pulse: kick: %s %s\n", step.Root, hold)
			continue
		}
		moePrintf(stderr, "pulse: kicking %s (dynamic)\n", step.Root)
		if code := chainKickRun(root, proj, runID, rideDynamic, stdout, stderr); code != 0 {
			moePrintf(stderr, "pulse: kick %s exited %d\n", step.Root, code)
			if code == exitInterrupted {
				return code
			}
			if firstFailure == 0 {
				firstFailure = code
			}
		}
	}
	return firstFailure
}

// kickStep is one thread root's place in a dynamic sweep's kick order,
// and what the sweep decided about it. Exactly one of Park and Hold is
// non-empty for a root that will not start; both empty means it cleared
// every guard and the loop will offer it a ride.
type kickStep struct {
	Root string
	// Gate is true for a root this sweep's gate groomed, false for one
	// the board enumeration found sitting parked.
	Gate bool
	// Park is the survey's own reason to hold this root, verbatim from
	// the gate. Non-empty means the floor was never consulted — the
	// survey's veto is upstream of it.
	Park string
	// Hold is the floor's reason, phrased as the tail of the kick's
	// stderr skip line so that line and the canvas section cannot drift
	// into two wordings for one disk fact.
	Hold string
}

// kickPlan is the whole of a dynamic sweep's kick decision: every root
// it will walk, in execution order, and why each one starts or doesn't.
//
// It exists so the sweep can *say* what it is about to do. A queued
// retry and a stranded run looked identical to the operator, because
// the kick reported only to the stderr of a process that outlives no
// terminal — three diagnosis runs in four days each re-derived the
// queue by hand. The canvas stamp renders this; pulseSelfKick executes
// it; neither builds its own answer.
type kickPlan struct {
	Steps []kickStep
}

// planKick walks the candidate roots and decides each one's fate,
// reading only the groom's final in-memory graph plus the roots' live
// session branches (see openSessionStage). No side effects: the caller
// decides whether to print it, stamp it, or execute it.
func planKick(root string, groomed groomResult) kickPlan {
	// Threads are keyed by root, and a park on any group that landed in
	// one holds the whole thread. Two groups routinely groom into the
	// same thread — one `onto` a run the other placed — and hand back two
	// groomedThreads with the same root. Under the old ask-field that
	// collision needed both groups to ask, so it stayed theoretical;
	// kicking by default makes it ordinary, and kicking one root twice
	// would start the ride and then start its finished remains.
	parked := make(map[string]string, len(groomed.threads))
	for _, th := range groomed.threads {
		if th.Park != "" && parked[th.Root] == "" {
			parked[th.Root] = th.Park
		}
	}
	// The groomed threads first, then the rest of the board. Order is the
	// whole of the precedence rule: a gate group that named a thread
	// enumeration would also find carries that thread's `park`, and the
	// dedupe keeps the first mention of each root.
	var plan kickPlan
	seen := make(map[string]bool, len(groomed.threads))
	add := func(rootKey string, gate bool) {
		if seen[rootKey] {
			return
		}
		seen[rootKey] = true
		plan.Steps = append(plan.Steps, kickStep{Root: rootKey, Gate: gate, Park: parked[rootKey]})
	}
	for _, th := range groomed.threads {
		add(th.Root, true)
	}
	for _, rootKey := range kickableThreadRoots(groomed.mds, groomed.byKey, groomed.graph, groomed.projectID) {
		add(rootKey, false)
	}

	for i := range plan.Steps {
		if plan.Steps[i].Park != "" {
			continue
		}
		plan.Steps[i].Hold = kickFloorHold(root, plan.Steps[i].Root, groomed)
	}
	return plan
}

// kickFloorHold asks the floor about one root and returns why it is
// held, or "" if it clears. The legs are the safety floor the survey's
// `park` sits on top of, and the returned phrase is the tail of the
// kick's stderr skip line.
//
// Called twice per root on the happy path — once when the plan is built,
// once when the loop reaches the root — and that is the point rather
// than an oversight: openSessionStage and the mode read are both live,
// and the roots behind the first ride can wait hours for their turn.
func kickFloorHold(root, threadRoot string, groomed groomResult) string {
	// The operator's standing cap first: under paused or safe the answer
	// is theirs, and the structural legs below are describing a start
	// that isn't going to happen either way.
	if hold := kickModeHold(root, threadRoot, groomed); hold != "" {
		return hold
	}
	// A group can be groomed onto a thread whose head has already
	// shipped — `onto` admits a settled anchor on purpose, that being
	// the queue-jump case — and the root then walks back to a merged
	// run. Kicking one would ride a finished thread from its finished
	// end. ChainChildLive is the same terminal-or-missing test every
	// other edge reader applies.
	md := groomed.byKey[threadRoot]
	if md == nil || !run.ChainChildLive(threadRoot, groomed.byKey) {
		return "heads a thread that has already settled — skipping"
	}
	if settled, turnClosed := rootDesignSettled(root, md, groomed.idx); !settled {
		return "is waiting at its first stage with " + designHeldReason(turnClosed) +
			" — the operator holds the trigger"
	}
	// A dead machine turn on the thread. After the design leg so a root
	// held for an unsettled design keeps its more specific reason, and
	// ordering against occupancy is moot — the reap deleted the session
	// branch openSessionStage reads.
	if hold := reapHeldThread(threadRoot, groomed.byKey, groomed.graph, groomed.idx); hold != "" {
		return hold
	}
	if stage := openSessionStage(root, md); stage != "" {
		return "has a live session at " + stage + " — skipping"
	}
	return ""
}

// renderKickSection renders a plan as the `## Kick` section a dynamic
// sweep stamps onto its own canvas at close. The pulse canvas is what
// the operator reads when a thread looks stuck, and until now it stopped
// at "parked, nothing to groom" — true of the board and silent about the
// queue the same sweep was about to run.
//
// Deliberately not a promise. The wording is "queued", the header says
// the floor is re-checked, and a root the loop then holds prints its
// skip line to stderr as it always did. A section that claimed execution
// would be a second decoy artifact where the first one cost three runs.
func renderKickSection(plan kickPlan) string {
	var b strings.Builder
	b.WriteString("## Kick\n\n")
	if len(plan.Steps) == 0 {
		b.WriteString("Nothing parked — nothing to start.\n")
		return b.String()
	}
	b.WriteString("The order this sweep handed its kick loop. Queued is a plan, not a\n")
	b.WriteString("promise: the floor is re-checked as each root is reached, so a live\n")
	b.WriteString("session or a settled thread can still hold one here.\n\n")
	for i, step := range plan.Steps {
		source := "parked board"
		if step.Gate {
			source = "gate thread"
		}
		fmt.Fprintf(&b, "%d. %s — %s, %s\n", i+1, step.Root, source, kickStepOutcome(step))
	}
	return b.String()
}

// kickStepOutcome is one step's fate in the section's vocabulary,
// reusing the stderr phrasing wherever there is one to reuse.
func kickStepOutcome(step kickStep) string {
	switch {
	case step.Park != "":
		return "parked by the survey — " + step.Park
	case step.Hold != "":
		return step.Hold
	default:
		return "queued — floor re-checked at start"
	}
}

// kickableThreadRoots returns the thread roots in a project that could
// be started: every in-progress chainable run walked back to its root,
// deduped, sorted so the answer doesn't ride on scan order.
//
// This is a shared *test*, not a shared answer, and the difference is
// load-bearing. Both callers ask "which roots are worth looking at" and
// then respond differently: the kick offers each one to the floor and
// prints a line per hold, while the heartbeat's parked leg asks whether
// a *sweep* could cause motion, which lets it look past a design-held
// head into the members queued behind it (see parkedKickableThread) —
// grooming work for the survey, and not something the kick may start.
// Before this the two seams enumerated the board separately, and the
// heartbeat re-offering a thread the kick could not reach is exactly
// how a run stalled for two days.
//
// Two shapes are dropped here because they are never a candidate on
// either side, whatever the floor would say:
//
//   - a root whose thread has already settled. `onto` admits a merged
//     anchor on purpose — that is the queue-jump case — so a live
//     thread can be rooted at a finished run, and riding it would start
//     it from its finished end.
//   - an operator-minted `chain` head. That head is the staging fence
//     the groom's stagingFenced honours, and a stageless head never
//     clears the settled-design admit anyway; dropping it here is what
//     makes the hold silent rather than a line per sweep about a normal
//     state.
func kickableThreadRoots(mds []*run.Metadata, byKey map[string]*run.Metadata, graph *run.ChainGraph, projectID string) []string {
	if graph == nil {
		// A groom that bailed before building one. Nothing to enumerate,
		// and the next sweep re-derives the board for free.
		return nil
	}
	var roots []string
	seen := make(map[string]bool, len(mds))
	for _, md := range mds {
		if md.Project != projectID || md.Status != run.StatusInProgress || !chainableWorkflow(md.Workflow) {
			continue
		}
		rootKey := graph.Root(md.Project + "/" + md.ID)
		if seen[rootKey] {
			continue
		}
		seen[rootKey] = true
		rootMd := byKey[rootKey]
		if rootMd == nil || !run.ChainChildLive(rootKey, byKey) {
			continue
		}
		if rootMd.Workflow == chainWorkflow && rootMd.SpawnedBy == "" {
			continue
		}
		roots = append(roots, rootKey)
	}
	sort.Strings(roots)
	return roots
}

// rootDesignSettled reports whether md's design is settled — the one
// structural readiness fact the kick admits on. "Parked at code, human
// runs are good to go" passes; an operator-promoted sketch still
// waiting at design with only its seed does not. Three legs, any of:
//
//   - **Past its first stage.** Mechanically (stageSatisfied) that
//     needs a first-stage work-turn *and* either an advance marker at
//     least as recent as it, or a downstream stage's turn. So this leg
//     is exactly "the operator advanced it, or real downstream work
//     exists" — and a run whose design merely closed still reads as
//     waiting at design, which preserves AdvancedTo's "a canvas merely
//     complete is not consent to proceed" by mechanism rather than by a
//     kick-side special case. The staleness rule (a marker out-dated by
//     a re-edit) is inherited the same way.
//   - **Machine-minted** (SpawnedBy) — the seed is a design baked by
//     the spawning run.
//   - **Chore-rooted** — the seed is the chore's operator-authored
//     prompt.md, so standing intent is a settled design by
//     construction. Its own leg because openChoreInProcess is the one
//     machine-open path that stamps no SpawnedBy, and it can't be made
//     to: autoOpenDueChores runs before the pulse run is minted, and a
//     nomination landing on an already-open chore run inherits the old
//     open commit.
//
// An empty ladder (the `chain` workflow's placeholder head) is never
// past-first. A root with nothing left to run is past-first by the same
// comparison and reports its own reason downstream — a terminal one has
// already fallen to the settled-thread guard above, and a live one owes
// its ride only its members, which is chainKickRun's nothing-pending
// branch to say.
//
// turnClosed is the caller's reporting bit and only means anything when
// settled is false: it says a first-stage work-turn has landed, so the
// hold is "worked, not advanced" rather than "never worked". It lives
// here because the first stage it asks about is the one the past-first
// comparison just resolved; deriving it at the call site would look the
// workflow up a second time to answer half a question.
func rootDesignSettled(root string, md *run.Metadata, idx *run.JournalIndex) (settled, turnClosed bool) {
	if md.SpawnedBy != "" {
		return true, false
	}
	if idx != nil && idx.ChoreByRun[md.Project+"/"+md.ID] != "" {
		return true, false
	}
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return false, false
	}
	stages := w.Stages()
	if len(stages) == 0 {
		return false, false
	}
	stage, _, err := w.NextWithIndex(root, md, idx)
	if err != nil {
		return false, false
	}
	if stage != stages[0] {
		return true, false
	}
	// Held at the first stage: a turn that landed and wasn't advanced
	// (no marker, a marker a re-edit out-dated, a failed gate) reads
	// differently to the operator than a stage nothing has run at. Same
	// workTurnTime stageSatisfied just consulted.
	when, err := workTurnTime(root, md.Project, md.ID, stages[0], idx)
	if err != nil {
		return false, false
	}
	return false, !when.IsZero()
}

// operatorMarked reports whether a run carries an explicit operator mark
// — the admit predicate `safe` mode holds the clock to, and the one
// question that separates "the machine may propose this" from "the
// machine may start this".
//
// Every leg is a disk fact about the *work*, not about who opened the
// run. Keying on lineage is the proxy that stranded runs twice in July,
// and rootDesignSettled next door is the same lesson already learned
// once. One shared function, called by both the heartbeat's parked-leg
// pre-ask and the kick's own admit, for the anti-drift reason
// kickableThreadRoots exists: two seams answering the same question
// separately is how a run sat for two days on 2026-08-13.
//
// Three shapes admit:
//
//   - **A valid advance marker.** The operator hit `a` at a chain prompt
//     or clicked Advance. That is recorded permission to carry the
//     thread forward, and because operatorAdvancedStage asks about
//     whatever stage the run is waiting at, it admits a mid-ladder
//     resume as readily as a design the operator signed off. Staleness
//     rules are inherited whole — a marker a re-edit out-dated stops
//     counting, exactly as it stops satisfying the stage.
//   - **Chore-rooted.** The seed is the chore's operator-authored
//     prompt.md, so standing intent is an operator mark by construction.
//   - **Tagged-idea lineage.** A live idea carrying a workflow tag (the
//     tag is the licence — untagged means human), and the run a
//     promotion made out of one. Both read the same operator act, one
//     before it is spent and one after.
//
// What that leaves out under safe, deliberately: a survey-invented spawn
// nobody has looked at, and a thread whose only progress is machine
// work-turns. Both stay on the board, held with a reason, until the
// operator advances, tags, or kicks.
//
// The advance leg goes last because it forks git (a session-branch
// probe) and the other two are map lookups over a scan the caller
// already holds.
func operatorMarked(root string, md *run.Metadata, mds []*run.Metadata, idx *run.JournalIndex) bool {
	if md == nil {
		return false
	}
	key := md.Project + "/" + md.ID
	if idx != nil && idx.ChoreByRun[key] != "" {
		return true
	}
	if md.Workflow == dash.IdeaWorkflow && md.PromoteTo != "" {
		return true
	}
	if idx != nil {
		for _, other := range mds {
			if other.Workflow != dash.IdeaWorkflow || other.PromoteTo == "" {
				continue
			}
			if idx.PromotedTo[other.Project+"/"+other.ID] == key {
				return true
			}
		}
	}
	_, _, advanced := operatorAdvancedStage(root, md, idx)
	return advanced
}

// pendingInputOnThread reports whether any live member of the thread
// carries operator prose no turn has picked up yet — the safe-mode leg
// the input record adds.
//
// It is a mark for the same reason the others are: the operator wrote
// direction at a concrete run, and requiring a second approval before
// that run may move would break the phone-to-motion loop the record
// exists for. Thread-scoped rather than root-scoped because that is
// where the prose lands — the run whose next agent needs it is routinely
// queued behind the head.
//
// Delivery consumes the licence: once the turn that read the note stamps
// it, the thread is back to needing an ordinary mark, and another note
// re-arms it. That is deliberately tighter than a licence that lasts the
// run's life — one note buys one push, not standing permission.
//
// It is emphatically *not* an advance marker. It satisfies no stage and
// chooses no successor. The next sweep still has to pass the ordinary
// stage and kick floors.
func pendingInputOnThread(root, threadRoot string, groomed groomResult) bool {
	if groomed.graph == nil {
		return false
	}
	for _, key := range groomed.graph.Thread(threadRoot) {
		proj, runID, err := splitProjectRun(key)
		if err != nil {
			continue
		}
		f, err := input.Load(root, proj, runID)
		if err != nil {
			continue
		}
		if len(f.Pending()) > 0 {
			return true
		}
	}
	return false
}

// threadOperatorTouch returns the committer time of the most recent
// commit on the thread that the machine did not write — the movement
// the reap hold releases on. Zero time when nobody has touched any live
// member by hand.
//
// Thread-scoped rather than run-scoped because that is where operator
// prose actually lands: the same reasoning pendingInputOnThread is
// built on. A note added at a member queued behind the head is movement
// on the thread the head belongs to.
func threadOperatorTouch(threadRoot string, byKey map[string]*run.Metadata, graph *run.ChainGraph, idx *run.JournalIndex) time.Time {
	var touched time.Time
	if graph == nil || idx == nil {
		return touched
	}
	for _, key := range graph.Thread(threadRoot) {
		if !run.ChainChildLive(key, byKey) {
			continue
		}
		if t := idx.LastOperatorActivity[key]; t.After(touched) {
			touched = t
		}
	}
	return touched
}

// reapHeldReason names why a run whose last machine turn was reaped is
// still held, in the one vocabulary every surface that reports the hold
// uses: the kick's skip line on stderr, the `## Kick` section it
// renders, and the chain-state block's annotation. Returns "" when the
// thread's latest operator touch has released it, or when the run
// carries no tombstone at all.
//
// A headless stage that refuses because the work needs an operator
// exits with its canvas unwritten, the heartbeat's reap tombstones the
// dead session, and the run parks — and before this the next sweep
// kicked the same stage again, one full turn of burn per sweep until a
// human noticed. So the note is a brake, and `touched` is the release.
//
// The comparison is strictly After on purpose. Reaped.At has second
// precision, so a movement landing inside the reap's own second reads
// as held — the safe direction, and the next touch releases.
//
// An unparseable At holds. It is the operator's brake, and the
// fail-open direction the rest of the kick takes would spend a stage
// turn in exactly the wrong place; the parse problem rides in the
// phrase so the skip line says what is wrong with the note.
func reapHeldReason(md *run.Metadata, touched time.Time) string {
	if md == nil || md.Reaped == nil {
		return ""
	}
	when := "at " + md.Reaped.At
	if at, err := time.Parse(time.RFC3339, md.Reaped.At); err != nil {
		when = "at an unreadable time (" + md.Reaped.At + ")"
	} else if touched.After(at) {
		return ""
	}
	return "its " + md.Reaped.Doc + " turn died and was reaped " + when +
		" — an operator touch on the thread releases it"
}

// reapHeldThread returns the floor's phrasing for a thread holding a
// reaped member no operator movement has released, or "" when the
// thread clears. Any live member holds the whole thread: the kick
// evaluates a thread at its root, and a member's dead turn is not
// something a ride from the head may walk into.
//
// Why this cannot loop is a property of what releases it, not of a
// counter. Every journal commit the refusal cycle lands is
// machine-stamped — the tombstone carries MoE-Consent explicitly, groom
// placements and spawns carry theirs — so LastOperatorActivity cannot
// move on the machine's own account (see the map's doc). And each
// failed retry re-arms at a later Reaped.At than the movement that
// licensed it, so k operator touches buy at most k retries.
func reapHeldThread(threadRoot string, byKey map[string]*run.Metadata, graph *run.ChainGraph, idx *run.JournalIndex) string {
	if graph == nil {
		return ""
	}
	touched := threadOperatorTouch(threadRoot, byKey, graph, idx)
	for _, key := range graph.Thread(threadRoot) {
		if !run.ChainChildLive(key, byKey) {
			continue
		}
		if why := reapHeldReason(byKey[key], touched); why != "" {
			return "is held — " + why
		}
	}
	return ""
}

// kickModeHold asks the project's mode about one root and returns why it
// holds, or "" when the mode lets it through. Empty for every invocation
// the operator typed: the mode binds the clock, and a typed sweep is
// consent whatever the standing config says.
//
// The read is per call rather than once per plan, and that is the point.
// The gate decided to sweep some minutes before this child reached its
// kick, and a ride earlier in the same loop can run for hours — so a
// mode the operator flipped in either window binds the roots that
// haven't started yet. A pause is meant to take effect when it's typed.
//
// An unreadable project.json holds rather than starts: this is the one
// read in the kick whose failure mode is "the operator's brake is
// invisible", and the fail-open direction the rest of the sweep takes
// would spend it in exactly the wrong place.
func kickModeHold(root, threadRoot string, groomed groomResult) string {
	if !clockInvoked {
		return ""
	}
	mode, err := project.ReadMode(root, groomed.projectID)
	if err != nil {
		return "is held — could not read the project's mode: " + err.Error()
	}
	switch mode {
	case project.ModePaused:
		return "is held by paused mode — the project starts nothing on its own"
	case project.ModeSafe:
		if !operatorMarked(root, groomed.byKey[threadRoot], groomed.mds, groomed.idx) &&
			!pendingInputOnThread(root, threadRoot, groomed) {
			return "is held by safe mode — no operator mark (an advance, a tag, a chore, or an undelivered note licenses it)"
		}
	}
	return ""
}

// designHeldReason names why an unsettled root is held, in the one
// vocabulary every surface that reports the hold uses: the kick's skip
// line on stderr, and the held-head annotation the survey reads in the
// chain-state block. Two surfaces describing the same disk fact in two
// wordings is how an agent learns to read them as two different states.
// Takes rootDesignSettled's turnClosed bit, and means nothing without
// it — settled roots are not held at all.
func designHeldReason(turnClosed bool) string {
	if turnClosed {
		return "its turn closed but not advanced"
	}
	return "only a seed"
}

// openSessionStage returns the stage md has a live session branch at,
// or "" for none. This is the occupancy check: a settled design says
// the run is ready, and this says nobody is already inside it.
//
// Branches are the only signal that works. run.json's session id
// reaches main only when the turn commits (commitSessionStart writes it
// on the session branch), so a mid-session run reads as having no
// session at all — the exact window the check exists to close. Refs
// live in the common dir, so HasRef reads true from any worktree, and
// Close/Abandon both delete the branch.
//
// A run that died mid-stage keeps its branch, and this cannot tell that
// leftover from an operator's live session — so such a root is held,
// one skip line per sweep, until the session is resumed or abandoned.
// Conservative on purpose; the line is the recovery signpost.
func openSessionStage(root string, md *run.Metadata) string {
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return ""
	}
	for _, stage := range w.Stages() {
		if git.HasRef(root, "refs/heads/"+session.BranchName(md.Project, md.ID, stage)) {
			return stage
		}
	}
	return ""
}
