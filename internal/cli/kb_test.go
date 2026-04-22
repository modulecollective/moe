package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestKBRegistered is the partner to TestSDLCRegistered: it guards
// against init() ordering / registration drift silently dropping the
// top-level `kb` command.
func TestKBRegistered(t *testing.T) {
	cmd, ok := commands["kb"]
	if !ok {
		t.Fatal(`expected top-level command "kb" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("kb command summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := cmd.Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "research", "summarize"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("kb usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestKBWorkflowStageOrder confirms the workflow's stage ladder is
// research → summarize and that `new` is a facade, not a stage.
func TestKBWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	want := []string{"research", "summarize"}
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

// TestKBWorkflowNextWalksStages mirrors TestWorkflowNextWalksStages
// for the kb ladder: no turns → research → summarize → done.
func TestKBWorkflowNextWalksStages(t *testing.T) {
	root := newTestBureaucracy(t)
	wf, err := LookupWorkflow("kb")
	if err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "r", Project: "p", Workflow: "kb", Status: run.StatusInProgress}

	next, kind, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "research" {
		t.Fatalf("no turns: expected stage research, got kind=%v name=%v", kind, nameOrNil(next))
	}

	t0 := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "p", "r", "kb", "research", t0)
	next, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindStage || next.Name != "summarize" {
		t.Fatalf("after research: expected stage summarize, got kind=%v name=%v", kind, nameOrNil(next))
	}

	commitWorkTurnAt(t, root, "p", "r", "kb", "summarize", t0.Add(time.Hour))
	_, kind, err = wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if kind != NextKindDone {
		t.Fatalf("after summarize: expected done, got kind=%v", kind)
	}
}

// TestBuildSystemPromptInjectsKBResearchFragment is the wiring check
// that stages/kb/research.md actually lands in the prompt when the
// run names the kb workflow. Matches the sdlc fragment injection
// tests — a broken embed directive or a moved file becomes a failing
// test here rather than a silent prompt regression.
func TestBuildSystemPromptInjectsKBResearchFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Title: "DNS basics", Workflow: "kb"}
	got, err := buildSystemPrompt(root, md, "research", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: research") {
		t.Fatalf("prompt missing research fragment heading:\n%s", got)
	}
}

func TestBuildSystemPromptInjectsKBSummarizeFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "dns-basics", Project: "tele", Title: "DNS basics", Workflow: "kb"}
	got, err := buildSystemPrompt(root, md, "summarize", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: summarize") {
		t.Fatalf("prompt missing summarize fragment heading:\n%s", got)
	}
}
