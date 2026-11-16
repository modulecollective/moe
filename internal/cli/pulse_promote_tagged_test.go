package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// A tagged idea starts on the next dynamic sweep whether or not the
// survey names it. 2026-11-15 is the case: two sweeps read the tag
// commits, wrote a quiet gate, and the kick reported "nothing parked".
// Every test here walks the post-gate half of runPulse — apply, the
// promote-the-rest step, groom, plan — the same sequence in the same
// order.

// sweepAfterGate runs the post-survey steps of a sweep against gate and
// returns the kick plan a dynamic sweep would stamp and walk.
func sweepAfterGate(t *testing.T, root string, gate pulseGate, stderr io.Writer) kickPlan {
	t.Helper()
	groups := applyPulseGate(root, "moe", "pulse-one", gate, io.Discard, stderr)
	promoteTaggedIdeas(root, "moe", "pulse-one", io.Discard, stderr)
	groomed := groomChains(root, "moe", "pulse-one", groups, nil /*kickoff edges*/, stderr)
	return planKick(root, groomed)
}

// promotedFrom returns the live sdlc run the pulse promoted idea into,
// failing the test if the idea is still parked.
func promotedFrom(t *testing.T, root, idea string) *run.Metadata {
	t.Helper()
	src, err := run.Load(root, "moe", idea)
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != run.StatusPromoted {
		t.Fatalf("idea %s status = %q, want promoted", idea, src.Status)
	}
	ids := runsWithWorkflow(t, root, "moe", "sdlc")
	if len(ids) != 1 {
		t.Fatalf("sdlc runs = %v, want exactly the one promoted from %s", ids, idea)
	}
	dest, err := run.Load(root, "moe", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

// TestQuietDynamicSweepPromotesATaggedIdea is the defect: a quiet gate
// under dynamic consent. The idea promotes into an ordinary machine
// spawn, the kick finds it on the board, and the stamped section names
// it as queued.
func TestQuietDynamicSweepPromotesATaggedIdea(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "split-run-stage-session")
	defer withRideMode(rideDynamic)()

	var errb bytes.Buffer
	plan := sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	dest := promotedFrom(t, root, "split-run-stage-session")
	if dest.SpawnedBy != "moe/pulse-one" {
		t.Errorf("spawned_by = %q, want the pulse — the kick floor admits machine spawns", dest.SpawnedBy)
	}
	if !strings.Contains(errb.String(), "pulse: promoted tagged idea moe/split-run-stage-session to sdlc run moe/"+dest.ID) {
		t.Errorf("stderr = %q, want the promotion line", errb.String())
	}
	if strings.Contains(errb.String(), "not promoted") {
		t.Errorf("stderr = %q, want no not-promoted line on the ordinary path", errb.String())
	}
	key := "moe/" + dest.ID
	if len(plan.Steps) != 1 || plan.Steps[0].Root != key || plan.Steps[0].Gate || plan.Steps[0].Park != "" || plan.Steps[0].Hold != "" {
		t.Fatalf("plan = %+v, want %s queued from the parked board; stderr=%q", plan.Steps, key, errb.String())
	}
	if section := renderKickSection(plan); !strings.Contains(section, "1. "+key+" — parked board, queued — floor re-checked at start") {
		t.Errorf("kick section = %q, want the promoted run queued", section)
	}
}

// TestHandTypedSweepLeavesATaggedIdeaParked: dynamic consent is the one
// door to machine-rooted motion. Without it, only what the gate names
// promotes — as before.
func TestHandTypedSweepLeavesATaggedIdeaParked(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "split-run-stage-session")

	var errb bytes.Buffer
	sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	idea, err := run.Load(root, "moe", "split-run-stage-session")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusInProgress {
		t.Errorf("idea status = %q, want it left parked outside dynamic consent", idea.Status)
	}
	if got := runsWithWorkflow(t, root, "moe", "sdlc"); len(got) != 0 {
		t.Errorf("sdlc runs = %v, want none", got)
	}
}

// TestDynamicSweepLeavesAnUntaggedIdeaAlone: untagged means human, and
// the new step must not widen that fence.
func TestDynamicSweepLeavesAnUntaggedIdeaAlone(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{
		ID: "needs-triage", Workflow: "idea", SeedDocs: map[string]string{"idea": "# Needs triage\n"},
	}); err != nil {
		t.Fatal(err)
	}
	defer withRideMode(rideDynamic)()

	var errb bytes.Buffer
	plan := sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	idea, err := run.Load(root, "moe", "needs-triage")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusInProgress {
		t.Errorf("idea status = %q, want it left parked", idea.Status)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("plan = %+v, want nothing to start", plan.Steps)
	}
	if strings.Contains(errb.String(), "requires operator triage") {
		t.Errorf("stderr = %q, want silence — the step only looks at tagged ideas", errb.String())
	}
}

// TestGateParkOnANamedTaggedIdeaPromotesOnce: the survey keeps its
// veto over an idea it names. The gate promotes it at its thread
// position; the new step must not promote it again, and the kick holds
// it with the survey's reason.
func TestGateParkOnANamedTaggedIdeaPromotesOnce(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "split-journal-index-walk")
	defer withRideMode(rideDynamic)()

	th := stringThread("split-journal-index-walk")
	th.Park = "the index walk lands after the stage split"
	var errb bytes.Buffer
	plan := sweepAfterGate(t, root, pulseGate{Status: "ok", Threads: []pulseThread{th}}, &errb)

	dest := promotedFrom(t, root, "split-journal-index-walk")
	if n := strings.Count(errb.String(), "pulse: promoted tagged idea"); n != 1 {
		t.Errorf("promotion lines = %d, want 1; stderr=%q", n, errb.String())
	}
	key := "moe/" + dest.ID
	if len(plan.Steps) != 1 || plan.Steps[0].Root != key || !plan.Steps[0].Gate || plan.Steps[0].Park != th.Park {
		t.Fatalf("plan = %+v, want %s held by the survey's park; stderr=%q", plan.Steps, key, errb.String())
	}
}

// TestQuietDynamicSweepCarriesTheDesignOnlyTag: a design-only tag rides
// one stage, the same as when the survey named it.
func TestQuietDynamicSweepCarriesTheDesignOnlyTag(t *testing.T) {
	root := spawnFixture(t)
	seedDesignOnlyIdea(t, root, "moe", "worth-a-think")
	defer withRideMode(rideDynamic)()

	var errb bytes.Buffer
	plan := sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	dest := promotedFrom(t, root, "worth-a-think")
	if !dest.DesignOnly {
		t.Errorf("run.json design_only = false, want the idea's licence carried")
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Root != "moe/"+dest.ID || !plan.Steps[0].OneStage || plan.Steps[0].Hold != "" {
		t.Fatalf("plan = %+v, want a one-stage queued ride; stderr=%q", plan.Steps, errb.String())
	}
}

// TestQuietDynamicSweepPromotesEveryTaggedIdeaInIDOrder: no cap, and a
// deterministic order the survey can override by naming them.
func TestQuietDynamicSweepPromotesEveryTaggedIdeaInIDOrder(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "split-run-stage-session")
	seedTaggedIdea(t, root, "moe", "split-journal-index-walk")
	defer withRideMode(rideDynamic)()

	var errb bytes.Buffer
	sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	if got := runsWithWorkflow(t, root, "moe", "idea"); len(got) != 0 {
		t.Errorf("ideas still parked = %v, want both promoted", got)
	}
	out := errb.String()
	a := strings.Index(out, "tagged idea moe/split-journal-index-walk")
	b := strings.Index(out, "tagged idea moe/split-run-stage-session")
	if a < 0 || b < 0 || a > b {
		t.Errorf("stderr = %q, want both promoted, journal-index-walk first", out)
	}
}

// TestDynamicSweepSaysWhyASiblingedTaggedIdeaStayed: a second live run
// on the idea's slug base keeps it from promoting — the seam's dedupe —
// and the step names that run instead of skipping in silence. The
// sibling is opened with IDBase, so it lands dated: the shape a prior
// promotion whose idea mark failed leaves behind.
func TestDynamicSweepSaysWhyASiblingedTaggedIdeaStayed(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "has-a-sibling")
	sib, err := run.New(root, "moe", run.Options{IDBase: "has-a-sibling", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	if sib.ID == "has-a-sibling" {
		t.Fatalf("sibling id = %q, want a dated id beside the idea", sib.ID)
	}
	defer withRideMode(rideDynamic)()

	var errb bytes.Buffer
	sweepAfterGate(t, root, pulseGate{Status: "ok"}, &errb)

	idea, err := run.Load(root, "moe", "has-a-sibling")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusInProgress {
		t.Errorf("idea status = %q, want it left parked", idea.Status)
	}
	out := errb.String()
	want := "pulse: promote: tagged idea moe/has-a-sibling not promoted — live run moe/" + sib.ID + " already holds its slug; `moe idea close moe/has-a-sibling`"
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("stderr = %q, want exactly one line naming sibling %s", out, sib.ID)
	}
	if strings.Contains(out, "pulse: promoted tagged idea") {
		t.Errorf("stderr = %q, want no promotion", out)
	}
}
