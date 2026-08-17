package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/git"
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
// worth chaining. Growth is clock-paced rather than recursive: a kicked
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
// held, or "" if it clears. The three legs are the safety floor the
// survey's `park` sits on top of, and the returned phrase is the tail of
// the kick's stderr skip line.
//
// Called twice per root on the happy path — once when the plan is built,
// once when the loop reaches the root — and that is the point rather
// than an oversight: openSessionStage is a live read, and the roots
// behind the first ride can wait hours for their turn.
func kickFloorHold(root, threadRoot string, groomed groomResult) string {
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
