package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/workspace"
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
		Workflow: hooksWorkflow,
	}
	got, err := buildSystemPrompt(root, md, hooksCodeDoc, "", false, nil)
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

// TestHooksNewAcceptsWorkspaceAsLabel pins proposal #2 of
// hook-dev-cleanup: `moe hooks new --workspace=<name>` is allowed and
// records the name on run.json as a no-claim label. The workspace
// claim pre-flight is sdlc-only — a hooks run binding to a workspace
// that an sdlc run already claims is the explicit non-conflict the
// design names.
func TestHooksNewAcceptsWorkspaceAsLabel(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Pre-claim the workspace under a fake sdlc run; the hooks new
	// path must ignore the claim because it's a label, not a binding.
	if _, err := workspace.Acquire(root, "tele", "www-dev", "tele/some-sdlc-run"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := runNew(hooksWorkflow, []string{"--workspace=www-dev", "tele/tighten-dev-env"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// run.json's workspace field is populated with the label.
	slug := "tighten-dev-env"
	mdPath := filepath.Join(root, "projects", "tele", "runs", slug, "run.json")
	body, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	if !strings.Contains(string(body), `"workspace": "www-dev"`) {
		t.Errorf("run.json missing workspace label:\n%s", body)
	}

	// Claim is unchanged — hooks didn't touch it.
	holder, err := workspace.ReadClaim(root, "tele", "www-dev")
	if err != nil {
		t.Fatal(err)
	}
	if holder == nil || holder.Run != "tele/some-sdlc-run" {
		t.Fatalf("hooks new should not touch the existing claim; holder=%+v", holder)
	}
}

// TestHooksCodeKickoffMentionsWorkspace verifies the hooks code
// kickoff prompt names the bound workspace path when the run was opened
// with --workspace, so the agent can answer "where do I cd to fire
// hooks?" without the operator spelling out the layout.
func TestHooksCodeKickoffMentionsWorkspace(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProjectWithSubmodule(t, root, "tele")
	t.Setenv("MOE_HOME", root)

	md, err := run.New(root, "tele", run.Options{
		ID:        "tighten-dev-env",
		Workflow:  hooksWorkflow,
		Workspace: "www-dev",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := buildHooksCodeKickoff(md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "www-dev") {
		t.Errorf("kickoff missing workspace name:\n%s", got)
	}
	wsPath := workspace.Path(root, "tele", "www-dev")
	if !strings.Contains(got, wsPath) {
		t.Errorf("kickoff missing workspace path %q:\n%s", wsPath, got)
	}
}

// TestHooksCodeKickoffOmitsWorkspaceWhenUnset confirms the kickoff is
// the unadorned base when the run has no workspace label.
func TestHooksCodeKickoffOmitsWorkspaceWhenUnset(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)

	md, err := run.New(root, "tele", run.Options{
		ID:       "plain-hooks-run",
		Workflow: hooksWorkflow,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := buildHooksCodeKickoff(md.Project, md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "named workspace") {
		t.Errorf("kickoff should not mention workspace when unset:\n%s", got)
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
