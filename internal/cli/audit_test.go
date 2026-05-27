package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestAuditRegistered partners with TestSDLCRegistered / TestMetaMoeRegistered:
// a registration drift in init() ordering would silently drop the
// audit workflow. Walking the typed CLI to print the group's usage is
// the cheapest integration check that both the CommandGroup and the
// Workflow registry hold the wiring.
func TestAuditRegistered(t *testing.T) {
	if _, err := LookupWorkflow(auditWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(auditWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("audit group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{auditWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "plan", "report", "close", "cat", "log"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("audit usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestAuditWorkflowStageOrder confirms the two-stage shape and the
// prereq edge. plan → report is the contract; adding or reordering
// stages should be a deliberate edit that updates this test.
func TestAuditWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow(auditWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{auditPlanDoc, auditReportDoc}
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
	if pre := wf.Prereqs(auditPlanDoc); len(pre) != 0 {
		t.Fatalf("plan prereqs=%v want empty", pre)
	}
	if pre := wf.Prereqs(auditReportDoc); len(pre) != 1 || pre[0] != auditPlanDoc {
		t.Fatalf("report prereqs=%v want [%s]", pre, auditPlanDoc)
	}
	if succ := wf.Successor(auditPlanDoc); succ != auditReportDoc {
		t.Fatalf("plan successor=%q want %q", succ, auditReportDoc)
	}
	if succ := wf.Successor(auditReportDoc); succ != "" {
		t.Fatalf("report successor=%q want empty (terminal stage)", succ)
	}
}

// TestBuildSystemPromptInjectsAuditPlanFragment is the wiring check:
// workflows/audit/plan.md lands in the prompt when the run names the
// audit workflow at the plan stage. Sentinel on the stage heading so
// the assertion survives minor body edits but flags a fragment-rename.
func TestBuildSystemPromptInjectsAuditPlanFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "checkup-2026-05-27",
		Project:  "moe",
		Workflow: auditWorkflow,
	}
	got, err := buildSystemPrompt(root, md, auditPlanDoc, "", nil)
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

// TestBuildSystemPromptInjectsAuditReportFragment is the wiring check
// for the report stage. The ethos block is what makes the report a
// review rather than a generic-AI-feedback dump; the budget framing
// keeps it ranked. Sentinels on both so a refactor that drops either
// becomes a failing test.
func TestBuildSystemPromptInjectsAuditReportFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "checkup-2026-05-27",
		Project:  "moe",
		Workflow: auditWorkflow,
	}
	got, err := buildSystemPrompt(root, md, auditReportDoc, "", nil)
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

// TestAuditCascadeDispatcherRegistered confirms the cascade driver
// can reach audit stages via the workflow-agnostic dispatcher
// registry. Without this wiring, `!` / `!<stage>` / `!!` / `!!!` at
// an audit run's chain prompt would print "workflow has no cascade
// dispatcher" and refuse to walk.
func TestAuditCascadeDispatcherRegistered(t *testing.T) {
	if d := lookupCascadeDispatcher(auditWorkflow); d == nil {
		t.Fatal("audit workflow has no cascade dispatcher registered")
	}
}
