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
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("nothing was driven; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickReturnsTheFirstFailureAndKeepsKicking pins the pulse's
// invocation contract: an ordinary stalled ride re-arms the heartbeat,
// but does not withhold an independent thread the survey also groomed.
func TestSelfKickReturnsTheFirstFailureAndKeepsKicking(t *testing.T) {
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "fails-first", "fails-later")
	groomed := groomChains(root, "moe", "pulse-groom", []groomGroup{
		{Runs: runsFrom("fails-first")},
		{Runs: runsFrom("fails-later")},
	}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	var dispatched []string
	prev := openSdlcStage
	openSdlcStage = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
		dispatched = append(dispatched, runID+":"+stage)
		if runID == minted["fails-first"] {
			return 7
		}
		return 9
	}
	t.Cleanup(func() { openSdlcStage = prev })

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	if code := pulseSelfKick(root, groomed, io.Discard, &errb); code != 7 {
		t.Fatalf("self-kick exit=%d, want first failure 7; stderr=%q", code, errb.String())
	}
	want := []string{minted["fails-first"] + ":design", minted["fails-later"] + ":design"}
	if strings.Join(dispatched, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want both independent roots %v", dispatched, want)
	}
	for _, wantLine := range []string{
		"kick moe/" + minted["fails-first"] + " exited 7",
		"kick moe/" + minted["fails-later"] + " exited 9",
	} {
		if !strings.Contains(errb.String(), wantLine) {
			t.Errorf("stderr=%q, want %q", errb.String(), wantLine)
		}
	}
}

// TestSelfKickInterruptStopsLaterThreads preserves Ctrl-C as operator
// intent: unlike an ordinary failure, it ends the pulse-rooted ride
// before another groomed root starts.
func TestSelfKickInterruptStopsLaterThreads(t *testing.T) {
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "interrupted", "must-not-start")
	groomed := groomChains(root, "moe", "pulse-groom", []groomGroup{
		{Runs: runsFrom("interrupted")},
		{Runs: runsFrom("must-not-start")},
	}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	var dispatched []string
	prev := openSdlcStage
	openSdlcStage = func(stage, projectID, runID string, headless bool, _, _ io.Writer) int {
		dispatched = append(dispatched, runID+":"+stage)
		return exitInterrupted
	}
	t.Cleanup(func() { openSdlcStage = prev })

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	if code := pulseSelfKick(root, groomed, io.Discard, &errb); code != exitInterrupted {
		t.Fatalf("self-kick exit=%d, want exitInterrupted; stderr=%q", code, errb.String())
	}
	want := []string{minted["interrupted"] + ":design"}
	if strings.Join(dispatched, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatched %v, want only the interrupted root %v", dispatched, want)
	}
}

// TestSelfKickSkipsWithoutDynamicConsent: a plain push, `!!` or `!!!`
// tail pulse grooms and parks. This is what makes the surprise ride
// impossible by construction — "I ran a plain push and my terminal is
// riding a thread I never saw" cannot happen.
//
// Silently, and that is the point of the schema flip: with no ask-field
// on the gate, an unparked thread outside a dynamic ride is not a
// declined request, it is ordinary curation. The old line reported a
// decision that no longer exists.
func TestSelfKickSkipsWithoutDynamicConsent(t *testing.T) {
	for _, mode := range []rideMode{rideNone, rideStatic} {
		t.Run(mode.String(), func(t *testing.T) {
			root, threadRoot, groomed, stages := selfKickFixture(t)

			defer withRideMode(mode)()
			var errb bytes.Buffer
			pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

			if len(*stages) != 0 {
				t.Fatalf("drove %v, want nothing under %s", kickStages(*stages), mode)
			}
			if errb.Len() != 0 {
				t.Errorf("stderr = %q, want silence outside a dynamic ride", errb.String())
			}
		})
	}
}

// handMintedHeadFixture stands up the operator's staging fence: a chain
// head minted by hand (no SpawnedBy) with a settled fix chained behind
// it. The edge is stamped the way `moe chain edit` stamps one, because
// the groom's own fence would redirect a group aimed at that head and
// the fixture wants the batch actually assembled.
func handMintedHeadFixture(t *testing.T) (root, headKey string, groomed groomResult, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	minted := groomFixture(t, root, "fix-a")
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	headKey = "moe/" + head.ID
	chainEdgeCommit(t, root, headKey, "moe/"+minted["fix-a"])
	return root, headKey, groomChains(root, "moe", "pulse-groom",
		nil /*no groups*/, nil /*kickoff edges*/, io.Discard, os.Stderr), stages
}

// TestSelfKickSkipsAHandMintedChainHead: the operator composes a chain
// head over an afternoon and hangs work off it. The `chain` workflow's
// ladder is empty by design, so a hand-minted head is never past its
// first stage and carries no machine or chore seed — it stays with the
// operator.
func TestSelfKickSkipsAHandMintedChainHead(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, headKey, groomed, stages := handMintedHeadFixture(t)

	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: headKey}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on an operator-rooted thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "waiting at its first stage with only a seed") {
		t.Errorf("stderr = %q, want the settled-design guard named", errb.String())
	}
}

// TestSelfKickDoesNotEnumerateAStagedBatch is the same fence seen from
// the board rather than the gate. Enumeration walks every parked run,
// and the fix behind the operator's head is settled and would otherwise
// clear the floor on its own — the head is what holds it, and it holds
// it *silently*: staging is a normal state, not a hold worth a line per
// sweep. The heartbeat's parked leg skips it the same way, which is why
// both sides read the same predicate.
func TestSelfKickDoesNotEnumerateAStagedBatch(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, headKey, groomed, stages := handMintedHeadFixture(t)

	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing out of a batch the operator is staging", kickStages(*stages))
	}
	if strings.Contains(errb.String(), headKey) {
		t.Errorf("stderr = %q, want the staging fence held without a line", errb.String())
	}
	if !strings.Contains(errb.String(), "nothing parked — nothing to start") {
		t.Errorf("stderr = %q, want the empty board reported", errb.String())
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
		[]groomGroup{{Runs: runsFrom("promoted-sketch")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/promoted-sketch"}), io.Discard, &errb)

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
		[]groomGroup{{Runs: runsFrom("design-done")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/design-done"}), io.Discard, &errb)

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
		[]groomGroup{{Runs: runsFrom("mid-ladder")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: "moe/mid-ladder"}), io.Discard, &errb)

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
		[]groomGroup{{Runs: runsFrom("readme-update-2026-07-22")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

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
		[]groomGroup{{Runs: runsFrom("advanced-run")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

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
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on an out-dated marker", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "waiting at its first stage with its turn closed but not advanced") {
		t.Errorf("stderr = %q, want the hold to name the closed turn, not a seed", errb.String())
	}
}

// TestSelfKickHoldsAParkedThread: the survey's one veto. A `park`
// reason is the marked case now — the thread is groomed and ready and
// the harness would start it, and the sentence the survey spent is what
// stops it. The reason prints verbatim, because it is the only account
// the operator gets of why a `!!!!` ended with this thread sitting.
func TestSelfKickHoldsAParkedThread(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Park: "fix-b touches the push path"}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a parked thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), threadRoot+" parked by the survey — fix-b touches the push path") {
		t.Errorf("stderr = %q, want the park reason reported verbatim", errb.String())
	}
}

// TestSelfKickRidesAnUnaskedThread is the default flip. Nothing in this
// gate asked for anything: the survey groomed a thread and wrote no
// `park`, and under a dynamic ride that is consent to start it. Three
// strandings bought this default — the survey used to have to spend
// confidence to cause motion, and kept declining to.
func TestSelfKickRidesAnUnaskedThread(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("nothing was driven; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+threadRoot) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickKicksASharedRootOnce is the collision the default flip
// makes ordinary: two groups grooming into the same thread — the second
// `onto` a run the first placed — hand back two groomedThreads with one
// root. Kicking it twice would start the ride, then start its finished
// remains. A park on either group holds the thread, because the park is
// a claim about the thread, not about the group that carried it.
func TestSelfKickKicksASharedRootOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		second  groomedThread
		want    int
		wantErr string
	}{
		{name: "both unparked", want: 1},
		{
			name:    "second parks it",
			second:  groomedThread{Park: "the tail member is speculative"},
			want:    0,
			wantErr: "parked by the survey — the tail member is speculative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, threadRoot, groomed, _ := selfKickFixture(t)
			second := tc.second
			second.Root = threadRoot

			defer withRideMode(rideDynamic)()
			var errb bytes.Buffer
			pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}, second), io.Discard, &errb)

			if got := strings.Count(errb.String(), "kicking "+threadRoot); got != tc.want {
				t.Errorf("kicked %s %d time(s), want %d; stderr=%q", threadRoot, got, tc.want, errb.String())
			}
			if tc.wantErr != "" && !strings.Contains(errb.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want %q", errb.String(), tc.wantErr)
			}
		})
	}
}

// TestSelfKickAnnouncesOneStartPerParkedThread is this run's seed bug,
// pinned. Every "kicking" line is a claim that a ride is starting, and
// the kick loop's whole ethos is being the operator's account of an
// unattended sweep. Under the old model each kicked ride's own tail
// fired a nested pulse that started the *next* parked thread, so control
// came back here with the snapshot stale and the loop re-announced roots
// an inner sweep had already ridden — N threads produced N rides but
// N(N+1)/2 lines. With no in-process fire there is only one walker, so
// the counts are equal by construction.
func TestSelfKickAnnouncesOneStartPerParkedThread(t *testing.T) {
	root, stages, _ := kickFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b", "fix-c")

	defer withRideMode(rideDynamic)()
	// No groups: the three loose parked runs are already correctly
	// ordered, so the survey has nothing to groom and the kick's own
	// board enumeration is what finds them — the retry shape the
	// heartbeat re-offers.
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*no groups*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	var errb bytes.Buffer
	pulseSelfKick(root, groomed, io.Discard, &errb)

	for _, slug := range []string{"fix-a", "fix-b", "fix-c"} {
		key := "moe/" + minted[slug]
		if got := strings.Count(errb.String(), "kicking "+key); got != 1 {
			t.Errorf("announced %s %d time(s), want exactly 1; stderr=%q", key, got, errb.String())
		}
	}
	if got := strings.Count(errb.String(), "pulse: kicking "); got != 3 {
		t.Errorf("kick lines = %d, want 3 (one per parked thread); stderr=%q", got, errb.String())
	}
	// Each ride walks its run's whole ladder, so count the runs entered
	// rather than the dispatches: three announced starts, three rides.
	ridden := map[string]bool{}
	for _, inv := range *stages {
		ridden[inv.runID] = true
	}
	if len(ridden) != 3 {
		t.Errorf("drove %v, want one ride per parked thread", kickStages(*stages))
	}
}

// TestSelfKickReportsAnEmptyBoard closes the silent sweep. A dynamic
// sweep that found nothing at all used to end with no stderr line
// — the step returned before its first print — so the operator saw a
// sweep finish and a terminal go quiet with no account of why. Every
// dynamic invocation says what it did.
func TestSelfKickReportsAnEmptyBoard(t *testing.T) {
	root, stages, _ := kickFixture(t)

	defer withRideMode(rideDynamic)()
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*no groups*/, nil /*kickoff edges*/, io.Discard, os.Stderr)
	var errb bytes.Buffer
	pulseSelfKick(root, groomed, io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing from an empty board", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "nothing parked — nothing to start") {
		t.Errorf("stderr = %q, want the quiet sweep reported", errb.String())
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
		[]groomGroup{{Onto: "shipped", Runs: runsFrom("fix-a")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
	if len(groomed.threads) != 1 || groomed.threads[0].Root != shippedKey {
		t.Fatalf("threads = %+v, want one rooted at the merged anchor %s", groomed.threads, shippedKey)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: shippedKey}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a settled thread root", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "already settled") {
		t.Errorf("stderr = %q, want the settled-root skip named", errb.String())
	}
}

// strandedThreadFixture is the run this change opened on: an sdlc run
// the operator opened and drove through design and code, parked at
// review, correctly ordered and therefore invisible to a survey with no
// ordering opinion to write. Its gate is empty, which is what the two
// pulses of 2026-08-13 actually wrote.
//
// Callers arm the dynamic ride before calling: the groom builds the
// board's graph under one, and the kick enumerates off that graph.
func strandedThreadFixture(t *testing.T) (root, runKey string, groomed groomResult, stages *[]openSdlcStageInvocation) {
	t.Helper()
	root, stages, _ = kickFixture(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "stalled-at-review", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# The fix\n\nbody\n", "code": "# The diff\n\nbody\n"})
	advanceAt(t, root, "moe", "stalled-at-review", "design", now.Add(-4*time.Hour))
	advanceAt(t, root, "moe", "stalled-at-review", "code", now.Add(-2*time.Hour))

	groomed = groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)
	if len(groomed.threads) != 0 {
		t.Fatalf("threads = %+v, want none — the gate named nothing", groomed.threads)
	}
	return root, "moe/stalled-at-review", groomed, stages
}

// TestSelfKickRidesAThreadTheGateNeverNamed is the stranding. A failed
// ride re-arms the heartbeat, the heartbeat re-offers the board, and
// the retry sweep finds a thread already in the right order — so it
// grooms nothing, and a kick keyed on the gate's `threads` list had
// nothing to start. The retry was a vehicle that structurally could not
// perform the retry. Enumeration is what closes it: the ride resumes at
// the stage the run is parked at, replaying nothing.
func TestSelfKickRidesAThreadTheGateNeverNamed(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, runKey, groomed, stages := strandedThreadFixture(t)

	var errb bytes.Buffer
	pulseSelfKick(root, groomed, io.Discard, &errb)

	if got := kickStages(*stages); len(got) == 0 || got[0] != "stalled-at-review:review" {
		t.Fatalf("drove %v, want the ride to enter at review; stderr=%q", got, errb.String())
	}
	if !strings.Contains(errb.String(), "kicking "+runKey) {
		t.Errorf("stderr = %q, want the kick announced", errb.String())
	}
}

// TestSelfKickHoldsAnEnumeratedThreadTheSurveyParked: the survey keeps
// its one veto over work it did not groom. Naming the thread in a group
// with a `park` line is the whole grammar — restating an order that is
// already correct changes no edges, so the park is all the group does.
//
// It also pins the precedence the candidate order encodes: the groomed
// thread carries the park and is seen first, so the same root arriving
// from the board behind it is already spoken for.
func TestSelfKickHoldsAnEnumeratedThreadTheSurveyParked(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, runKey, _, stages := strandedThreadFixture(t)

	groomed := groomChains(root, "moe", "pulse-groom-2",
		[]groomGroup{{Runs: runsFrom("stalled-at-review"), Park: "the review canvas contradicts the design"}},
		nil /*kickoff edges*/, io.Discard, os.Stderr)

	var errb bytes.Buffer
	pulseSelfKick(root, groomed, io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a parked thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), runKey+" parked by the survey — the review canvas contradicts the design") {
		t.Errorf("stderr = %q, want the park reason reported verbatim", errb.String())
	}
}

// TestSelfKickKicksAnEnumeratedRootOnce: the gate groomed the thread
// and the board holds it too. One root, one ride — kicking it twice
// would start the ride and then start its finished remains.
func TestSelfKickKicksAnEnumeratedRootOnce(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, runKey, groomed, _ := strandedThreadFixture(t)

	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: runKey}), io.Discard, &errb)

	if got := strings.Count(errb.String(), "kicking "+runKey); got != 1 {
		t.Errorf("kicked %s %d time(s), want 1; stderr=%q", runKey, got, errb.String())
	}
}

// TestSelfKickHoldsEnumeratedFloorMisses: the floor is unchanged for
// work that arrives off the board rather than out of the gate, and it
// reports each hold in the same words. A seed-only root the operator is
// still composing, and a root somebody has a stage open on.
func TestSelfKickHoldsEnumeratedFloorMisses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(t *testing.T, root string)
		want  string
	}{
		{
			name:  "seed only",
			setUp: func(t *testing.T, root string) {},
			want:  "waiting at its first stage with only a seed",
		},
		{
			name: "session open",
			setUp: func(t *testing.T, root string) {
				advanceAt(t, root, "moe", "promoted-sketch", "design", time.Now().Local().Add(-2*time.Hour))
				if _, err := session.Open(root, "moe", "promoted-sketch", "code"); err != nil {
					t.Fatal(err)
				}
			},
			want: "live session at code",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, stages, _ := kickFixture(t)
			seedRun(t, root, "moe", "promoted-sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
				map[string]string{"design": "# A thought I had\n\nseed\n"})
			tc.setUp(t, root)

			groomed := groomChains(root, "moe", "pulse-groom",
				nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)
			var errb bytes.Buffer
			pulseSelfKick(root, groomed, io.Discard, &errb)

			if len(*stages) != 0 {
				t.Fatalf("drove %v, want the floor to hold it", kickStages(*stages))
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr = %q, want %q", errb.String(), tc.want)
			}
		})
	}
}

// TestSelfKickDoesNotEnumerateASettledThread: a thread whose root has
// merged is finished from the kick's point of view, and enumeration
// never offers one. The groomed path prints "already settled" because
// the survey named it; the board says nothing, because a merged run is
// not a decision anyone made this sweep.
func TestSelfKickDoesNotEnumerateASettledThread(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages, _ := kickFixture(t)
	minted := groomFixture(t, root, "shipped", "fix-a")
	chainEdgeCommit(t, root, "moe/"+minted["shipped"], "moe/"+minted["fix-a"])
	setRunStatus(t, root, "moe", minted["shipped"], run.StatusMerged)

	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)
	var errb bytes.Buffer
	pulseSelfKick(root, groomed, io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("drove %v, want nothing on a settled thread", kickStages(*stages))
	}
	if !strings.Contains(errb.String(), "nothing parked — nothing to start") {
		t.Errorf("stderr = %q, want the settled thread passed over in silence", errb.String())
	}
}

// TestKickableRootsAndTheParkedLegSeeOneBoard is the seam this change
// welded shut. The heartbeat sweeps because it found parked kickable
// work; the kick decides what may start. When the two enumerated the
// board separately they could disagree — the heartbeat re-offering a
// thread the kick structurally could not reach is how a run sat still
// for two days — so both now read one predicate.
func TestKickableRootsAndTheParkedLegSeeOneBoard(t *testing.T) {
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "loose-fix", "staged-fix", "shipped-fix")
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/"+head.ID, "moe/"+minted["staged-fix"])
	setRunStatus(t, root, "moe", minted["shipped-fix"], run.StatusMerged)

	sc := mustPulseScan(t, root)
	roots := kickableThreadRoots(sc.mds, sc.byKey, sc.graph, "moe")
	want := []string{"moe/" + minted["loose-fix"]}
	if strings.Join(roots, ",") != strings.Join(want, ",") {
		t.Fatalf("kickable roots = %v, want %v — the staged batch and the merged run are neither side's business", roots, want)
	}
	if got := parkedKickableThread(root, sc, "moe"); got != want[0] {
		t.Errorf("parked leg = %q, want the same root the kick would take, %q", got, want[0])
	}
}
