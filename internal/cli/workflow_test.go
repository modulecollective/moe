package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/request"
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
	md := &request.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: request.StatusInProgress}

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

	md.Status = request.StatusPushed
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindDone || next != nil {
		t.Fatalf("after pushed: expected done, got kind=%v name=%v", kind, nameOrNil(next))
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
	md := &request.Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: request.StatusInProgress}

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
// "workflow silently forgot what it was" bugs.
func TestWorkflowNextUnknownWorkflow(t *testing.T) {
	if _, err := LookupWorkflow("bogus"); err == nil {
		t.Fatal("expected error for unknown workflow")
	} else if !strings.Contains(err.Error(), "known:") {
		t.Fatalf("error should list known workflows, got: %v", err)
	}
	// Empty name defaults to sdlc.
	if _, err := LookupWorkflow(""); err != nil {
		t.Fatalf("empty name should default to sdlc, got: %v", err)
	}
}

func nameOrNil(c *Command) string {
	if c == nil {
		return "<nil>"
	}
	return c.Name
}
