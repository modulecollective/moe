package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// selfKickFixture stands up a project with a machine-rooted, self-rooted
// thread of two parked fixes and returns its root key. The cascade's
// agent seams are stubbed, so a kick that fires is observed rather than
// executed.
func selfKickFixture(t *testing.T) (root, threadRoot string, groomed groomResult, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	groomed = groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)
	return root, "moe/" + minted["fix-a"], groomed, stages
}

// wantKick reshapes a real groom result into "these threads asked for a
// kick", keeping the sweep's own graph-derived byKey so the kick step
// reads exactly what the groom stamped.
func wantKick(groomed groomResult, threads ...groomedThread) groomResult {
	groomed.threads = threads
	return groomed
}

// TestSelfKickRidesUnderTheFourthBang: an unchained spawner, a dynamic
// verb upstream, a machine-rooted thread — the pulse kicks it, and the
// ride is itself dynamic. This is the level-4 loop's one door.
func TestSelfKickRidesUnderTheFourthBang(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "" /*unchained spawner*/, io.Discard, &errb)

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
			root, threadRoot, groomed, stages := selfKickFixture(t)

			defer withRideMode(mode)()
			var errb bytes.Buffer
			pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

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
	root, threadRoot, groomed, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	// The spawner is the thread's own root — a chain member, which the
	// groom sweep reports off its own final graph.
	groomed.spawnerChained = true
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), threadRoot, io.Discard, &errb)

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
	groomFixture(t, root, "fix-a")

	// An operator-minted head (no SpawnedBy) with the fix behind it.
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	headKey := "moe/" + head.ID
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: head.ID, Runs: runsFrom("fix-a")}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: headKey, Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on an operator-rooted thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "operator-rooted") {
		t.Errorf("stderr = %q, want the machine-rooted guard named", errb.String())
	}
}

// TestSelfKickRidesAtAnOperatorOpenedSpawner is the happy path with a
// spawner actually named: a pulse fired by an unchained operator-opened
// run kicks, and that kick is the one generation the `!!!!` licensed.
// The generation bound itself now sits at fire time (pulseFiresForRun),
// so by the time a pulse exists its spawner is already operator-rooted.
func TestSelfKickRidesAtAnOperatorOpenedSpawner(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	spawner, err := mintChainRun(root, "moe", "operator-push", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "moe/"+spawner.ID, io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("nothing was driven; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickIgnoredWhenNoGroupAsked: the common case. A groom sweep
// that placed work but asked for no kick starts nothing, whatever the
// ride mode.
func TestSelfKickIgnoredWhenNoGroupAsked(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: false}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing", kickStages(*stages))
	}
	if errb.Len() != 0 {
		t.Errorf("stderr = %q, want silence", errb.String())
	}
}

// TestSelfKickSkipsASettledThreadRoot: `onto` deliberately admits a
// settled anchor — that is the queue-jump case — so a group can land
// behind a run that already merged, and the thread it joins is then
// rooted at that merged run. Kicking one would ride a finished thread
// from its finished end.
func TestSelfKickSkipsASettledThreadRoot(t *testing.T) {
	root, stages, _ := kickFixture(t)
	minted := groomFixture(t, root, "shipped", "fix-a")
	shippedKey := "moe/" + minted["shipped"]
	setRunStatus(t, root, "moe", minted["shipped"], run.StatusMerged)

	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Onto: "shipped", Runs: runsFrom("fix-a")}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)
	if len(groomed.threads) != 1 || groomed.threads[0].Root != shippedKey {
		t.Fatalf("threads = %+v, want one rooted at the merged anchor %s", groomed.threads, shippedKey)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: shippedKey, Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a settled thread root", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "already settled") {
		t.Errorf("stderr = %q, want the settled-root skip named", errb.String())
	}
}
