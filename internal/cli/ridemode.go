package cli

// Ride modes are consent vocabulary, one notch past `!!!`.
//
// `!!!` (and plain `moe chain kick`) is the **static** ride: the
// machine cannot grow it. Tail pulses still survey, spawn and groom,
// but a placement that resolves into the unit being ridden is
// redirected to a self-rooted thread, so what the operator saw at kick
// time is what runs.
//
// `!!!!` (and `moe chain kick --dynamic`) is the **dynamic** ride, and
// means one thing everywhere: *the machine may run things beyond what
// you see right now.* A tail pulse may append onto the ridden unit's
// tail (the per-hop journal-index rebuild in maybeRideChain picks it
// up), and — on an unchained spawner — may kick a thread it groomed.
//
// The mode is a property of the *invocation*, not of any one call in
// the stack: one `moe` process is one operator verb carrying one
// consent level, and every hop of a ride inherits it. So it is held as
// process state rather than threaded through push options, close
// commands and the chain-kick body — five plumbing seams for a value
// that can never legitimately differ between them.
//
// Its only consumers are the pulse: the groom step's placement rules,
// the self-kick gate, and the survey's chain-state context line.
// maybeRideChain, `chain edit`, kick and every other chain mechanic
// never read it — the mode gates the pulse; the chain itself doesn't
// care. That is also why `rideChain bool` still threads the cascade
// unchanged: "does this ride at all" is a different question from
// "may the machine grow it".
type rideMode int

const (
	// rideNone: no ride in flight — a bare push, `!`, `!<stage>`, `!!`.
	// Grooming is pure curation: nothing this pulse places can move
	// until someone kicks the thread it landed on.
	rideNone rideMode = iota
	// rideStatic: `!!!` / `moe chain kick`. Grooming is redirected away
	// from the ridden unit; self-kick is refused.
	rideStatic
	// rideDynamic: `!!!!` / `moe chain kick --dynamic`, and every kick
	// the pulse roots itself. Mid-ride growth and self-kick are live.
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

// withRideMode sets the process ride mode and returns the restore.
// Call as `defer withRideMode(m)()` at a cascade entry point.
func withRideMode(m rideMode) func() {
	prev := currentRideMode
	currentRideMode = m
	return func() { currentRideMode = prev }
}

// rideModeContextLine tells a mid-ride survey which kind of ride it is
// inside, so its placement judgment can adapt. Empty when no ride is in
// flight — most pulses — because a line that says "nothing is riding"
// is context the agent can't act on.
func rideModeContextLine() string {
	switch currentRideMode {
	case rideStatic:
		return "This pulse is firing inside a **static** ride: the operator's kick is walking a chain " +
			"right now, and the machine cannot grow it. A placement aimed into that chain will be " +
			"redirected to its own thread. Shape new threads worth naming instead of trying to extend " +
			"the one that's running, and don't ask for a kick — it will be refused."
	case rideDynamic:
		return "This pulse is firing inside a **dynamic** ride: the operator licensed the machine to " +
			"extend it. Work groomed onto the ridden chain's tail will run in this same ride, and a " +
			"`\"kick\": true` group may start a thread of its own. Both are real motion with no human " +
			"look in between — hold the ordering and kick bars accordingly."
	}
	return ""
}

// rideModeForAnswer maps a chain-prompt bang answer to its mode. Only
// the two ride forms carry one; `!`, `!<stage>` and `!!` do not ride, so
// there is no unit for the machine to grow or refrain from growing.
func rideModeForAnswer(answer string) rideMode {
	switch answer {
	case "!!!":
		return rideStatic
	case "!!!!":
		return rideDynamic
	default:
		return rideNone
	}
}
