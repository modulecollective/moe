package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
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

// TestSelfKickSkipsAHandMintedChainHead: the operator composes a chain
// head over an afternoon and hangs work off it. The `chain` workflow's
// ladder is empty by design, so a hand-minted head is never past its
// first stage and carries no machine or chore seed — it stays with the
// operator.
func TestSelfKickSkipsAHandMintedChainHead(t *testing.T) {
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
	if !strings.Contains(errb.String(), "waiting at its first stage with only a seed") {
		t.Errorf("stderr = %q, want the settled-design guard named", errb.String())
	}
}

// TestSelfKickSkipsASeedOnlyOperatorRoot is the boundary the operator
// drew: a promoted sketch sitting at design with nothing but its seed
// is not a settled design, so the machine does not start it. This is
// the class the readiness admit deliberately keeps holding.
func TestSelfKickSkipsASeedOnlyOperatorRoot(t *testing.T) {
	root, stages, _ := kickFixture(t)
	seedRun(t, root, "moe", "promoted-sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# A thought I had\n\nseed\n"})
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("promoted-sketch"), Kick: true}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/promoted-sketch", Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a seed-only root", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "waiting at its first stage with only a seed") {
		t.Errorf("stderr = %q, want the settled-design guard named", errb.String())
	}
}

// TestSelfKickSkipsADesignClosedButNotAdvancedRoot pins the boundary
// AdvancedTo's comment records — "a canvas merely complete is not
// consent to proceed" — now that the admit no longer names the advance
// marker. The design turn landed and the operator declined the chain
// prompt without hitting `a`, so the run still reads as waiting at
// design, and past-first holds it for free. The skip line says so: this
// run's design *did* run, which is the half "only a seed" got wrong.
func TestSelfKickSkipsADesignClosedButNotAdvancedRoot(t *testing.T) {
	root, stages, _ := kickFixture(t)
	seedRun(t, root, "moe", "design-done", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# Worked, then parked\n\nbody\n"})
	trailerstest.CommitWorkTurnAt(t, root, "moe", "design-done", "sdlc", "design", time.Now().Local())
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("design-done"), Kick: true}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/design-done", Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a design that merely closed", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "waiting at its first stage with its turn closed but not advanced") {
		t.Errorf("stderr = %q, want the hold to name the closed turn, not a seed", errb.String())
	}
}

// TestSelfKickRidesARootParkedByDownstreamWork is the one genuinely new
// admitted class: nobody clicked `a` and no machine minted it, but
// design and code turns have both landed, so the run is unambiguously
// past its first stage. Real downstream work is a settled design.
func TestSelfKickRidesARootParkedByDownstreamWork(t *testing.T) {
	root, stages, _ := kickFixture(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "mid-ladder", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Half built\n\nbody\n", "code": "# The diff\n\nbody\n"})
	trailerstest.CommitWorkTurnAt(t, root, "moe", "mid-ladder", "sdlc", "design", now.Add(-2*time.Hour))
	trailerstest.CommitWorkTurnAt(t, root, "moe", "mid-ladder", "sdlc", "code", now.Add(-time.Hour))
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("mid-ladder"), Kick: true}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/mid-ladder", Kick: true}), "", io.Discard, &errb)

	// Next parks at the last stage worked (code has no successor turn and
	// no marker), so the ride resumes there rather than at design.
	if got := kickStages(*stages); len(got) == 0 || got[0] != "mid-ladder:code" {
		t.Fatalf("drove %v, want the ride to resume at code; stderr=%q", got, errb.String())
	}
}

// choreKickFixture reproduces the 2026-07-22 *evening* incident: a
// dynamic tail pulse nominated a judged chore, openChoreInProcess
// opened its run with the MoE-Chore trailer and — alone among
// machine-open paths — no SpawnedBy, and the groom rooted a thread at
// that fresh run. Seeded rather than driven through the chore pipeline
// so the fixture is the *shape* the kick reads: run.json with the
// chore's seed canvas, plus the open commit's chore trailer.
func choreKickFixture(t *testing.T) (root, threadRoot string, groomed groomResult, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	seedRun(t, root, "moe", "readme-update-2026-07-22", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# Update the README\n\nthe chore's own prompt\n"})
	trailerstest.CommitTrailer(t, root, "Open run moe/readme-update-2026-07-22",
		"MoE-Run: readme-update-2026-07-22\nMoE-Project: moe\nMoE-Workflow: sdlc\nMoE-Chore: moe/readme-update",
		time.Time{})

	groomed = groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("readme-update-2026-07-22"), Kick: true}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)
	if groomed.idx.ChoreByRun["moe/readme-update-2026-07-22"] == "" {
		t.Fatal("precondition: the groom's index should carry the chore edge the open commit recorded")
	}
	return root, "moe/readme-update-2026-07-22", groomed, stages
}

// TestSelfKickRidesAChoreRootedThread is the incident this run opened
// on. The chore's prompt.md is operator-authored standing intent — a
// settled design by construction — so the fresh run it seeds is
// kickable even though nothing about its lineage says so. Under the
// old lineage admit this generation stranded, taking a reflect
// carrying four merged runs' twin observations with it.
func TestSelfKickRidesAChoreRootedThread(t *testing.T) {
	root, threadRoot, groomed, stages := choreKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

	// A chore run is fresh, so the ride starts at its first stage.
	if got := kickStages(*stages); len(got) == 0 || got[0] != "readme-update-2026-07-22:design" {
		t.Fatalf("drove %v, want the ride to start at design; stderr=%q", got, errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickSkipsAChoreRootWithALiveSession: the occupancy check is
// not welded to the advanced leg any more — a settled design says the
// run is ready, and this says nobody is already inside it. A chore run
// the operator picked up by hand is held, and the skip line names the
// stage so the operator can see which session to finish or abandon.
func TestSelfKickSkipsAChoreRootWithALiveSession(t *testing.T) {
	root, threadRoot, groomed, stages := choreKickFixture(t)
	if _, err := session.Open(root, "moe", "readme-update-2026-07-22", "design"); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing while a session is open", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "live session at design") {
		t.Errorf("stderr = %q, want the occupancy skip to name the stage", errb.String())
	}
}

// advancedKickFixture reproduces the 2026-07-22 incident's shape: an
// operator-opened sdlc run (no SpawnedBy) that the operator advanced
// past design and left, groomed as a self-rooted one-run thread asking
// for a kick. Returns its thread root.
//
// Seeded rather than driven through `moe sdlc`: the block's own tests
// establish that this journal shape is what `a` at a chain prompt
// leaves, and a real stage run needs an agent session.
func advancedKickFixture(t *testing.T) (root, threadRoot string, groomed groomResult, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "advanced-run", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Widen the kick admit\n\nbody\n"})
	advanceAt(t, root, "moe", "advanced-run", "design", now.Add(-2*time.Hour))

	groomed = groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("advanced-run"), Kick: true}}, "", nil /*kickoff edges*/, io.Discard, os.Stderr)
	if len(groomed.threads) != 1 || groomed.threads[0].Root != "moe/advanced-run" {
		t.Fatalf("threads = %+v, want one self-rooted at moe/advanced-run", groomed.threads)
	}
	return root, "moe/advanced-run", groomed, stages
}

// TestSelfKickRidesAnOperatorAdvancedRoot is the morning incident. A
// run the operator personally clicked forward at the design chain
// prompt was the only work a dynamic generation found ready, and the
// machine-rooted admit refused it — the most-consented parked work was
// the one class the pulse could never start. The marker satisfies the
// design stage, so the run reads as past its first stage, and the ride
// resumes at the stage it is waiting at. No advance-marker special case
// on the kick side: this arrives through stageSatisfied.
func TestSelfKickRidesAnOperatorAdvancedRoot(t *testing.T) {
	root, threadRoot, groomed, stages := advancedKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

	// Mid-ladder pickup: design is already satisfied by the marker, so
	// the ride starts at code rather than re-opening the stage the
	// operator finished.
	if got := kickStages(*stages); len(got) == 0 || got[0] != "advanced-run:code" {
		t.Fatalf("drove %v, want the ride to start at code; stderr=%q", got, errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickSkipsAnAdvancedRootWithALiveSession: the double-run
// guard. The operator is working the very stage the kick would open,
// and a session branch is the only signal that says so while that stage
// is still running.
func TestSelfKickSkipsAnAdvancedRootWithALiveSession(t *testing.T) {
	root, threadRoot, groomed, stages := advancedKickFixture(t)
	if _, err := session.Open(root, "moe", "advanced-run", "code"); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing while the operator has the stage open", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "live session at code") {
		t.Errorf("stderr = %q, want the occupancy skip to name the stage", errb.String())
	}
}

// TestSelfKickSkipsAnAdvancedRootOutdatedByAReEdit: the staleness rule
// the admit inherits from stageSatisfied for free. A re-edit of the
// stage the operator advanced past lands a newer work-turn that
// out-dates the marker, so the run reads as waiting at design again —
// which is right, because the consent was for a canvas that has since
// moved. Two turns have landed at design here, so the skip line reports
// the turn, not a seed.
func TestSelfKickSkipsAnAdvancedRootOutdatedByAReEdit(t *testing.T) {
	root, threadRoot, groomed, stages := advancedKickFixture(t)
	trailerstest.CommitWorkTurnAt(t, root, "moe", "advanced-run", "sdlc", "design", time.Now().Local())
	// The groom's snapshot predates the re-edit, so re-read the journal
	// the way the next sweep would.
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	groomed.idx = idx

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Kick: true}), "", io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on an out-dated marker", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "waiting at its first stage with its turn closed but not advanced") {
		t.Errorf("stderr = %q, want the hold to name the closed turn, not a seed", errb.String())
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
