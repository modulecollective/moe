package cli

import "github.com/modulecollective/moe/internal/project"

// Ride modes are consent vocabulary: what the invocation licensed the
// machine to do beyond the verb the operator typed.
//
// `moe pulse new --dynamic` is the **dynamic** rung, and means one thing:
// *the machine may start work you did not name.* A sweep under it rides the
// one run it picks. `moe serve --dynamic` is the standing spelling of that
// consent. A bang is a walk at rideNone: the operator named the run.
//
// The mode is a property of the *invocation*, not of any one call in the
// stack: one `moe` process is one operator verb carrying one consent level.
// So it is held as process state rather than threaded through push options
// and close commands.
type rideMode int

const (
	// rideNone: a bare push, `!`, `!<stage>`, `!!`, or a hand-typed
	// `moe pulse new`, which opens work and starts none of it.
	rideNone rideMode = iota
	// rideDynamic: `moe pulse new --dynamic` (the heartbeat's own child)
	// and the one ride a sweep starts.
	rideDynamic
)

func (m rideMode) String() string {
	switch m {
	case rideDynamic:
		return "dynamic"
	default:
		return "none"
	}
}

// currentRideMode is the consent level the invoking verb carried. It is
// process state deliberately (see the file comment); entry points set it
// through withRideMode, which hands back a restore so a prompt loop that
// dispatches several cascades in one session doesn't leak the first
// answer's mode into the second.
var currentRideMode = rideNone

// rideWalkActive distinguishes "a machine walk is in flight carrying
// consent rideNone" (a `!` step, a `!!` ship) from "nobody set a mode,
// so the zero value stands" (a bare operator `moe push`). currentRideMode
// alone can't: both read rideNone. Every withRideMode entry point is a
// machine-walk entry, so the flag is exactly "did a bang answer or a
// sweep hand this invocation to the machine".
var rideWalkActive = false

// withRideMode sets the process ride mode and returns the restore.
// Call as `defer withRideMode(m)()` at a cascade entry point.
func withRideMode(m rideMode) func() {
	prevMode, prevActive := currentRideMode, rideWalkActive
	currentRideMode, rideWalkActive = m, true
	return func() { currentRideMode, rideWalkActive = prevMode, prevActive }
}

// clockInvoked says the heartbeat spawned this process, rather than the
// operator typing its command. It is the one thing per-project modes
// bind on: the mode caps what the *clock* may start, and everything the
// operator types — bangs, stage verbs, and a hand-typed
// `moe pulse new --dynamic` — runs in every mode.
//
// It needs its own bit because the heartbeat's child is spelled exactly
// like the hand-typed command. `--dynamic` is consent, not authorship;
// `--emit-run` is incidental slug plumbing that a future caller could
// legitimately pass. So the child says so out loud, with `--heartbeat`.
//
// Process state for the same reason currentRideMode is: one `moe`
// process is one invocation, and who invoked it cannot differ between
// two calls in the same stack.
var clockInvoked = false

// withClockInvoked marks this process as the heartbeat's own child and
// returns the restore, matching withRideMode's shape.
func withClockInvoked() func() {
	prev := clockInvoked
	clockInvoked = true
	return func() { clockInvoked = prev }
}

// consentTrailerValue reports the MoE-Consent value for a commit written
// by this invocation, plus whether a machine walk is actually in flight.
//
// Emit sites that are machine by construction (a run mint that stamps
// MoE-Spawned-By, a pulse groom) ignore the second return and always
// stamp — the pulse acting is a machine turn whether or not the operator
// rode anything. Sites shared with operator verbs (the push record) stamp
// only when active is true, so `moe push` and an interactive `moe sdlc
// push` leave no trailer.
func consentTrailerValue() (value string, active bool) {
	return currentRideMode.String(), rideWalkActive
}

// walkConsent is the MoE-Consent value for a journal commit written by
// an emit site shared between operator verbs and machine walks: the ride
// level while a walk is in flight, and "" otherwise. It is the whole
// stamp rule in one call — every such site spells it the same way, so
// the set of machine-marked commits can't drift site by site.
//
// The rule exists because MoE-Consent is documented as *the* machine
// marker ("Presence is the machine marker", trailers.Block.Consent) and
// serve's heartbeat promises every act it takes is journal-marked —
// neither of which was true of the run-lifecycle commits a walk lands.
// A sweep's own `Open run` / `work: start session for pulse` / `work:
// update pulse` / `Close pulse run` carried no mark at all, so a reader
// asking "did the operator do this" read the machine's own exhaust as
// the operator's.
//
// Empty when no walk is active, so an operator's commit message stays
// byte-identical to what it was before this landed — the stamp records
// a fact about the process, and typing `moe close` yourself is not it.
func walkConsent() string {
	value, active := consentTrailerValue()
	if !active {
		return ""
	}
	return value
}

// spawnConsent is the MoE-Consent value for the open commit of a run
// being minted with spawnedBy. It pairs with MoE-Spawned-By rather than
// standing alone: `spawned_by` present already means machine-opened, so
// the consent trailer decorates that edge with the ride level instead of
// making a second, weaker claim. An operator-typed `moe pulse` (no
// spawner) mints with neither.
func spawnConsent(spawnedBy string) string {
	if spawnedBy == "" {
		return ""
	}
	value, _ := consentTrailerValue()
	return value
}

// rideModeContextLine tells a **dynamic** sweep that its ordering
// opinion is about to become motion, so its placement judgment can
// adapt. Empty otherwise — a hand-typed `moe pulse new` is pure
// curation, and a line saying "nothing will start" is context the agent
// can't act on.
func rideModeContextLine() string {
	if currentRideMode != rideDynamic {
		return ""
	}
	return "This is a **dynamic** sweep: the operator armed a clock that licensed the machine to start " +
		"work. The one run you name in `next` rides when this sweep finishes, with no human look in between, " +
		"and nothing else starts. Omit `next` if nothing should."
}

// projectModeContextLine tells a clock-invoked sweep that the project it
// is surveying is in **safe** mode, so its grooming knows which of the
// threads it places can actually start. Empty in every other case: auto
// is the ordinary state the ride line already describes, paused never
// reaches a sweep at all, and a hand-typed dynamic sweep ignores the
// mode outright.
func projectModeContextLine(root, projectID string) string {
	if !clockInvoked {
		return ""
	}
	if mode, err := project.ReadMode(root, projectID); err != nil || mode != project.ModeSafe {
		return ""
	}
	return "This project is in **safe** mode: a `next` here starts only if the run carries an explicit " +
		"operator mark — an advance marker, a chore's standing intent, a workflow tag on the idea it was " +
		"promoted from, or an undelivered note (a `moe input add` note or an answered question, consumed " +
		"once a turn delivers it). An unmarked pick is held with a reason and waits for the operator."
}
