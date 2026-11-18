package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

const escalatedReviewCanvas = `# Review

## Gate

` + "```json" + `
{"status":"blocked","escalate":true}
` + "```" + `

## Findings

The retry loop in foo.go:42 still never resets the counter — the same
finding as last round, unrepaired.

## Evidence Reviewed

diff of foo.go; ran go test ./internal/foo.
`

// stubReviewDispatch swaps openSdlcStage for one that, on each review
// dispatch, optionally lands a review work-turn (as a real review turn
// would) and rewrites the canvas to whatever next returns.
func stubReviewDispatch(t *testing.T, root string, md *run.Metadata, commitTurn bool, next func(n int) string) *int {
	t.Helper()
	n := 0
	prev := openSdlcStage
	openSdlcStage = func(s, _, _ string, _ bool, _, _ io.Writer) int {
		if s == "review" {
			n++
			if commitTurn {
				trailerstest.CommitWorkTurnAt(t, root, md.Project, md.ID, "sdlc", "review", time.Time{})
			}
			writeStageCanvas(t, root, md, "review", next(n))
		}
		return 0
	}
	t.Cleanup(func() { openSdlcStage = prev })
	return &n
}

func commitReviewTurns(t *testing.T, root string, md *run.Metadata, n int) {
	t.Helper()
	for range n {
		trailerstest.CommitWorkTurnAt(t, root, md.Project, md.ID, "sdlc", "review", time.Time{})
	}
}

func runReviewGate(t *testing.T, md *run.Metadata) ([]cascadeStepResult, int, string) {
	t.Helper()
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	steps, _, code := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", true, &stdout, &stderr)
	return steps, code, stderr.String()
}

// TestReviewLoopRunsUntilReady: a review that blocks three times and
// then passes gets three headless kickbacks and three re-dispatches, and
// the cascade proceeds — no operator kick between rounds.
func TestReviewLoopRunsUntilReady(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	commitReviewTurns(t, root, md, 1)
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
	dispatches := stubReviewDispatch(t, root, md, true, func(n int) string {
		if n < 3 {
			return blockedReviewCanvas
		}
		return readyReviewCanvas
	})
	kicks := stubHeadlessKickback(t, 0, nil)

	steps, code, stderr := runReviewGate(t, md)
	if code != 0 {
		t.Fatalf("gate code=%d, want 0; stderr=%q", code, stderr)
	}
	if len(*kicks) != 3 || *dispatches != 3 {
		t.Fatalf("kickbacks=%d dispatches=%d, want 3 and 3", len(*kicks), *dispatches)
	}
	want := "build,review,build,review,build,review"
	var got []string
	for _, s := range steps {
		got = append(got, s.stage)
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("steps = %v, want %s", got, want)
	}
}

// TestReviewLoopStopsOnEscalation: the reviewer's escalate key stops the
// loop before any kickback, parks, and says why.
func TestReviewLoopStopsOnEscalation(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
	kicks := stubHeadlessKickback(t, 0, nil)

	steps, code, stderr := runReviewGate(t, md)
	if code != 1 || len(*kicks) != 0 || len(steps) != 0 {
		t.Fatalf("code=%d kicks=%d steps=%v, want a park with no kickback", code, len(*kicks), steps)
	}
	for _, want := range []string{"cascade: review stays blocked: the reviewer escalated", "parked at review"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q: %q", want, stderr)
		}
	}
}

// TestReviewLoopEscalatesMidLoop: a reviewer that blocks plainly, then
// escalates on the next round, stops after one kickback.
func TestReviewLoopEscalatesMidLoop(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
	stubReviewDispatch(t, root, md, true, func(int) string { return escalatedReviewCanvas })
	kicks := stubHeadlessKickback(t, 0, nil)

	_, code, stderr := runReviewGate(t, md)
	if code != 1 || len(*kicks) != 1 || !strings.Contains(stderr, "the reviewer escalated") {
		t.Fatalf("code=%d kicks=%d stderr=%q, want one kickback then an escalation park", code, len(*kicks), stderr)
	}
}

// TestReviewLoopJournalCap: the cap is read from the journal, so it
// binds across cascades. At the cap no kickback fires; one short of it,
// exactly one does, because the re-dispatch lands the tenth turn.
func TestReviewLoopJournalCap(t *testing.T) {
	for _, tc := range []struct {
		prior, wantKicks int
	}{
		{prior: maxReviewTurns, wantKicks: 0},
		{prior: maxReviewTurns - 1, wantKicks: 1},
	} {
		t.Run(strings.Repeat("r", tc.prior), func(t *testing.T) {
			root := isolateCascadeMoeHome(t)
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			commitReviewTurns(t, root, md, tc.prior)
			writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
			stubReviewDispatch(t, root, md, true, func(int) string { return blockedReviewCanvas })
			kicks := stubHeadlessKickback(t, 0, nil)

			_, code, stderr := runReviewGate(t, md)
			if code != 1 || len(*kicks) != tc.wantKicks {
				t.Fatalf("code=%d kicks=%d, want 1 and %d; stderr=%q", code, len(*kicks), tc.wantKicks, stderr)
			}
			if !strings.Contains(stderr, "review has run 10 times (cap 10)") {
				t.Fatalf("stderr should name the cap: %q", stderr)
			}
		})
	}
}

// TestReviewLoopCountErrorParks: a journal that can't be read counts as
// spent — park rather than spin.
func TestReviewLoopCountErrorParks(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
	prev := reviewTurns
	reviewTurns = func(string, *run.Metadata) (int, bool, error) { return 0, false, errors.New("boom") }
	t.Cleanup(func() { reviewTurns = prev })
	kicks := stubHeadlessKickback(t, 0, nil)

	_, code, stderr := runReviewGate(t, md)
	if code != 1 || len(*kicks) != 0 || !strings.Contains(stderr, "review turns could not be counted (boom)") {
		t.Fatalf("code=%d kicks=%d stderr=%q, want a park naming the read error", code, len(*kicks), stderr)
	}
}

// TestReviewLoopInProcessBound: a review that blocks forever while the
// journal count never grows (a round that lands no review turn) still
// terminates, at the cap's worth of rounds in this call.
func TestReviewLoopInProcessBound(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
	dispatches := stubReviewDispatch(t, root, md, false, func(int) string { return blockedReviewCanvas })
	kicks := stubHeadlessKickback(t, 0, nil)

	_, code, stderr := runReviewGate(t, md)
	if code != 1 || len(*kicks) != maxReviewTurns || *dispatches != maxReviewTurns {
		t.Fatalf("code=%d kicks=%d dispatches=%d, want 1, %d, %d", code, len(*kicks), *dispatches, maxReviewTurns, maxReviewTurns)
	}
	if !strings.Contains(stderr, "10 rounds in this cascade (cap 10)") {
		t.Fatalf("stderr should name the in-process bound: %q", stderr)
	}
}

// TestReviewGateEscalateParse: escalate is read only on a blocked gate,
// and never changes what the status-only readers see.
func TestReviewGateEscalateParse(t *testing.T) {
	readyEscalated := strings.Replace(escalatedReviewCanvas, `"status":"blocked"`, `"status":"ready"`, 1)
	if reviewGateEscalated(readyEscalated) {
		t.Error("escalate on a ready gate must be ignored")
	}
	if reviewGateEscalated(blockedReviewCanvas) {
		t.Error("a plain blocked gate is not escalated")
	}
	if !reviewGateEscalated(escalatedReviewCanvas) {
		t.Error("blocked + escalate must read as escalated")
	}
	if status, ok := stageGateStatus(escalatedReviewCanvas); !ok || status != "blocked" {
		t.Errorf("stageGateStatus = %q, %v; an escalated gate must still read blocked", status, ok)
	}
}

// TestReviewEscalatedStillOffersInteractiveKickback: the interactive
// kickback is uncapped and keys on the status alone, so an escalated
// review still earns the [Y/n] offer.
func TestReviewEscalatedStillOffersInteractiveKickback(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
	if _, blocked := cascadeStageBlocked(md, "review", io.Discard); !blocked {
		t.Fatal("an escalated review must read as blocked to the kickback readers")
	}
}

// TestKickFloorHoldsASpentReviewLoop: the sweep must not re-ride a run
// parked at a blocked review the loop gave up on, until a note arrives.
// The clears cases alone pass vacuously; the held cases are the ones a
// deleted leg turns red.
func TestKickFloorHoldsASpentReviewLoop(t *testing.T) {
	turn := func(doc string) func(t *testing.T, root string) {
		return func(t *testing.T, root string) {
			t.Helper()
			trailerstest.CommitWorkTurnAt(t, root, "moe", "loop", "sdlc", doc, time.Time{})
		}
	}
	reviews := func(n int) func(t *testing.T, root string) {
		return func(t *testing.T, root string) {
			t.Helper()
			for range n {
				turn("review")(t, root)
			}
		}
	}
	const escalatedHold = "is parked at a blocked review: the reviewer escalated — another build round won't clear it — answer its question, kick it back by hand, or close it"
	const capHold = "is parked at a blocked review: review has run 10 times (cap 10) — answer its question, kick it back by hand, or close it"
	for _, tc := range []struct {
		name   string
		canvas string
		setUp  []func(t *testing.T, root string)
		want   string
	}{
		{name: "escalated, held", canvas: escalatedReviewCanvas, setUp: []func(*testing.T, string){turn("build"), turn("review")}, want: escalatedHold},
		{name: "at the cap, held", canvas: blockedReviewCanvas, setUp: []func(*testing.T, string){turn("build"), reviews(maxReviewTurns)}, want: capHold},
		{name: "blocked with budget, rides", canvas: blockedReviewCanvas, setUp: []func(*testing.T, string){turn("build"), turn("review")}},
		{name: "ready review, rides", canvas: readyReviewCanvas, setUp: []func(*testing.T, string){turn("build"), reviews(maxReviewTurns)}},
		{name: "build answered since, rides", canvas: escalatedReviewCanvas, setUp: []func(*testing.T, string){turn("review"), turn("build")}},
		{
			name: "design answered since, rides", canvas: escalatedReviewCanvas,
			setUp: []func(*testing.T, string){turn("build"), turn("review"), turn("design"), func(t *testing.T, root string) {
				advanceAt(t, root, "moe", "loop", "design", time.Now().Local().Add(time.Minute))
			}},
		},
		{
			name: "an undelivered note releases it", canvas: escalatedReviewCanvas,
			setUp: []func(*testing.T, string){turn("build"), turn("review"), func(t *testing.T, root string) {
				if _, err := input.Add(root, "moe", "loop", "the counter reset is in bar.go, look there"); err != nil {
					t.Fatal(err)
				}
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer withRideMode(rideDynamic)()
			root, _, _ := kickFixture(t)
			seedRun(t, root, "moe", "loop", "sdlc", run.StatusInProgress, time.Now().Local(),
				map[string]string{"design": "# Loop\n\nbody\n", "review": tc.canvas})
			advanceAt(t, root, "moe", "loop", "design", time.Now().Local().Add(-time.Hour))
			for _, f := range tc.setUp {
				f(t, root)
			}
			groomed := groomChains(root, "moe", "pulse-groom", nil, nil, os.Stderr)
			if got := kickFloorHold(root, "moe/loop", groomed); got != tc.want {
				t.Errorf("kickFloorHold = %q, want %q", got, tc.want)
			}
		})
	}
}

// seedLiveReviewRun lays down the run.json input.Ask reads to accept an
// ask; the cascade fixtures otherwise carry only canvases.
func seedLiveReviewRun(t *testing.T, root string, md *run.Metadata) {
	t.Helper()
	seedRunJSON(t, root, md.Project, md.ID, md.Workflow, md.Status)
}

// openPings returns the run's open pings — never more than one when
// input.Ask is the only writer, so a second is a test failure.
func openPings(t *testing.T, root string, md *run.Metadata) []input.Entry {
	t.Helper()
	f, err := input.Load(root, md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	var open []input.Entry
	for _, e := range f.Notes {
		if e.Open() {
			open = append(open, e)
		}
	}
	return open
}

// TestSpentReviewAsksTheOperator: both spent exits — the reviewer's
// escalation and the journal cap — open one ping on the run, asked by
// the run, carrying the spent reason, and the park itself is unchanged.
func TestSpentReviewAsksTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name, canvas, reason string
		prior                int
	}{
		{name: "escalated", canvas: escalatedReviewCanvas, reason: "the reviewer escalated — another build round won't clear it"},
		{name: "cap", canvas: blockedReviewCanvas, prior: maxReviewTurns, reason: "review has run 10 times (cap 10)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := isolateCascadeMoeHome(t)
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			seedLiveReviewRun(t, root, md)
			commitReviewTurns(t, root, md, tc.prior)
			writeStageCanvas(t, root, md, "review", tc.canvas)
			kicks := stubHeadlessKickback(t, 0, nil)

			steps, code, stderr := runReviewGate(t, md)
			if code != 1 || len(*kicks) != 0 || len(steps) != 0 {
				t.Fatalf("code=%d kicks=%d steps=%v, want a park with no kickback; stderr=%q", code, len(*kicks), steps, stderr)
			}
			open := openPings(t, root, md)
			if len(open) != 1 {
				t.Fatalf("open pings = %+v, want one; stderr=%q", open, stderr)
			}
			want := "Review is blocked and the loop has stopped: " + tc.reason + "."
			if open[0].FirstLine() != want || open[0].AskedBy != "tele/fix-it" {
				t.Fatalf("ping = %+v, want first line %q asked by tele/fix-it", open[0], want)
			}
			if !strings.Contains(stderr, "cascade: review: asked the operator tele/fix-it#1") {
				t.Fatalf("stderr should name the ask: %q", stderr)
			}
		})
	}
}

// TestSpentReviewKeepsAnExistingPing: a run that already has a question
// in front of the operator keeps it — one open ping per run — and the
// park goes ahead unchanged.
func TestSpentReviewKeepsAnExistingPing(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	seedLiveReviewRun(t, root, md)
	if _, err := input.Ask(root, md.Project, md.ID, "tele/pulse-one", "Which store?", ""); err != nil {
		t.Fatal(err)
	}
	writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
	kicks := stubHeadlessKickback(t, 0, nil)

	steps, code, stderr := runReviewGate(t, md)
	if code != 1 || len(*kicks) != 0 || len(steps) != 0 {
		t.Fatalf("code=%d kicks=%d steps=%v, want the same park", code, len(*kicks), steps)
	}
	open := openPings(t, root, md)
	if len(open) != 1 || open[0].Question != "Which store?" {
		t.Fatalf("open pings = %+v, want only the original", open)
	}
	if !strings.Contains(stderr, "cascade: review: operator already has an open question on this run") {
		t.Fatalf("stderr should name the existing question: %q", stderr)
	}
}

// TestUnspentReviewAsksNothing is the negative control: a loop that
// blocks and then readies, and a non-yolo park, open no ping. Without
// it the asserts above pass on a helper that asks at every blocked
// round.
func TestUnspentReviewAsksNothing(t *testing.T) {
	t.Run("loop runs until ready", func(t *testing.T) {
		root := isolateCascadeMoeHome(t)
		md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
		seedLiveReviewRun(t, root, md)
		writeStageCanvas(t, root, md, "review", blockedReviewCanvas)
		stubReviewDispatch(t, root, md, true, func(n int) string {
			if n < 3 {
				return blockedReviewCanvas
			}
			return readyReviewCanvas
		})
		stubHeadlessKickback(t, 0, nil)
		if _, code, stderr := runReviewGate(t, md); code != 0 {
			t.Fatalf("gate code=%d, want 0; stderr=%q", code, stderr)
		}
		if open := openPings(t, root, md); len(open) != 0 {
			t.Fatalf("open pings = %+v, want none", open)
		}
	})
	t.Run("non-yolo park", func(t *testing.T) {
		root := isolateCascadeMoeHome(t)
		md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
		seedLiveReviewRun(t, root, md)
		writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
		stubHeadlessKickback(t, 0, nil)
		wf, _ := LookupWorkflow("sdlc")
		var stdout, stderr bytes.Buffer
		if _, parked, _ := cascadeStageGate(wf, cascadeDispatchers["sdlc"], md, "review", false, &stdout, &stderr); !parked {
			t.Fatalf("parked=false, want a park to prompt; stderr=%q", stderr.String())
		}
		if open := openPings(t, root, md); len(open) != 0 {
			t.Fatalf("open pings = %+v, want none", open)
		}
	})
}

// TestSpentReviewAnswerReleasesAndReasks is the round trip: the ask is
// not a work turn (so the hold survives the summons), the operator's
// answer releases the hold, and a review that escalates again on the
// released turn opens a fresh ping.
func TestSpentReviewAnswerReleasesAndReasks(t *testing.T) {
	defer withRideMode(rideDynamic)()
	realGate := checkCascadeStageGate
	root, _, _ := kickFixture(t)
	checkCascadeStageGate = realGate // kickFixture stubs it; the park needs the real gate
	md := &run.Metadata{ID: "loop", Project: "moe", Workflow: "sdlc", Status: run.StatusInProgress}
	seedRun(t, root, "moe", "loop", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# Loop\n\nbody\n", "review": escalatedReviewCanvas})
	advanceAt(t, root, "moe", "loop", "design", time.Now().Local().Add(-time.Hour))
	trailerstest.CommitWorkTurnAt(t, root, "moe", "loop", "sdlc", "build", time.Time{})
	commitReviewTurns(t, root, md, 1)
	stubHeadlessKickback(t, 0, nil)
	hold := func() string {
		t.Helper()
		return kickFloorHold(root, "moe/loop", groomChains(root, "moe", "pulse-groom", nil, nil, io.Discard))
	}

	if _, code, stderr := runReviewGate(t, md); code != 1 {
		t.Fatalf("gate code=%d, want the park; stderr=%q", code, stderr)
	}
	first := openPings(t, root, md)
	if len(first) != 1 {
		t.Fatalf("open pings = %+v, want one", first)
	}
	if n, last, err := reviewTurns(root, md); err != nil || n != 1 || !last {
		t.Fatalf("reviewTurns after ask = %d, %v, %v; want 1, true — the ask must not count as a work turn", n, last, err)
	}
	if h := hold(); !strings.Contains(h, "answer its question") {
		t.Fatalf("hold = %q, want the run held until the question is answered", h)
	}

	if _, err := input.Answer(root, "moe", "loop", 0, "the counter reset is in bar.go"); err != nil {
		t.Fatal(err)
	}
	if h := hold(); h != "" {
		t.Fatalf("an answered question should release the hold, got %q", h)
	}

	// The released review turn escalates again: a new exception, a new
	// question.
	commitReviewTurns(t, root, md, 1)
	if _, code, stderr := runReviewGate(t, md); code != 1 {
		t.Fatalf("gate code=%d, want the park; stderr=%q", code, stderr)
	}
	second := openPings(t, root, md)
	if len(second) != 1 || second[0].ID == first[0].ID {
		t.Fatalf("open pings = %+v, want one fresh ping beside answered #%d", second, first[0].ID)
	}
}

// TestCascadeFromGateSpentReviewAsks drives the ask through the real
// cascade call stack: a `!!` walk into an escalated review parks, and
// the ask commits — no repo lock is held at the park site.
func TestCascadeFromGateSpentReviewAsks(t *testing.T) {
	root := isolateCascadeMoeHome(t)
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	seedLiveReviewRun(t, root, md)
	writeStageCanvas(t, root, md, "review", escalatedReviewCanvas)
	writeStampedBuild(t, root, md)
	stubReviewDispatch(t, root, md, false, func(int) string { return escalatedReviewCanvas })
	kicks := stubHeadlessKickback(t, 0, nil)
	pushes := stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("review", "", false, false, md, &stdout, &stderr)
	if code == 0 || res.shipped || len(*kicks) != 0 || len(*pushes) != 0 {
		t.Fatalf("code=%d shipped=%v kicks=%d pushes=%d, want a park; stderr=%q", code, res.shipped, len(*kicks), len(*pushes), stderr.String())
	}
	if open := openPings(t, root, md); len(open) != 1 || open[0].AskedBy != "tele/fix-it" {
		t.Fatalf("open pings = %+v, want the run's own question; stderr=%q", open, stderr.String())
	}
}
