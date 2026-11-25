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

// pulseQuietWindow is the operator's quiet tick, matching serve's baked
// heartbeat cadence. The heartbeat holds a project for it, but the one
// sweep can still see that project's board, so the ride asks again.
const pulseQuietWindow = 5 * time.Minute

// pulsePick is the survey's `next`, resolved against the board.
type pulsePick struct {
	Key    string
	Why    string
	Repair bool
	// Idea says Key is a tagged idea, promoted only once it is ridden.
	Idea bool
	// Hold is the floor's reason not to start it, phrased as the tail of
	// the ride's stderr line and of the canvas's `## Next` section.
	Hold string
	// OneStage bounds the ride to one stage: a design-only spawn's seed
	// turn, or the turn an operator note buys.
	OneStage bool
}

// planPick resolves the gate's `next`. It has no side effects, so the
// sweep can stamp what it is about to do before doing it. minted maps a
// slug the same gate opened to the id the harness actually gave it.
func planPick(sc *pulseScan, next *pulseNext, minted map[string]string, prChecks []ownedPRChecks) *pulsePick {
	if next == nil {
		return nil
	}
	pick := &pulsePick{Key: next.Run, Why: next.Why, Repair: next.Repair != ""}
	if id, ok := minted[next.Run]; ok {
		proj, _, _ := splitProjectRun(next.Run)
		pick.Key = proj + "/" + id
	}
	md := sc.byKey[pick.Key]
	if md == nil {
		pick.Hold = "names no run on the board — starting nothing"
		return pick
	}
	switch {
	case pick.Repair:
		p, ok := repairEvidence(prChecks, pick.Key)
		if !ok {
			pick.Hold = "is held — no actionable PR evidence from this survey"
			return pick
		}
		_, pick.Hold = ciRepairEligibility(sc, p)
	case md.Workflow == dash.IdeaWorkflow:
		pick.Idea = true
		pick.Hold = ideaPickHold(sc.root, md)
	case !operatorCascades(md.Workflow) || md.Status != run.StatusInProgress:
		pick.Hold = fmt.Sprintf("is a %s %s run, not parked work — starting nothing", md.Status, md.Workflow)
	default:
		pick.Hold = kickFloorHold(sc.root, pick.Key, sc)
		pick.OneStage = oneStageRide(sc.root, pick.Key, sc)
	}
	return pick
}

// ideaPickHold is why an idea cannot be promoted and ridden. The tag is
// the licence; the root project has no source for a run to work in.
func ideaPickHold(root string, md *run.Metadata) string {
	switch {
	case md.Status != run.StatusInProgress:
		return "is a " + md.Status + " idea — starting nothing"
	case md.PromoteTo == "":
		return "is an untagged idea — the operator holds the trigger"
	case md.Project == project.RootID:
		return "is a top-level idea with no source — file its work into a project"
	}
	return quietWindowHold(root, md, modeHold(root, md))
}

// modeHold is the project's standing cap on the clock. Empty for anything
// the operator typed. An unreadable project.json holds: the failure would
// otherwise make the operator's brake invisible.
func modeHold(root string, md *run.Metadata) string {
	if !clockInvoked {
		return ""
	}
	mode, err := project.ReadMode(root, md.Project)
	if err != nil {
		return "is held — could not read the project's mode: " + err.Error()
	}
	if mode == project.ModePaused {
		return "is held by paused mode — the project starts nothing on its own"
	}
	return ""
}

// quietWindowHold keeps a clock ride off a board the operator touched
// within the last tick. hold passes through when it is already set. A run
// with an undelivered note is exempt: the window protects work still being
// arranged, and a note is the operator pushing this run.
func quietWindowHold(root string, md *run.Metadata, hold string) string {
	if hold != "" || !clockInvoked || pendingInputOnRun(root, md) {
		return hold
	}
	if operatorActedSince(root, md.Project, time.Now().Add(-pulseQuietWindow)) {
		return "is held — an operator commit landed in " + md.Project + " within the last tick"
	}
	return ""
}

// pulseRide is the last step of a dynamic sweep: start the one run the
// survey picked and nothing else. A hand-typed sweep never rides.
func pulseRide(root string, pick *pulsePick, prChecks []ownedPRChecks, stdout, stderr io.Writer) int {
	if currentRideMode != rideDynamic {
		return 0
	}
	if pick == nil {
		moePrintf(stderr, "pulse: next: nothing picked — starting nothing\n")
		return 0
	}
	if pick.Hold != "" {
		moePrintf(stderr, "pulse: next: %s %s\n", pick.Key, pick.Hold)
		return 0
	}
	if pick.Repair {
		return combinePulseExit(0, kickCIRepair(root, pick.Key, prChecks, stdout, stderr))
	}
	key := pick.Key
	proj, runID, err := splitProjectRun(key)
	if err != nil {
		moePrintf(stderr, "pulse: next: malformed run %q: %v\n", key, err)
		return 1
	}
	if pick.Idea {
		m := &pulseMinter{root: root, projectID: proj, spawnedBy: currentSweep}
		id, dup := m.promoteIfTaggedIdea(runID, pulseRunSpec{}, stdout, stderr)
		if dup {
			sayWhyTaggedIdeaStayed(root, proj, runID, stderr)
		}
		if id == "" {
			return 0
		}
		runID, key = id, proj+"/"+id
	}
	// Re-ask against the board as it stands now: the survey took minutes,
	// and a session opened or a mode flipped in that time binds this start.
	sc, ok := newPulseScan(root)
	if !ok {
		moePrintf(stderr, "pulse: next: could not read the board — not starting %s\n", key)
		return 1
	}
	if hold := kickFloorHold(root, key, sc); hold != "" {
		moePrintf(stderr, "pulse: next: %s %s\n", key, hold)
		return 0
	}
	oneStage := oneStageRide(root, key, sc)
	ride := "dynamic"
	if oneStage {
		ride = "dynamic, one stage"
	}
	moePrintf(stderr, "pulse: riding %s (%s) — %s\n", key, ride, pick.Why)
	code := rideRun(root, proj, runID, rideDynamic, oneStage, stdout, stderr)
	switch code {
	case 0:
	case exitYielded:
		moePrintf(stderr, "pulse: ride %s yielded to operator control\n", key)
	case exitParked:
		// rideParked said where; it alone knows the stage.
	default:
		moePrintf(stderr, "pulse: ride %s exited %d\n", key, code)
	}
	return combinePulseExit(0, code)
}

// kickFloorHold asks the floor about one run and returns why it is held,
// or "" if it clears. Mode comes first: under paused or safe the answer is
// the operator's, whatever the structural legs would say.
func kickFloorHold(root, key string, sc *pulseScan) string {
	md := sc.byKey[key]
	if md == nil || md.Status != run.StatusInProgress {
		return "has already settled — skipping"
	}
	if hold := quietWindowHold(root, md, kickModeHold(root, key, sc)); hold != "" {
		return hold
	}
	if settled, turnClosed := rootDesignSettled(root, md, sc.idx); !settled {
		// An unsettled run passes only on unsettledAdmission: a
		// design-only spawn's seed turn, one turn for an operator note, or
		// a resume once the prerequisite its blocked gate names settles.
		if adm, _ := unsettledAdmission(sc, md, turnClosed); adm == admitNone {
			if md.DesignOnly {
				return designOnlyHeldReason
			}
			return "is waiting at its first stage with " + designHeldReason(turnClosed) +
				" — the operator holds the trigger"
		}
	}
	if why := reapHeldReason(md, sc.idx.LastOperatorActivity[key]); why != "" {
		return "is held — " + why
	}
	if stage := openSessionStage(root, md); stage != "" {
		return "has a live session at " + stage + " — skipping"
	}
	// A settled session has no branch, but its invocation may still be at
	// the prompt.
	if hold := runOwnerHold(root, md); hold != "" {
		return hold
	}
	if hold := controlHoldPhrase(root, md); hold != "" {
		return hold
	}
	return reviewLoopHold(root, md)
}

// kickModeHold is modeHold plus safe mode's mark requirement.
func kickModeHold(root, key string, sc *pulseScan) string {
	md := sc.byKey[key]
	if hold := modeHold(root, md); hold != "" || !clockInvoked {
		return hold
	}
	mode, err := project.ReadMode(root, md.Project)
	if err != nil {
		return "is held — could not read the project's mode: " + err.Error()
	}
	if mode == project.ModeSafe && !operatorMarked(root, md, sc.mds, sc.idx) && !pendingInputOnRun(root, md) {
		return "is held by safe mode — no operator mark (an advance, a tag, a chore, or an undelivered note licenses it)"
	}
	return ""
}

// renderNextSection renders the `## Next` section a dynamic sweep stamps
// onto its own canvas at close. It is a plan rather than a record: the
// floor is asked again when the ride starts.
func renderNextSection(pick *pulsePick) string {
	var b strings.Builder
	b.WriteString("## Next\n\n")
	switch {
	case pick == nil:
		b.WriteString("Nothing picked — nothing starts.\n")
	case pick.Hold != "":
		fmt.Fprintf(&b, "`%s` %s.\n\nWhy it was picked: %s\n", pick.Key, pick.Hold, pick.Why)
	default:
		kind := "queued — floor re-checked at start"
		switch {
		case pick.Repair:
			kind = "queued — repair CI on the existing PR; claim re-checked at start"
		case pick.Idea:
			kind = "queued — promote the tagged idea, then ride it"
		case pick.OneStage:
			kind = "queued, one stage — then it parks"
		}
		fmt.Fprintf(&b, "`%s` %s.\n\nWhy: %s\n", pick.Key, kind, pick.Why)
	}
	return b.String()
}

// kickableRuns lists a project's parked runs a ride could start, sorted so
// the answer does not depend on scan order.
func kickableRuns(sc *pulseScan, projectID string) []string {
	var keys []string
	for _, md := range sc.mds {
		if md.Project == projectID && md.Status == run.StatusInProgress && operatorCascades(md.Workflow) {
			keys = append(keys, md.Project+"/"+md.ID)
		}
	}
	sort.Strings(keys)
	return keys
}

// noteWaitingOnParkedWork reports whether a run a pick could name carries an
// undelivered note, which lets the heartbeat look inside the quiet window.
func noteWaitingOnParkedWork(sc *pulseScan, projectID string) bool {
	for _, key := range kickableRuns(sc, projectID) {
		if pendingInputOnRun(sc.root, sc.byKey[key]) {
			return true
		}
	}
	for _, md := range sc.mds {
		if md.Project == projectID && md.Status == run.StatusInProgress &&
			md.Workflow == dash.IdeaWorkflow && md.PromoteTo != "" && pendingInputOnRun(sc.root, md) {
			return true
		}
	}
	return false
}

// pendingInputOnRun reports whether the run carries operator prose no turn
// has picked up yet. Delivery consumes it: one note buys one push.
func pendingInputOnRun(root string, md *run.Metadata) bool {
	f, err := input.Load(root, md.Project, md.ID)
	return err == nil && len(f.Pending()) > 0
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
//     the spawning run. Except a design-only spawn, whose seed is a
//     brief the spawner explicitly declined to bake: that run settles
//     through the past-first leg above, exactly the way an operator's
//     own `--from-idea` run does. Narrowing this leg rather than
//     deleting it keeps the decided behaviour for ordinary spawns —
//     the alternative, "spawned *and* no design turn yet", would
//     start holding every spawn whose ride broke after design.
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
	if md.SpawnedBy != "" && !md.DesignOnly {
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
	return "its " + md.Reaped.Doc + " turn was unfinished and was reaped " + when +
		" — an operator touch on the thread releases it"
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

// designOnlyHeldReason is designHeldReason's sibling for a run whose
// seed was a brief: the ride it was minted for has already happened, so
// "waiting at its first stage with only a seed" would read as a stall
// rather than as the designed resting place. Same one-vocabulary rule —
// the kick's stderr skip line and the `## Kick` section it renders both
// carry this string — and it names both of the operator's exits,
// because a parked design-only run is meant to end in one of them. The
// chain-state block phrases the same fact for its own reader (see
// heldNote): the survey's question there is where to put work, not
// whether to press a button.
const designOnlyHeldReason = "is a design-only spawn parked at design — advance it to ride, or close it"

// oneStageRide reports whether this root's next ride is the bounded
// one: an unsettled run admitted for a single stage — a design-only
// spawn's seed turn, or the turn an undelivered operator note buys. It
// routes through unsettledAdmission, so the bound and the floor's admit
// cannot disagree about which runs ride one stage and which ride on.
func oneStageRide(root, key string, sc *pulseScan) bool {
	md := sc.byKey[key]
	if md == nil {
		return false
	}
	settled, turnClosed := rootDesignSettled(root, md, sc.idx)
	if settled {
		return false
	}
	adm, _ := unsettledAdmission(sc, md, turnClosed)
	return adm == admitOneStage
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
	return openSessionStageExcept(root, md, "")
}

func openSessionStageExcept(root string, md *run.Metadata, ownBranch string) string {
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return ""
	}
	for _, stage := range w.Stages() {
		branch := session.BranchName(md.Project, md.ID, stage)
		if branch != ownBranch && git.HasRef(root, "refs/heads/"+branch) {
			return stage
		}
	}
	return ""
}
