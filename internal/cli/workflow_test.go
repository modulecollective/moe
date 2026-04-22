package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/request"
)

// TestWorkflowNextWalksStages exercises the satisfaction logic in the
// exact sequence a happy-path sdlc run takes: no turns → design → code
// → terminal. Then we flip status to pushed and confirm we're done.
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
	if kind != NextKindTerminal || next.Name != "push" {
		t.Fatalf("after code: expected terminal push, got kind=%v name=%v", kind, nameOrNil(next))
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
// push. This is the same rule `moe push` enforces as a hard gate.
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

// TestWorkRunDispatchesStageAndChainExits exercises the `moe work`
// dispatcher against a synthetic workflow: the first stage runs and
// commits a work turn, and with isTTY=false the dispatcher reports
// "next" and exits rather than prompting. This is the piped-stdin
// branch of the continue guard.
func TestWorkRunDispatchesStageAndChainExits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	ranSpec := false
	registerTestWorkflow(t, "wftest",
		&Command{Name: "spec", Run: func(_ []string, _ io.Writer, _ io.Writer) int {
			ranSpec = true
			commitWorkTurnAt(t, root, "reqid", "spec", time.Now().UTC())
			return 0
		}},
		&Command{Name: "impl", Run: func(_ []string, _ io.Writer, _ io.Writer) int {
			t.Fatal("impl should not run when continue prompt is skipped")
			return 0
		}},
	)
	seedRequestWithWorkflow(t, root, "proj", "reqid", "wftest")

	var out, errb strings.Builder
	code := runWorkInternal([]string{"proj", "reqid"}, strings.NewReader(""), false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !ranSpec {
		t.Fatalf("spec stage did not run, stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "next: moe wftest impl proj reqid") {
		t.Fatalf("expected next-step hint, got:\n%s", out.String())
	}
}

// TestWorkRunShortCircuitsOnNonZero confirms that a failing stage stops
// the loop with its exit code rather than plowing ahead.
func TestWorkRunShortCircuitsOnNonZero(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	registerTestWorkflow(t, "wftest2",
		&Command{Name: "brokenstage", Run: func(_ []string, _ io.Writer, _ io.Writer) int { return 7 }},
	)
	seedRequestWithWorkflow(t, root, "proj", "r2", "wftest2")

	var out, errb strings.Builder
	code := runWorkInternal([]string{"proj", "r2"}, strings.NewReader(""), false, &out, &errb)
	if code != 7 {
		t.Fatalf("expected passthrough exit 7, got %d stderr=%q", code, errb.String())
	}
}

// TestWorkRunNoProgressGuard exercises the same-stage re-entry guard:
// a stage that exits 0 without committing a work turn must not spin
// the dispatch loop.
func TestWorkRunNoProgressGuard(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	calls := 0
	registerTestWorkflow(t, "wftest3",
		&Command{Name: "idle", Run: func(_ []string, _ io.Writer, _ io.Writer) int {
			calls++
			return 0
		}},
	)
	seedRequestWithWorkflow(t, root, "proj", "r3", "wftest3")

	var out, errb strings.Builder
	code := runWorkInternal([]string{"proj", "r3"}, strings.NewReader(""), false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if calls != 1 {
		t.Fatalf("expected exactly one stage call, got %d", calls)
	}
	if !strings.Contains(out.String(), "produced no progress") {
		t.Fatalf("expected no-progress hint, got:\n%s", out.String())
	}
}

// registerTestWorkflow builds and registers a throwaway workflow for a
// test and deletes it when the test ends. Stages have no prereqs and
// no terminal — so Next walks them in order and reports Done after the
// last one is committed.
func registerTestWorkflow(t *testing.T, name string, stages ...*Command) {
	t.Helper()
	wf := NewWorkflow(name, "test-only")
	for _, s := range stages {
		wf.Register(s)
	}
	RegisterWorkflow(wf)
	t.Cleanup(func() { delete(workflows, name) })
}

// seedRequestWithWorkflow is seedRequest with a workflow override, so
// dispatcher tests can target a custom workflow instead of sdlc.
func seedRequestWithWorkflow(t *testing.T, root, projectID, reqID, wfName string) {
	t.Helper()
	seedRequest(t, root, projectID, reqID, request.StatusInProgress)
	md, err := request.Load(root, projectID, reqID)
	if err != nil {
		t.Fatal(err)
	}
	md.Workflow = wfName
	if err := request.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

func nameOrNil(c *Command) string {
	if c == nil {
		return "<nil>"
	}
	return c.Name
}
