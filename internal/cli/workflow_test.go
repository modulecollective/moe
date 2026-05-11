package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestWorkflowNextWalksStages exercises the satisfaction logic in the
// exact sequence a happy-path sdlc run takes: no turns → design → code
// → push. Then we flip status to pushed and confirm we're done.
//
// Under the forward-walking rule, a stage with a committed turn but
// no successor turn after it stays parked at that stage. So after
// each stage's first commit Next reports the just-finished stage as
// the parked one — dash and resume see "next: design" right after
// design committed, "next: code" right after code committed — and the
// run only progresses past it when either the next stage commits a
// fresher turn or the run reaches a terminal status.
func TestWorkflowNextWalksStages(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "design" {
		t.Fatalf("no turns: expected stage design, got kind=%v name=%q", kind, next)
	}

	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "p", "r", "sdlc", "design", t0)
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "design" {
		t.Fatalf("after design (no code yet): expected stage design (parked), got kind=%v name=%q", kind, next)
	}

	commitWorkTurnAt(t, root, "p", "r", "sdlc", "code", t0.Add(time.Hour))
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "code" {
		t.Fatalf("after code (no push yet): expected stage code (parked), got kind=%v name=%q", kind, next)
	}

	for _, terminal := range []string{run.StatusPushed, run.StatusMerged, run.StatusClosed} {
		md.Status = terminal
		next, kind, err = wf.Next(root, md)
		if err != nil {
			t.Fatal(err)
		}
		if kind != NextKindDone || next != "" {
			t.Fatalf("after %s: expected done, got kind=%v name=%q", terminal, kind, next)
		}
	}
}

// TestWorkflowNextReopensStaleStage covers the re-open case: a fresh
// design turn after code has already committed.
//
// Under the forward-walking rule, design's latest turn is now newer
// than code's, so design's "successor has a newer turn" check fails
// and design is unsatisfied — Next returns design. The previous
// backward-walking rule would have flipped Next to code (because code's
// prereq became newer); the new rule expresses the same staleness
// in the forward direction. Either way, the operator can't reach push
// without reconciling: re-running design parks the run at design until
// a fresh code turn lands.
func TestWorkflowNextReopensStaleStage(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}

	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "p", "r", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "p", "r", "sdlc", "code", t0.Add(time.Hour))
	// Design reworked after the code turn — design becomes parked
	// again, awaiting a fresh code turn.
	commitWorkTurnAt(t, root, "p", "r", "sdlc", "design", t0.Add(2*time.Hour))

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "design" {
		t.Fatalf("re-opened design: expected stage design, got kind=%v name=%q", kind, next)
	}

	// Land a fresh code turn — design's successor is now newer than
	// design, so design satisfies and code becomes the parked stage.
	commitWorkTurnAt(t, root, "p", "r", "sdlc", "code", t0.Add(3*time.Hour))
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "code" {
		t.Fatalf("re-coded after re-design: expected stage code, got kind=%v name=%q", kind, next)
	}
}

// TestWorkflowNextUnknownWorkflow verifies LookupWorkflow surfaces a
// useful error for typos — it's the first line of defense against
// "workflow silently forgot what it was" bugs. Empty now errors the
// same way: run.Load requires the field, so empty should never
// reach LookupWorkflow in normal use.
func TestWorkflowNextUnknownWorkflow(t *testing.T) {
	for _, name := range []string{"bogus", ""} {
		if _, err := LookupWorkflow(name); err == nil {
			t.Fatalf("expected error for unknown workflow %q", name)
		} else if !strings.Contains(err.Error(), "known:") {
			t.Fatalf("error should list known workflows, got: %v", err)
		}
	}
}

// TestWorkflowNewFacadeNotInStageLadder guards the contract that
// RegisterFacade (used by `moe sdlc new`) adds to the dispatch table
// without participating in the stage ladder. A regression here would
// put "new" into Next()'s walk and make the workflow perpetually think
// the run hasn't started.
func TestWorkflowNewFacadeNotInStageLadder(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range wf.Stages() {
		if s == "new" {
			t.Fatalf("`new` leaked into Stages(): %v", wf.Stages())
		}
	}
}

// TestWorkflowNextIgnoresOtherProjectSameSlug: two projects opening a
// run with the same slug must not cross-satisfy each other's stages.
// Same-project cross-workflow collisions are handled at write time —
// runs/<slug> is a flat namespace, so slug uniqueness is enforced
// when a run is opened, not here.
func TestWorkflowNextIgnoresOtherProjectSameSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	// Project "a" has a full sdlc run for slug "fix-bug".
	commitWorkTurnAt(t, root, "a", "fix-bug", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "a", "fix-bug", "sdlc", "code", t0.Add(time.Hour))

	// Project "b" opens the same slug fresh. Should start at design.
	md := &run.Metadata{ID: "fix-bug", Project: "b", Workflow: "sdlc", Status: run.StatusInProgress}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "design" {
		t.Fatalf("expected project b to start at design, got kind=%v name=%q", kind, next)
	}
}

// TestWorkflowNextIgnoresSessionStartCommit guards the latent bug the
// design called out: a `work: start session for code` commit carries
// every MoE trailer stageSatisfied looked at before the subject pin,
// so without the `^work: update …$` anchor it flipped the stage to
// satisfied before the agent had actually done anything. The anchored
// subject grep keeps session-start commits out of the match.
func TestWorkflowNextIgnoresSessionStartCommit(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("quick")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "quick", Status: run.StatusInProgress,
		Documents: map[string]*run.Document{}}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "code"); err != nil {
		t.Fatalf("commitSessionStart: %v", err)
	}

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next != "code" {
		t.Fatalf("session-start alone must not satisfy code, got kind=%v name=%q", kind, next)
	}
}

// TestWorkflowSuccessor covers the pure DAG lookup the chain prompt
// uses to ask "what's next after the stage I just finished?". The
// answer is decoupled from Next() — under the forward-walking rule
// Next() reports the just-finished stage as parked, which is the
// wrong answer for the chain prompt.
func TestWorkflowSuccessor(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		stage string
		want  string // "" means no successor or unknown stage
	}{
		{"design", "code"},
		{"code", "push"},
		{"push", ""}, // terminal stage
		{"bogus", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := wf.Successor(tc.stage); got != tc.want {
			t.Errorf("Successor(%q) = %q, want %q", tc.stage, got, tc.want)
		}
	}
}
