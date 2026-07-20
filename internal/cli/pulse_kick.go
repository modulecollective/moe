package cli

import (
	"fmt"
	"io"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// The two harness-owned steps that follow grooming: where the
// auto-spawned twin reflect lands, and whether the pulse kicks anything
// itself.

// placeReflect decides where the sweep's auto-spawned twin reflect
// goes, so that under live consent one ride drains the fixes *and*
// brings the twin current — the reflect reading the post-fix settled
// record rather than the record as of mid-batch.
//
// Two conditions gate the whole rule, both structural. The reflect has
// to exist, and the ride has to be dynamic (a static ride's unit is
// closed to machine growth, so the reflect parks standalone as it does
// today). No agent-side control: the reflect's slug is harness-minted,
// so this is a harness rule, not a `chain` entry.
//
// A third condition picks *which* of two placements applies. With a
// chained spawner there is a tail to stamp onto and the reflect joins
// that unit. With an unchained spawner — the common case, since most
// pulses fire off a standalone run's push — there is no tail to guess
// at, so the reflect stays self-rooted and comes back as a kick
// candidate instead: the caller appends it to pulseSelfKick's thread
// list, last, so gate-named fix threads ride first and the reflect
// still reads the settled post-fix record. That is the same
// read-after-fixes ordering the tail stamp gives, and the same license
// — a reflect stamped onto a ridden dynamic unit already rides today.
//
// Returns the self-rooted reflect thread in that second case, and nil
// otherwise (stamped a tail, no reflect, or no dynamic consent).
//
// One accepted wrinkle on the stamped path: a later mid-ride append
// lands *behind* the reflect, so it reflects the chain as of its own
// hop. Whatever merges after it waits for the next due reflect — which
// is exactly the cadence reflects have today.
//
// Warn-only: an unstamped reflect is still a parked reflect run, which
// is where it would have landed before this rule existed.
func placeReflect(root, projectID, pulseSlug, reflectID, spawnerKey string, stdout, stderr io.Writer) *groomedThread {
	if reflectID == "" || currentRideMode != rideDynamic {
		return nil
	}
	reflectKey := projectID + "/" + reflectID
	selfRooted := &groomedThread{Root: reflectKey, Kick: true}
	if spawnerKey == "" {
		return selfRooted
	}
	tail, ok := liveChainTail(root, spawnerKey)
	if !ok {
		return selfRooted
	}
	if tail == reflectKey {
		return nil
	}
	msg := fmt.Sprintf("chain: chain %s after %s\n\n", reflectKey, tail) +
		trailers.Block{
			Run:       reflectID,
			Project:   projectID,
			Workflow:  "twin",
			ChainedTo: []string{tail + " " + reflectKey},
		}.String()
	err := sync.WithJournalPush(root, repolock.Options{
		Purpose: "pulse-reflect-chain",
		Run:     projectID + "/" + pulseSlug,
	}, stdout, stderr, func() error {
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		// Not rerouted to a self-rooted kick: this reflect belongs on the
		// spawner's unit, and the failure is in the journal write, not in
		// the placement. Parking it is the pre-existing fallback and the
		// one that doesn't start a ride the failed commit was meant to
		// order.
		moePrintf(stderr, "pulse: chain reflect %s after %s: %v — the reflect is open but unchained\n", reflectKey, tail, err)
		return nil
	}
	moePrintf(stderr, "pulse: chained twin reflect %s after %s\n", reflectKey, tail)
	return nil
}

// liveChainTail walks forward from key to the end of its thread, over
// live edges only. ok is false when key heads nothing — a run with no
// outgoing live edge is not a chain member, and there is no unit tail
// to speak of.
//
// Built fresh off the journal rather than handed down from the groom
// sweep: grooming committed since, and the whole point of stamping here
// is to land *after* whatever it appended.
func liveChainTail(root, key string) (string, bool) {
	g, ok := loadChainGraph(root)
	if !ok || len(g.unit(key)) < 2 {
		return "", false
	}
	return g.tailFrom(key), true
}

// pulseSelfKick is the last step of a pulse: kick the threads whose
// groom group asked for it. This is the only door to machine-rooted
// motion, and two structural guards hold it shut everywhere else.
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
