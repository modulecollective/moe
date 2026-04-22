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
	if kind != NextKindStage || next.Name != "design" {
		t.Fatalf("no turns: expected stage design, got kind=%v name=%v", kind, nameOrNil(next))
	}

	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "r", "design", t0)
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "code" {
		t.Fatalf("after design: expected stage code, got kind=%v name=%v", kind, nameOrNil(next))
	}

	commitWorkTurnAt(t, root, "r", "code", t0.Add(time.Hour))
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "push" {
		t.Fatalf("after code: expected stage push, got kind=%v name=%v", kind, nameOrNil(next))
	}

	for _, terminal := range []string{run.StatusPushed, run.StatusMerged, run.StatusClosed} {
		md.Status = terminal
		next, kind, err = wf.Next(root, md)
		if err != nil {
			t.Fatal(err)
		}
		if kind != NextKindDone || next != nil {
			t.Fatalf("after %s: expected done, got kind=%v name=%v", terminal, kind, nameOrNil(next))
		}
	}
}

// TestWorkflowNextReopensStaleStage reproduces the readyToShip
// staleness rule: a design turn landing after the last code turn
// should kick Next back to "code" so the operator reconciles before
// the push stage can be satisfied.
func TestWorkflowNextReopensStaleStage(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress}

	t0 := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "r", "design", t0)
	commitWorkTurnAt(t, root, "r", "code", t0.Add(time.Hour))
	// Design reworked after the code turn: code becomes stale.
	commitWorkTurnAt(t, root, "r", "design", t0.Add(2*time.Hour))

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "code" {
		t.Fatalf("stale code: expected stage code, got kind=%v name=%v", kind, nameOrNil(next))
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

func nameOrNil(c *Command) string {
	if c == nil {
		return "<nil>"
	}
	return c.Name
}
