package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestReviewRegistered partners with TestSDLCRegistered / TestMetaMoeRegistered:
// a registration drift in init() ordering would silently drop the
// review workflow. Walking the typed CLI to print the group's usage is
// the cheapest integration check that both the CommandGroup and the
// Workflow registry hold the wiring.
func TestReviewRegistered(t *testing.T) {
	if _, err := LookupWorkflow(reviewWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(reviewWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("review group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{reviewWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "plan", "report", "close", "cat", "log"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("review usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestReviewWorkflowStageOrder confirms the two-stage shape and the
// prereq edge. plan → report is the contract; adding or reordering
// stages should be a deliberate edit that updates this test.
func TestReviewWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow(reviewWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{reviewPlanDoc, reviewReportDoc}
	if len(got) != len(want) {
		t.Fatalf("stages=%v want=%v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Fatalf("stages=%v want=%v", got, want)
		}
	}
	// plan has no prereqs; report names plan as its prereq so the
	// chain prompt's Successor lookup wires plan→report.
	if pre := wf.Prereqs(reviewPlanDoc); len(pre) != 0 {
		t.Fatalf("plan prereqs=%v want empty", pre)
	}
	if pre := wf.Prereqs(reviewReportDoc); len(pre) != 1 || pre[0] != reviewPlanDoc {
		t.Fatalf("report prereqs=%v want [%s]", pre, reviewPlanDoc)
	}
	if succ := wf.Successor(reviewPlanDoc); succ != reviewReportDoc {
		t.Fatalf("plan successor=%q want %q", succ, reviewReportDoc)
	}
	if succ := wf.Successor(reviewReportDoc); succ != "" {
		t.Fatalf("report successor=%q want empty (terminal stage)", succ)
	}
}

// TestBuildSystemPromptInjectsReviewPlanFragment is the wiring check:
// workflows/review/plan.md lands in the prompt when the run names the
// review workflow at the plan stage. Sentinel on the stage heading so
// the assertion survives minor body edits but flags a fragment-rename.
func TestBuildSystemPromptInjectsReviewPlanFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "checkup-2026-05-27",
		Project:  "moe",
		Workflow: reviewWorkflow,
	}
	got, err := buildSystemPrompt(root, md, reviewPlanDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: plan") {
		t.Fatalf("prompt missing plan fragment heading:\n%s", got)
	}
	// The two-mode framing (interactive vs headless) is load-bearing:
	// without it the agent stalls in headless cascades waiting for an
	// operator reply that never comes.
	if !strings.Contains(got, "Headless default") {
		t.Fatalf("plan.md missing headless-default framing:\n%s", got)
	}
}

// TestBuildSystemPromptInjectsReviewReportFragment is the wiring check
// for the report stage. The ethos block is what makes the report a
// review rather than a generic-AI-feedback dump; the budget framing
// keeps it ranked. Sentinels on both so a refactor that drops either
// becomes a failing test.
func TestBuildSystemPromptInjectsReviewReportFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "checkup-2026-05-27",
		Project:  "moe",
		Workflow: reviewWorkflow,
	}
	got, err := buildSystemPrompt(root, md, reviewReportDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: report") {
		t.Fatalf("prompt missing report fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "Ethos") {
		t.Fatalf("report.md missing ethos block:\n%s", got)
	}
	if !strings.Contains(got, "Budget") {
		t.Fatalf("report.md missing budget block:\n%s", got)
	}
	if !strings.Contains(got, "Elon's algorithm") {
		t.Fatalf("report.md missing Elon's-algorithm reading frame:\n%s", got)
	}
}

// TestReviewHeadlessDispatcherRegistered confirms the cascade driver
// can reach review stages headlessly via the workflow-agnostic
// dispatcher registry. Without this wiring, `!` / `!<stage>` / `!!`
// at a review run's chain prompt would print "workflow has no headless
// dispatcher" and refuse to walk.
func TestReviewHeadlessDispatcherRegistered(t *testing.T) {
	if d := lookupHeadlessDispatcher(reviewWorkflow); d == nil {
		t.Fatal("review workflow has no headless dispatcher registered")
	}
}
