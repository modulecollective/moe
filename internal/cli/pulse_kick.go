package cli

import (
	"io"

	"github.com/modulecollective/moe/internal/run"
)

// The harness-owned step that follows grooming: whether the pulse kicks
// anything itself. Where a spawned run *lands* is not here — that is a
// `chain` claim the survey makes, twin reflects included.

// pulseSelfKick is the last step of a pulse: kick the threads whose
// groom group asked for it. This is the only door to machine-rooted
// motion, and two structural guards hold it shut everywhere else.
//
// First, **dynamic consent upstream**. A `!!!` tail pulse — or a manual
// `moe pulse new` — grooms and parks; only a fourth bang the operator
// actually typed licenses the machine to start something. That is what
// makes the surprise ride impossible by construction rather than by
// restraint — "I ran a plain push and my terminal is riding a thread I
// never saw" cannot happen, and a plain push no longer sweeps at all.
//
// Second, **re-entrancy**: a pulse-kick only roots at an unchained
// spawner. If the run whose tail fired this pulse is itself a chain
// member, the ride that is (probably) carrying it already picks up
// growth on its own tail, so nested rides are impossible — again by
// construction, not by flag-threading.
//
// There is deliberately no third bound on *how many* generations this
// can run for. A kicked ride's own tail does fire a pulse, so a survey
// can groom and kick work whose tail grooms and kicks again — the
// machine walks until a survey finds nothing worth chaining. What holds
// that open-ended is the two guards above plus the ladder itself: each
// generation is real shipped work behind review and test, it shows up on
// the dash as it lands, and a Ctrl-C halts the ride. Escalation by
// visibility, not by counting.
//
// Kicks that do fire are themselves dynamic rides: a confident pulse
// rooting bounded-only motion would defeat the point, and an operator
// who wants bounded keeps `!!!`.
//
// And the thread must be **machine-rooted**. The pulse curates operator
// chains but never starts them; that trigger stays with the operator.
//
// Every skip is one stderr line, warn-only ethos.
//
// Every fact this step keys on comes out of the groom's final in-memory
// graph (see groomResult) — thread roots, the spawner's chain
// membership, and whether a root is still kickable. Re-reading the
// journal here would answer the same questions a second time against a
// state the sweep had already moved.
func pulseSelfKick(root string, groomed groomResult, spawnerKey string, stdout, stderr io.Writer) {
	var wanted []groomedThread
	for _, th := range groomed.threads {
		if th.Kick && th.Root != "" {
			wanted = append(wanted, th)
		}
	}
	if len(wanted) == 0 {
		return
	}
	if currentRideMode != rideDynamic {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, this verb carried no dynamic consent (`!!!!` or --dynamic)\n", len(wanted))
		return
	}
	if spawnerKey != "" && groomed.spawnerChained {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, %s is itself chained and its ride picks up growth on its own tail\n",
			len(wanted), spawnerKey)
		return
	}
	for _, th := range wanted {
		proj, runID, err := splitProjectRun(th.Root)
		if err != nil {
			moePrintf(stderr, "pulse: kick: malformed thread root %q: %v\n", th.Root, err)
			continue
		}
		// A group can be groomed onto a thread whose head has already
		// shipped — `onto` admits a settled anchor on purpose, that being
		// the queue-jump case — and the root then walks back to a merged
		// run. Kicking one would ride a finished thread from its finished
		// end. ChainChildLive is the same terminal-or-missing test every
		// other edge reader applies.
		md := groomed.byKey[th.Root]
		if md == nil || !run.ChainChildLive(th.Root, groomed.byKey) {
			moePrintf(stderr, "pulse: kick: %s heads a thread that has already settled — skipping\n", th.Root)
			continue
		}
		if md.SpawnedBy == "" {
			moePrintf(stderr, "pulse: kick: %s is operator-rooted — the pulse curates those, it doesn't start them\n", th.Root)
			continue
		}
		moePrintf(stderr, "pulse: kicking %s (dynamic)\n", th.Root)
		if code := chainKickRun(root, proj, runID, rideDynamic, stdout, stderr); code != 0 {
			moePrintf(stderr, "pulse: kick %s exited %d\n", th.Root, code)
		}
	}
}
