package cli

import (
	"slices"
	"testing"
)

// The serve UI declarations are made in each workflow's init(), so the
// registry is fully populated by the time any test runs. These tests
// pin the composed shapes serve consumes — the same data cli/serve.go
// wires through Options.WorkflowUI / Options.NewRunWorkflows.

func TestLookupServeWorkflowUISdlc(t *testing.T) {
	ui, ok := lookupServeWorkflowUI("sdlc")
	if !ok {
		t.Fatal("sdlc should carry a serve declaration")
	}
	if want := []string{"design", "code", "test"}; !slices.Equal(ui.Stages, want) {
		t.Errorf("sdlc spawnable stages = %v, want %v (push excluded)", ui.Stages, want)
	}
	if !ui.Cascade {
		t.Error("sdlc should declare the cascade trio")
	}
	if !ui.Close {
		t.Error("sdlc should report a registered close pipeline")
	}
}

func TestLookupServeWorkflowUIPdlc(t *testing.T) {
	ui, ok := lookupServeWorkflowUI(pdlcWorkflow)
	if !ok {
		t.Fatal("pdlc should carry a serve declaration")
	}
	if want := []string{pdlcFrameDoc, pdlcPrdDoc, pdlcChunkDoc}; !slices.Equal(ui.Stages, want) {
		t.Errorf("pdlc spawnable stages = %v, want %v", ui.Stages, want)
	}
	if ui.Cascade {
		t.Error("pdlc must not declare cascade — its stage verbs have no --ship/--chain")
	}
	if !ui.Close {
		t.Error("pdlc should report a registered close pipeline")
	}
}

// Workflows that declared nothing stay read-only in serve, even when
// they registered a CLI close (chat does).
func TestLookupServeWorkflowUIUndeclared(t *testing.T) {
	for _, wf := range []string{"chat", "audit", "kb", "twin", "hooks", "idea", "nope"} {
		if _, ok := lookupServeWorkflowUI(wf); ok {
			t.Errorf("workflow %q should have no serve declaration", wf)
		}
	}
}

func TestServeNewRunWorkflows(t *testing.T) {
	got := serveNewRunWorkflows()
	if len(got) != 2 {
		t.Fatalf("new-run workflows = %+v, want exactly sdlc and pdlc", got)
	}
	if got[0].Name != "sdlc" || got[0].FirstStage != "design" || !got[0].Workspace {
		t.Errorf("first entry = %+v, want sdlc/design with workspace", got[0])
	}
	if got[1].Name != pdlcWorkflow || got[1].FirstStage != pdlcFrameDoc || got[1].Workspace {
		t.Errorf("second entry = %+v, want pdlc/frame without workspace", got[1])
	}
}

// Every workflow that registered the shared close skeleton is
// reachable through the registry serve's CloseRun callback dispatches
// by; idea's bespoke close stays out.
func TestCloseRegistrationsCoverCloseCommandWorkflows(t *testing.T) {
	for _, wf := range []string{"sdlc", "pdlc", "kb", "chat", "audit", "twin", "hooks", "chores"} {
		if _, ok := lookupCloseRegistration(wf); !ok {
			t.Errorf("workflow %q registered closeCommand but has no close registration", wf)
		}
	}
	if _, ok := lookupCloseRegistration("idea"); ok {
		t.Error("idea must not appear in the close registry — its close is bespoke")
	}
	reg, _ := lookupCloseRegistration("sdlc")
	if reg.cleanup == nil {
		t.Error("sdlc close registration should carry the workspace-release cleanup")
	}
	if reg.subject != sdlcCloseSubject {
		t.Errorf("sdlc close subject = %q, want %q", reg.subject, sdlcCloseSubject)
	}
}

// skipPostTurnPrompt is the gate runStageSession's tail runs before
// firing the chain prompt. The MOE_SERVE_AGENT handshake must suppress
// the prompt for every workflow's interactive stage — pdlc's openers
// pass neither SkipNextStage nor Headless, and a serve-spawned chunk
// sitting that fell through to promptPdlcHarvest would wedge on the
// PTY stdin nobody types into.
func TestSkipPostTurnPromptServeHandshake(t *testing.T) {
	pdlcShaped := stageSessionOpts{NeedsSandbox: true, EnforceSandboxBoundary: true}
	if skipPostTurnPrompt(pdlcShaped) {
		t.Fatal("without the handshake an interactive sitting should reach the prompt")
	}
	t.Setenv("MOE_SERVE_AGENT", "1")
	if !skipPostTurnPrompt(pdlcShaped) {
		t.Error("MOE_SERVE_AGENT=1 must suppress the post-turn prompt with no per-callsite flag")
	}
	t.Setenv("MOE_SERVE_AGENT", "")
	if !skipPostTurnPrompt(stageSessionOpts{SkipNextStage: true}) {
		t.Error("SkipNextStage callers (chat, push) must still suppress")
	}
	if !skipPostTurnPrompt(stageSessionOpts{Headless: true}) {
		t.Error("headless turns must still suppress")
	}
}
