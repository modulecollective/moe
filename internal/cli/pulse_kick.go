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
// motion, and three structural guards hold it shut everywhere else.
//
// First, **dynamic consent upstream**. A plain push, `!!` or `!!!` tail
// pulse grooms and parks; only a fourth bang the operator actually
// typed licenses the machine to start something. That is what makes the
// surprise ride impossible by construction rather than by restraint —
// "I ran a plain push and my terminal is riding a thread I never saw"
// cannot happen.
//
// Second, **re-entrancy**: a pulse-kick only roots at an unchained
// spawner. If the run whose tail fired this pulse is itself a chain
// member, the ride that is (probably) carrying it already picks up
// growth on its own tail, so nested rides are impossible — again by
// construction, not by flag-threading.
//
// Third, **one machine generation per operator push**. The guards above
// are per-hop, and each hop satisfies them afresh: a kicked ride's own
// tail push fires a pulse that grooms, spawns and kicks again, without
// bound. So a pulse whose firing push came from a machine-opened run
// declines all kicks. Each `!!!!` licenses the machine to start
// something once; letting generation N license generation N+1 is the
// runaway. The deferred work isn't lost — it parks, and a parked thread
// is exactly what the next operator-consented pulse consolidates.
//
// Kicks that do fire are themselves dynamic rides: a confident pulse
// rooting bounded-only motion would defeat the point, and an operator
// who wants bounded keeps `!!!`.
//
// And the thread must be **machine-rooted**. The pulse curates operator
// chains but never starts them; that trigger stays with the operator.
//
// Every skip is one stderr line, warn-only ethos.
func pulseSelfKick(root string, threads []groomedThread, spawnerKey string, stdout, stderr io.Writer) {
	var wanted []groomedThread
	for _, th := range threads {
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
	if spawnerKey != "" && chainMember(root, spawnerKey) {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, %s is itself chained and its ride picks up growth on its own tail\n",
			len(wanted), spawnerKey)
		return
	}
	if spawnerKey != "" && machineOpened(root, spawnerKey) {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, %s is itself machine-opened; second-generation work parks for the next operator pulse\n",
			len(wanted), spawnerKey)
		return
	}
	for _, th := range wanted {
		proj, runID, err := splitProjectRun(th.Root)
		if err != nil {
			moePrintf(stderr, "pulse: kick: malformed thread root %q: %v\n", th.Root, err)
			continue
		}
		md, err := run.Load(root, proj, runID)
		if err != nil {
			moePrintf(stderr, "pulse: kick: load %s: %v\n", th.Root, err)
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

// chainMember reports whether key sits in a chain of two or more live
// runs — the re-entrancy question pulseSelfKick asks about its spawner.
// A read failure reads as "member", which is the conservative answer:
// it suppresses a kick rather than risking a nested ride.
func chainMember(root, key string) bool {
	g, ok := loadChainGraph(root)
	if !ok {
		return true
	}
	return len(g.unit(key)) >= 2
}

// machineOpened reports whether key names a run the machine opened —
// the depth question pulseSelfKick asks about its spawner. Lineage is
// already durable: every machine-opened run records the run that opened
// it, so the answer is one metadata read and no carried state.
//
// A read failure reads as "machine-opened", the same conservative
// direction chainMember takes: suppress a kick rather than risk an
// unbounded one.
func machineOpened(root, key string) bool {
	proj, runID, err := splitProjectRun(key)
	if err != nil {
		return true
	}
	md, err := run.Load(root, proj, runID)
	if err != nil {
		return true
	}
	return md.SpawnedBy != ""
}
