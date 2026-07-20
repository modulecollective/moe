package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// selfKickFixture stands up a project with a machine-rooted, self-rooted
// thread of two parked fixes and returns its root key. The cascade's
// agent seams are stubbed, so a kick that fires is observed rather than
// executed.
func selfKickFixture(t *testing.T) (root, threadRoot string, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Runs: []string{"fix-a", "fix-b"}}}, minted, "", io.Discard, os.Stderr)
	return root, "moe/" + minted["fix-a"], stages
}

// TestSelfKickRidesUnderTheFourthBang: an unchained spawner, a dynamic
// verb upstream, a machine-rooted thread — the pulse kicks it, and the
// ride is itself dynamic. This is the level-4 loop's one door.
func TestSelfKickRidesUnderTheFourthBang(t *testing.T) {
	root, threadRoot, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, []groomedThread{{Root: threadRoot, Kick: true}}, "" /*unchained spawner*/, io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("nothing was driven; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickSkipsWithoutDynamicConsent: a plain push, `!!` or `!!!`
// tail pulse grooms and parks. This is what makes the surprise ride
// impossible by construction — "I ran a plain push and my terminal is
// riding a thread I never saw" cannot happen.
func TestSelfKickSkipsWithoutDynamicConsent(t *testing.T) {
	for _, mode := range []rideMode{rideNone, rideStatic} {
		t.Run(mode.String(), func(t *testing.T) {
			root, threadRoot, stages := selfKickFixture(t)

			defer withRideMode(mode)()
			var errb bytes.Buffer
			pulseSelfKick(root, []groomedThread{{Root: threadRoot, Kick: true}}, "", io.Discard, &errb)

			if len(*stages) != 0 {
				t.Fatalf("drove %v, want nothing under %s", kickStages(*stages), mode)
			}
			if !strings.Contains(errb.String(), "no dynamic consent") {
				t.Errorf("stderr = %q, want the consent skip named", errb.String())
			}
		})
	}
}

// TestSelfKickSkipsAtAChainedSpawner is the re-entrancy guard: if the
// run whose tail fired this pulse is itself a chain member, the ride
// carrying it already picks up growth on its own tail. Nested rides are
// impossible by construction, not by flag-threading.
func TestSelfKickSkipsAtAChainedSpawner(t *testing.T) {
	root, threadRoot, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	// The spawner is the thread's own root — a chain member.
	pulseSelfKick(root, []groomedThread{{Root: threadRoot, Kick: true}}, threadRoot, io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing from a chained spawner", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "itself chained") {
		t.Errorf("stderr = %q, want the re-entrancy skip named", errb.String())
	}
}

// TestSelfKickSkipsAnOperatorRootedThread: the pulse curates operator
// chains but never starts them. That trigger stays with the operator.
func TestSelfKickSkipsAnOperatorRootedThread(t *testing.T) {
	root, stages, _ := kickFixture(t)
	minted := groomFixture(t, root, "fix-a")

	// An operator-minted head (no SpawnedBy) with the fix behind it.
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	headKey := "moe/" + head.ID
	groomChains(root, "moe", "pulse-groom",
		[]pulseChainGroup{{Onto: head.ID, Runs: []string{"fix-a"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, []groomedThread{{Root: headKey, Kick: true}}, "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on an operator-rooted thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "operator-rooted") {
		t.Errorf("stderr = %q, want the machine-rooted guard named", errb.String())
	}
}

// TestReflectStampsAtTheUnitTailWhenDynamic: the auto-spawned twin
// reflect joins the chain the pulse fired on, after the fixes, so it
// reads the post-fix settled record. One ride drains the fixes and
// brings the twin current.
func TestReflectStampsAtTheUnitTailWhenDynamic(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "member", "reflect-stand-in")
	spawnerKey := "moe/" + minted["spawner"]
	memberKey := "moe/" + minted["member"]
	reflectID := minted["reflect-stand-in"]

	groomChains(root, "moe", "pulse-setup",
		[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	got := placeReflect(root, "moe", "pulse-groom", reflectID, spawnerKey, io.Discard, os.Stderr)

	if tail := liveEdges(t, root)[memberKey]; tail != "moe/"+reflectID {
		t.Fatalf("unit tail %s chains to %q, want the reflect", memberKey, tail)
	}
	if got != nil {
		t.Errorf("returned kick candidate %+v, want none — a stamped reflect rides the unit it joined", *got)
	}
}

// TestReflectSelfRootsForAKickWhenSpawnerUnchained: the gap this run
// exists to close. Under dynamic consent with an unchained spawner
// there is no tail to stamp onto, so the reflect comes back as its own
// kick candidate instead of parking unridden.
func TestReflectSelfRootsForAKickWhenSpawnerUnchained(t *testing.T) {
	root := spawnFixture(t)
	minted := groomFixture(t, root, "spawner", "reflect-stand-in")
	reflectKey := "moe/" + minted["reflect-stand-in"]

	defer withRideMode(rideDynamic)()
	got := placeReflect(root, "moe", "pulse-groom", minted["reflect-stand-in"],
		"moe/"+minted["spawner"], io.Discard, os.Stderr)

	if got == nil {
		t.Fatalf("returned no kick candidate, want the reflect self-rooted")
	}
	if got.Root != reflectKey || !got.Kick {
		t.Errorf("candidate = %+v, want {Root: %q, Kick: true}", *got, reflectKey)
	}
	for parent, child := range liveEdges(t, root) {
		if child == reflectKey {
			t.Fatalf("reflect chained under %s, want it self-rooted", parent)
		}
	}
}

// TestReflectParksStandaloneOutsideADynamicRide: a static ride's unit
// is closed to machine growth, so the reflect neither joins it nor
// rides — it parks, and what the operator saw at kick time is what
// runs. Same for a static ride with nothing chained at all.
func TestReflectParksStandaloneOutsideADynamicRide(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chained bool
	}{
		{name: "chained-spawner", chained: true},
		{name: "unchained-spawner", chained: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := spawnFixture(t)
			minted := groomFixture(t, root, "spawner", "member", "reflect-stand-in")
			spawnerKey := "moe/" + minted["spawner"]
			reflectKey := "moe/" + minted["reflect-stand-in"]

			if tc.chained {
				groomChains(root, "moe", "pulse-setup",
					[]pulseChainGroup{{Runs: []string{"spawner", "member"}}}, minted, "", io.Discard, os.Stderr)
			}

			defer withRideMode(rideStatic)()
			got := placeReflect(root, "moe", "pulse-groom", minted["reflect-stand-in"],
				spawnerKey, io.Discard, os.Stderr)

			if got != nil {
				t.Errorf("returned kick candidate %+v, want none under a static ride", *got)
			}
			for parent, child := range liveEdges(t, root) {
				if child == reflectKey {
					t.Fatalf("reflect chained under %s, want it parked standalone", parent)
				}
			}
		})
	}
}

// TestSelfKickIgnoredWhenNoGroupAsked: the common case. A groom sweep
// that placed work but asked for no kick starts nothing, whatever the
// ride mode.
func TestSelfKickIgnoredWhenNoGroupAsked(t *testing.T) {
	root, threadRoot, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, []groomedThread{{Root: threadRoot, Kick: false}}, "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing", kickStages(*stages))
	}
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want silence", errb.String())
	}
}
