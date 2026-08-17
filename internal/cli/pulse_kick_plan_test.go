package cli

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// planSteps renders a plan as "<root>|<source>|<outcome>" lines, which is
// how these tests read order and disposition in one comparison.
func planSteps(plan kickPlan) []string {
	out := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		source := "board"
		if step.Gate {
			source = "gate"
		}
		out = append(out, step.Root+"|"+source+"|"+kickStepOutcome(step))
	}
	return out
}

// TestKickPlanOrdersGateThreadsAheadOfTheBoard pins the precedence the
// candidate walk encodes, and pins it against sort order rather than
// with it: the gate's thread is named last alphabetically, so a plan
// that merely enumerated the board would put it second.
func TestKickPlanOrdersGateThreadsAheadOfTheBoard(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	minted := groomFixture(t, root, "aa-board", "zz-gate-head", "zz-gate-tail")
	groomed := groomChains(root, "moe", "pulse-groom",
		[]groomGroup{{Runs: runsFrom("zz-gate-head", "zz-gate-tail")}},
		nil /*kickoff edges*/, io.Discard, os.Stderr)

	got := planSteps(planKick(root, groomed))
	want := []string{
		"moe/" + minted["zz-gate-head"] + "|gate|queued — floor re-checked at start",
		"moe/" + minted["aa-board"] + "|board|queued — floor re-checked at start",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("plan = %v, want the groomed thread ahead of the board root: %v", got, want)
	}
}

// TestKickPlanNamesARootOnce: the gate groomed the thread and the board
// enumeration finds the same root behind it. One step, attributed to the
// gate — which is what carries any `park` the group wrote.
func TestKickPlanNamesARootOnce(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, threadRoot, groomed, _ := selfKickFixture(t)

	got := planSteps(planKick(root, wantKick(groomed, groomedThread{Root: threadRoot})))
	want := []string{threadRoot + "|gate|queued — floor re-checked at start"}
	if !slices.Equal(got, want) {
		t.Fatalf("plan = %v, want one step for the shared root: %v", got, want)
	}
}

// TestKickPlanCarriesTheParkSentence: the survey's veto reaches the plan
// verbatim, and it is upstream of the floor — a parked root carries no
// hold, because nothing asked the floor about it.
func TestKickPlanCarriesTheParkSentence(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, threadRoot, groomed, _ := selfKickFixture(t)
	const why = "the reflect would read a half-finished record"

	plan := planKick(root, wantKick(groomed, groomedThread{Root: threadRoot, Park: why}))
	if len(plan.Steps) != 1 || plan.Steps[0].Park != why {
		t.Fatalf("plan = %+v, want one step carrying the park sentence %q", plan.Steps, why)
	}
	if plan.Steps[0].Hold != "" {
		t.Errorf("parked step hold = %q, want none — the survey's veto is upstream of the floor", plan.Steps[0].Hold)
	}
}

// TestKickPlanNamesEachFloorHold: every reason the floor holds a root
// reaches the plan in the same vocabulary the kick's stderr skip line
// uses, so the stamped section and the line cannot drift into two
// wordings for one disk fact.
func TestKickPlanNamesEachFloorHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(t *testing.T, root string) string
		want  string
	}{
		{
			name: "settled thread",
			setUp: func(t *testing.T, root string) string {
				minted := groomFixture(t, root, "shipped", "fix-a")
				chainEdgeCommit(t, root, "moe/"+minted["shipped"], "moe/"+minted["fix-a"])
				setRunStatus(t, root, "moe", minted["shipped"], run.StatusMerged)
				return "moe/" + minted["shipped"]
			},
			want: "heads a thread that has already settled — skipping",
		},
		{
			name: "seed only",
			setUp: func(t *testing.T, root string) string {
				seedRun(t, root, "moe", "promoted-sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
					map[string]string{"design": "# A thought I had\n\nseed\n"})
				return "moe/promoted-sketch"
			},
			want: "is waiting at its first stage with only a seed — the operator holds the trigger",
		},
		{
			name: "session open",
			setUp: func(t *testing.T, root string) string {
				seedRun(t, root, "moe", "promoted-sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
					map[string]string{"design": "# A thought I had\n\nseed\n"})
				advanceAt(t, root, "moe", "promoted-sketch", "design", time.Now().Local().Add(-2*time.Hour))
				if _, err := session.Open(root, "moe", "promoted-sketch", "code"); err != nil {
					t.Fatal(err)
				}
				return "moe/promoted-sketch"
			},
			want: "has a live session at code — skipping",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, _, _ := kickFixture(t)
			threadRoot := tc.setUp(t, root)
			groomed := groomChains(root, "moe", "pulse-groom",
				nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

			plan := planKick(root, wantKick(groomed, groomedThread{Root: threadRoot}))
			if len(plan.Steps) != 1 || plan.Steps[0].Hold != tc.want {
				t.Fatalf("plan = %+v, want one step held with %q", plan.Steps, tc.want)
			}
		})
	}
}

// TestKickPlanIsEmptyOnAnEmptyBoard: nothing parked is a plan too, and
// the section it renders is what a sweep with nothing to start owes the
// operator instead of silence.
func TestKickPlanIsEmptyOnAnEmptyBoard(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	groomed := groomChains(root, "moe", "pulse-groom",
		nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, os.Stderr)

	if plan := planKick(root, groomed); len(plan.Steps) != 0 {
		t.Fatalf("plan = %+v, want no steps on an empty board", plan.Steps)
	}
}

// TestRenderKickSectionReadsAsAQueueNotAPromise: the section numbers the
// walk, names where each root came from, and says out loud that the
// floor is re-checked. Claiming execution would put a second decoy
// artifact where the first one cost three diagnosis runs.
func TestRenderKickSectionReadsAsAQueueNotAPromise(t *testing.T) {
	got := renderKickSection(kickPlan{Steps: []kickStep{
		{Root: "moe/serve-pause", Gate: true},
		{Root: "moe/rebase-failure", Gate: false},
		{Root: "moe/half-recorded", Gate: true, Park: "the reflect would read a half-finished record"},
		{Root: "moe/occupied", Gate: false, Hold: "has a live session at code — skipping"},
	}})
	for _, want := range []string{
		"## Kick\n",
		"the floor is re-checked as each root is reached",
		"1. moe/serve-pause — gate thread, queued — floor re-checked at start\n",
		"2. moe/rebase-failure — parked board, queued — floor re-checked at start\n",
		"3. moe/half-recorded — gate thread, parked by the survey — the reflect would read a half-finished record\n",
		"4. moe/occupied — parked board, has a live session at code — skipping\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section =\n%s\nwant %q", got, want)
		}
	}
	if strings.Contains(got, "will start") {
		t.Errorf("section =\n%s\nmust not promise execution", got)
	}
}

// TestRenderKickSectionOnAnEmptyPlan: the sweep that starts nothing says
// so, rather than stamping an empty heading the next reader has to
// interpret.
func TestRenderKickSectionOnAnEmptyPlan(t *testing.T) {
	if got := renderKickSection(kickPlan{}); got != "## Kick\n\nNothing parked — nothing to start.\n" {
		t.Errorf("section = %q, want the empty-board note", got)
	}
}
