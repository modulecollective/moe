package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestQuickRegistered partners with TestSDLCRegistered and
// TestKBRegistered: init() ordering or registration drift silently
// dropping the top-level `quick` command would break the whole
// workflow at dispatch.
func TestQuickRegistered(t *testing.T) {
	cmd, ok := commands["quick"]
	if !ok {
		t.Fatal(`expected top-level command "quick" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("quick command summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := cmd.Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "code", "push"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("quick usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestQuickWorkflowStageOrder confirms the stage ladder is code →
// push and that `new` is a facade, not a stage.
func TestQuickWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow("quick")
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{"code", "push"}
	if len(got) != len(want) {
		t.Fatalf("stages=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stages[%d]=%q want %q", i, got[i], want[i])
		}
	}
	for _, s := range got {
		if s == "new" {
			t.Fatalf("`new` leaked into Stages(): %v", got)
		}
	}
}

// TestQuickWorkflowNextWalksStages mirrors the sdlc/kb next-walk:
// no turns → code → push → done after push status.
func TestQuickWorkflowNextWalksStages(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("quick")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "quick", Status: run.StatusInProgress}

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "code" {
		t.Fatalf("no turns: expected stage code, got kind=%v name=%v", kind, nameOrNil(next))
	}

	t0 := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "p", "r", "quick", "code", t0)
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "push" {
		t.Fatalf("after code: expected stage push, got kind=%v name=%v", kind, nameOrNil(next))
	}

	// Push is terminal via status flip, not a work turn — mirror runPush.
	md.Status = run.StatusMerged
	_, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindDone {
		t.Fatalf("after merged: expected done, got kind=%v", kind)
	}
}

// TestBuildSystemPromptInjectsQuickCodeFragment is the wiring check
// that stages/quick/code.md lands in the prompt when the run names
// the quick workflow. A broken embed directive or a moved file
// becomes a failing test here rather than a silent prompt regression.
func TestBuildSystemPromptInjectsQuickCodeFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-typo", Project: "tele", Title: "Fix typo", Workflow: "quick"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: code") {
		t.Fatalf("prompt missing code fragment heading:\n%s", got)
	}
	// Quick-specific framing: the brief is the canvas, not a design doc.
	if !strings.Contains(got, "no design doc") {
		t.Fatalf("quick/code.md missing its no-design framing:\n%s", got)
	}
}

// TestBuildSystemPromptInjectsSharedFragmentsAtQuickCode confirms the
// shared-fragment gate admits quick/code alongside sdlc/design and
// sdlc/code. Without this, the widened allow-list in
// sharedStageFragments would be dead code.
func TestBuildSystemPromptInjectsSharedFragmentsAtQuickCode(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "fix-typo", Project: "tele", Title: "Fix typo", Workflow: "quick"}
	got, err := buildSystemPrompt(root, md, "code", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Before you start", "## Only edit this run"} {
		if !strings.Contains(got, want) {
			t.Fatalf("quick/code missing shared fragment %q:\n%s", want, got)
		}
	}
}

// TestRunNewFromIdeaSeedsQuickFirstStage mirrors
// TestRunNewFromIdeaWorksForKBFirstStage: the idea body lands in
// the quick workflow's first-stage doc, which is `code`, not
// `design`.
func TestRunNewFromIdeaSeedsQuickFirstStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	captureIdea(t, "tele", "Bump timeout")

	var out, errb bytes.Buffer
	code := runNew("quick", []string{"--from-idea=bump-timeout", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Quick's first stage is `code` — seed should land there.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "bump-timeout", "documents", "code", "content.md")); err != nil {
		t.Fatalf("quick's first-stage doc not seeded: %v", err)
	}
	// And not in a non-existent design dir.
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "bump-timeout", "documents", "design")); !os.IsNotExist(err) {
		t.Fatalf("quick run should not have a design dir; stat err=%v", err)
	}
}
