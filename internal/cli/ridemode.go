package cli

import "github.com/modulecollective/moe/internal/project"

// Ride modes are consent vocabulary: what the invocation licensed the
// machine to do beyond the verb the operator typed.
//
// `!!!` (and `moe chain kick`) is the **static** ride: ship this run and
// walk the chain as it stands. It sweeps nothing — no verb fires a pulse
// in-process any more — so what the operator saw at kick time is what
// runs, by construction.
//
// `moe pulse new --dynamic` is the **dynamic** rung, and means one thing:
// *the machine may run things beyond what you see right now.* A sweep
// under it kicks the parked threads on the board. `moe serve --dynamic`
// is the standing spelling of that consent, and every kick a sweep roots
// inherits it.
//
// The mode is a property of the *invocation*, not of any one call in
// the stack: one `moe` process is one operator verb carrying one
// consent level, and every hop of a ride inherits it. So it is held as
// process state rather than threaded through push options, close
// commands and the chain-kick body — four plumbing seams for a value
// that can never legitimately differ between them.
//
// Its consumers are the pulse's self-kick gate and the survey's context
// line, plus the consent trailers every machine-written commit carries.
// maybeRideChain, `chain edit`, kick and every other chain mechanic
// never read it — the mode gates the sweep; the chain itself doesn't
// care. That is also why `rideChain bool` still threads the cascade
// unchanged: "does this ride at all" is a different question from
// "may the machine start more".
type rideMode int

const (
	// rideNone: no ride in flight — a bare push, `!`, `!<stage>`, `!!`,
	// or a hand-typed `moe pulse new`. A sweep here is pure curation:
	// nothing it places can move until someone kicks the thread it
	// landed on.
	rideNone rideMode = iota
	// rideStatic: `!!!` / `moe chain kick`. Ship and ride the chain as
	// it stands; a sweep under it starts nothing.
	rideStatic
	// rideDynamic: `moe pulse new --dynamic` (the heartbeat's own child),
	// and every kick a sweep roots itself. Self-kick is live.
	rideDynamic
)

func (m rideMode) String() string {
	switch m {
	case rideStatic:
		return "static"
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
// chain kick hand this invocation to the machine".
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
// operator types — bangs, stage verbs, chain kicks, and a hand-typed
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
		"work. Every kickable parked thread on the board gets its own kick when this sweep finishes — " +
		"the ones you groom and the ones already sitting in order — unless you park it. That is real " +
		"motion with no human look in between, so hold the ordering bar accordingly and write a " +
		"`\"park\"` line for any thread the operator should see first."
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
	return "This project is in **safe** mode: the clock may sweep it, but the kick will start only threads " +
		"carrying an explicit operator mark — an advance marker, a chore's standing intent, or a workflow " +
		"tag on the idea a run was promoted from. Everything else you groom is held with a reason and waits " +
		"for the operator. Groom the board as you normally would; just don't count on an unmarked thread " +
		"moving before someone looks at it."
}

// rideModeForAnswer maps a chain-prompt bang answer to its mode. Only
// `!!!` rides; `!`, `!<stage>` and `!!` do not, so there is no chain in
// flight to describe. No typed answer reaches rideDynamic — that rung is
// the pulse verb's `--dynamic`, said at the door a clock knocks on.
func rideModeForAnswer(answer string) rideMode {
	if answer == "!!!" {
		return rideStatic
	}
	return rideNone
}
