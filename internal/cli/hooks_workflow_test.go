package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestHooksWorkflowRegistered partners with TestSDLCRegistered: a
// registration drift in init() ordering would silently drop the hooks
// workflow.
func TestHooksWorkflowRegistered(t *testing.T) {
	if _, err := LookupWorkflow(hooksWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(hooksWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("hooks group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{hooksWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "code", "close"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("hooks usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestHooksWorkflowStageOrder confirms `code` is the only stage and
// `new` is a facade. A second stage later would require updating this
// test alongside the workflow.
func TestHooksWorkflowStageOrder(t *testing.T) {
	wf, err := LookupWorkflow(hooksWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	if len(got) != 1 || got[0] != hooksCodeDoc {
		t.Fatalf("stages=%v want=[%s]", got, hooksCodeDoc)
	}
}

// TestBuildSystemPromptInjectsHooksCodeFragment is the wiring check
// that workflows/hooks/code.md lands in the prompt when the run names
// the hooks workflow.
func TestBuildSystemPromptInjectsHooksCodeFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "hook-development-2026-05-13",
		Project:  "moe",
		Title:    "hook-development",
		Workflow: hooksWorkflow,
	}
	got, err := buildSystemPrompt(root, md, hooksCodeDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: code") {
		t.Fatalf("prompt missing code fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "moe hook fire") {
		t.Fatalf("hooks code.md should mention the fire verb:\n%s", got)
	}
}

// TestHooksStageHooksDirReturnsProjectHooks: the ExtraStagePaths
// callback returns projects/<p>/hooks so commitTurn stages the agent's
// hook edits alongside the canvas. Without this the per-turn commit
// would carry only the canvas and the hook edits would silently drop.
func TestHooksStageHooksDirReturnsProjectHooks(t *testing.T) {
	md := &run.Metadata{Project: "tele", ID: "x"}
	paths, err := hooksStageHooksDir("/tmp/whatever", md)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "projects/tele/hooks" {
		t.Fatalf("got %v, want [projects/tele/hooks]", paths)
	}
}
