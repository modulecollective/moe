package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// The reap hold's timeline, fixed so every assertion in this file reads
// against one clock. reapAt is when the machine turn died; before and
// after straddle it.
var (
	reapBefore = time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	reapAt     = time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	reapAfter  = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
)

// reapHeldPhrase is the wording every surface reporting the hold has to
// carry, spelled out here rather than derived from reapHeldReason so
// the tests pin the operator-facing sentence rather than agreeing with
// whatever the code says today.
const reapHeldPhrase = "its design turn died and was reaped at 2026-08-22T09:00:00Z" +
	" — an operator touch on the thread releases it"

// runJournalCommit lands one run-scoped journal commit at a chosen
// committer time, with whatever extra trailers the caller needs. extra
// is the whole of the difference between an operator's commit and the
// machine's, which is the difference the release rule turns on.
func runJournalCommit(t *testing.T, root, projectID, runID, subject, extra string, when time.Time) {
	t.Helper()
	stamp := when.UTC().Format(time.RFC3339)
	body := subject + "\n\nMoE-Project: " + projectID + "\nMoE-Run: " + runID + "\n" + extra
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "--allow-empty", "-m", body)
}

// tombstone reaps a run the way tombstoneReap does: the note on
// run.json, plus the consent-stamped journal commit that carries it —
// which lands a second *after* the note's own timestamp. That ordering
// is the whole reason the release rule cannot be "any journal movement
// after At": the reap's own commit is journal movement after At.
func tombstone(t *testing.T, root, projectID, runID, doc string, at time.Time) {
	t.Helper()
	md := loadRun(t, root, projectID, runID)
	md.Reaped = &run.ReapNote{Doc: doc, At: at.UTC().Format(time.RFC3339), Tip: strings.Repeat("a", 40)}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", filepath.Join(run.Dir(projectID, runID), "run.json"))
	stamp := at.Add(time.Second).UTC().Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "-m", "reap: "+projectID+"/"+runID+"\n\nMoE-Project: "+projectID+
		"\nMoE-Run: "+runID+"\nMoE-Consent: dynamic\n")
}

// reapThread mints a two-run thread, applies each caller mutation, and
// grooms — in that order, because the groom's index has to see every
// commit the test landed. Returns the thread root.
func reapThread(t *testing.T, root string, mutate func(minted map[string]string)) (threadRoot string, groomed groomResult) {
	t.Helper()
	minted := groomFixture(t, root, "fix-a", "fix-b")
	mutate(minted)
	groomed = groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
	return "moe/" + minted["fix-a"], groomed
}

// holdOf runs the floor over the thread and returns its one step's hold.
func holdOf(t *testing.T, root, threadRoot string, groomed groomResult) string {
	t.Helper()
	plan := planKick(root, wantKick(groomed, groomedThread{Root: threadRoot}))
	if len(plan.Steps) != 1 {
		t.Fatalf("plan = %+v, want one step", plan.Steps)
	}
	return plan.Steps[0].Hold
}

// TestReapHoldSurvivesTheReapsOwnCommit is the loop pin, and the test
// that encodes the whole proof. The refusal cycle's one journal commit
// is the tombstone, it is run-scoped, and it lands after the At it
// carries — so the naive rule ("any journal movement after At releases")
// releases the hold in the same tick that armed it, and the next sweep
// burns another stage turn on the same refusal. It is stamped
// MoE-Consent, and that is the exclusion that makes the rule provable.
func TestReapHoldSurvivesTheReapsOwnCommit(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
		tombstone(t, root, "moe", minted["fix-a"], "design", reapAt)
	})

	if got := holdOf(t, root, threadRoot, groomed); got != "is held — "+reapHeldPhrase {
		t.Fatalf("hold = %q, want the reap hold — the machine's own tombstone must not release it", got)
	}
}

// TestReapHoldReleasesOnOperatorMovement: the operator's directive for
// this run, in both directions. An unstamped run-scoped commit after the
// note releases; the same commit before the note does not, because it is
// movement the operator made before the turn ever died.
func TestReapHoldReleasesOnOperatorMovement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		when     time.Time
		released bool
	}{
		{name: "after the reap", when: reapAfter, released: true},
		{name: "before the reap", when: reapBefore, released: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, _, _ := kickFixture(t)
			threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
				tombstone(t, root, "moe", minted["fix-a"], "design", reapAt)
				runJournalCommit(t, root, "moe", minted["fix-a"], "input: added a note",
					"MoE-Input-Added: 1\n", tc.when)
			})

			got := holdOf(t, root, threadRoot, groomed)
			if tc.released && got != "" {
				t.Fatalf("hold = %q, want none — an operator touch after the reap is the release", got)
			}
			if !tc.released && got == "" {
				t.Fatal("hold = none, want the reap hold — movement predating the note licenses nothing")
			}
		})
	}
}

// TestReapHoldReleasesFromAnyThreadMember: the release is thread-scoped
// for the same reason pendingInputOnThread is — operator prose lands at
// the run whose next agent needs it, which is routinely queued behind
// the head, not at the head the floor evaluates.
func TestReapHoldReleasesFromAnyThreadMember(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
		tombstone(t, root, "moe", minted["fix-a"], "design", reapAt)
		runJournalCommit(t, root, "moe", minted["fix-b"], "input: added a note",
			"MoE-Input-Added: 1\n", reapAfter)
	})

	if got := holdOf(t, root, threadRoot, groomed); got != "" {
		t.Fatalf("hold = %q, want none — a note at a member behind the head is movement on the thread", got)
	}
}

// TestReapHoldOnAMemberHoldsTheWholeThread: the floor evaluates a thread
// at its root, so a member's dead turn has to reach the root's answer —
// otherwise a ride from the head walks straight into it.
func TestReapHoldOnAMemberHoldsTheWholeThread(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
		tombstone(t, root, "moe", minted["fix-b"], "design", reapAt)
	})

	if got := holdOf(t, root, threadRoot, groomed); got != "is held — "+reapHeldPhrase {
		t.Fatalf("hold = %q, want the member's reap hold at the root", got)
	}
	// And the survey is told *which* member, which the design hold's
	// head-only annotation would not have said.
	block := chainStateBlock(mustPulseScan(t, root), "moe")
	if !strings.Contains(block, ", held: "+reapHeldPhrase) {
		t.Errorf("chain-state block does not annotate the reaped member behind the head:\n%s", block)
	}
}

// TestReapHoldReArmsAfterAFailedRetry is the bound the proof claims: k
// operator touches buy at most k retries. A release lets one kick
// through; if the stage refuses again the new reap writes a fresh At
// that postdates the movement which licensed it, and that movement now
// licenses nothing further.
func TestReapHoldReArmsAfterAFailedRetry(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	tombstone(t, root, "moe", minted["fix-a"], "design", reapAt)
	runJournalCommit(t, root, "moe", minted["fix-a"], "input: added a note",
		"MoE-Input-Added: 1\n", reapAfter)

	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)
	threadRoot := "moe/" + minted["fix-a"]
	if got := holdOf(t, root, threadRoot, groomed); got != "" {
		t.Fatalf("hold = %q, want the note to buy the first retry", got)
	}

	// The retry refuses too, and the heartbeat reaps it again — this time
	// at a moment after the note that licensed the retry.
	reArmed := reapAfter.Add(time.Hour)
	tombstone(t, root, "moe", minted["fix-a"], "design", reArmed)
	groomed = groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	got := holdOf(t, root, threadRoot, groomed)
	if !strings.Contains(got, reArmed.Format(time.RFC3339)) {
		t.Fatalf("hold = %q, want the re-armed note at %s — one movement, one retry",
			got, reArmed.Format(time.RFC3339))
	}
}

// TestReapHoldOnAnUnreadableTimestamp: the note is the operator's brake,
// so an At nothing can compare against holds rather than clears, and the
// skip line says what is wrong with it. Fail-open here would spend a
// stage turn per sweep in exactly the place the hold exists to protect.
func TestReapHoldOnAnUnreadableTimestamp(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
		md := loadRun(t, root, "moe", minted["fix-a"])
		md.Reaped = &run.ReapNote{Doc: "design", At: "not a time", Tip: strings.Repeat("a", 40)}
		if err := run.Save(root, md); err != nil {
			t.Fatal(err)
		}
		// Every kind of movement, so the hold is the parse failure's
		// doing and not an absence of touches.
		runJournalCommit(t, root, "moe", minted["fix-a"], "input: added a note",
			"MoE-Input-Added: 1\n", reapAfter)
	})

	got := holdOf(t, root, threadRoot, groomed)
	if !strings.Contains(got, "unreadable time (not a time)") {
		t.Fatalf("hold = %q, want the hold to name the unparseable timestamp", got)
	}
}

// TestReapHoldKeepsTheDesignHoldsReason: a root held for an unsettled
// design keeps the more specific reason. The floor's leg order is the
// contract the chain-state annotation mirrors, so the two surfaces
// cannot describe one run two ways.
func TestReapHoldKeepsTheDesignHoldsReason(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	seedRun(t, root, "moe", "promoted-sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# A thought I had\n\nseed\n"})
	tombstone(t, root, "moe", "promoted-sketch", "design", reapAt)
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	want := "is waiting at its first stage with only a seed — the operator holds the trigger"
	if got := holdOf(t, root, "moe/promoted-sketch", groomed); got != want {
		t.Fatalf("hold = %q, want the design hold %q", got, want)
	}
}

// TestReapHoldSurfacesAgree: the kick's stderr skip line, the `## Kick`
// section it stamps, and the chain-state block the survey reads all
// carry the identical phrase. Two artifacts wording one disk fact two
// ways is how an agent learns to read them as two different states.
func TestReapHoldSurfacesAgree(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages, _ := kickFixture(t)
	threadRoot, groomed := reapThread(t, root, func(minted map[string]string) {
		tombstone(t, root, "moe", minted["fix-a"], "design", reapAt)
	})
	asked := wantKick(groomed, groomedThread{Root: threadRoot})

	var errb bytes.Buffer
	pulseSelfKick(root, asked, io.Discard, &errb)
	if len(*stages) != 0 {
		t.Fatalf("the kick started %v on a reap-held thread", kickStages(*stages))
	}
	surfaces := map[string]string{
		"skip line":    errb.String(),
		"kick section": renderKickSection(planKick(root, asked)),
		"chain-state":  chainStateBlock(mustPulseScan(t, root), "moe"),
	}
	for name, got := range surfaces {
		if !strings.Contains(got, reapHeldPhrase) {
			t.Errorf("%s missing the hold's phrase %q:\n%s", name, reapHeldPhrase, got)
		}
	}
	if !strings.Contains(surfaces["chain-state"], ", held: "+reapHeldPhrase) {
		t.Errorf("chain-state block does not annotate the reaped member:\n%s", surfaces["chain-state"])
	}
}

// TestParkedKickableThreadSkipsAReapHeldThreadWhole: the heartbeat's
// pre-ask stands down rather than spending a pulse turn per idle tick on
// a board whose only thread the kick would then hold. Skipped whole,
// members included — the reap's recovery is an operator touch, already
// on the dash, so there is no stranded-work case for descending.
func TestParkedKickableThreadSkipsAReapHeldThreadWhole(t *testing.T) {
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "fix-a", "fix-b")
	groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("fix-a", "fix-b")}}, nil /*kickoff edges*/, io.Discard, os.Stderr)

	want := "moe/" + minted["fix-a"]
	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Fatalf("parkedKickableThread = %q, want the parked thread %q before any reap", got, want)
	}

	tombstone(t, root, "moe", minted["fix-b"], "design", reapAt)
	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Fatalf("parkedKickableThread = %q, want \"\" — a reaped member holds its whole thread", got)
	}

	runJournalCommit(t, root, "moe", minted["fix-b"], "input: added a note",
		"MoE-Input-Added: 1\n", reapAfter)
	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Fatalf("parkedKickableThread = %q, want %q — the operator's touch re-opens the board", got, want)
	}
}
