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
	if want := []string{"design", "code", "review", "test"}; !slices.Equal(ui.Stages, want) {
		t.Errorf("sdlc spawnable stages = %v, want %v (push excluded)", ui.Stages, want)
	}
	if !ui.Cascade {
		t.Error("sdlc should declare the cascade trio")
	}
	if !ui.Close {
		t.Error("sdlc should report a registered close pipeline")
	}
}

// The operator-paced workflows all declare a serve UI now, deriving the
// cascade trio from operatorCascades rather than a per-decl bit. Each
// renders its spawnable stages, the cascade chips, and (where a close
// pipeline is registered) the close chip.
func TestLookupServeWorkflowUIOperatorPaced(t *testing.T) {
	cases := []struct {
		workflow string
		stages   []string
	}{
		{"twin", []string{"vision", "architecture", "patterns", "operations", "glossary", "finalize"}},
		{"kb", []string{"research", "summarize"}},
		{"hooks", []string{"code"}},
		{"chores", []string{"code"}},
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			ui, ok := lookupServeWorkflowUI(tc.workflow)
			if !ok {
				t.Fatalf("%s should carry a serve declaration", tc.workflow)
			}
			if !slices.Equal(ui.Stages, tc.stages) {
				t.Errorf("%s spawnable stages = %v, want %v", tc.workflow, ui.Stages, tc.stages)
			}
			if !ui.Cascade {
				t.Errorf("%s should declare the cascade trio (operatorCascades)", tc.workflow)
			}
			if !ui.Close {
				t.Errorf("%s should report a registered close pipeline", tc.workflow)
			}
		})
	}
}

// Workflows that declared nothing stay read-only in serve, even when
// they registered a CLI close (chat does) or a cascade dispatcher
// (chat, pulse do). No serve declaration → no run-page affordances.
func TestLookupServeWorkflowUIUndeclared(t *testing.T) {
	for _, wf := range []string{"chat", "idea", "pulse", "nope"} {
		if _, ok := lookupServeWorkflowUI(wf); ok {
			t.Errorf("workflow %q should have no serve declaration", wf)
		}
	}
}

func TestServeNewRunWorkflows(t *testing.T) {
	got := serveNewRunWorkflows()
	if len(got) != 1 {
		t.Fatalf("new-run workflows = %+v, want exactly sdlc", got)
	}
	if got[0].Name != "sdlc" || got[0].FirstStage != "design" || !got[0].Workspace {
		t.Errorf("first entry = %+v, want sdlc/design with workspace", got[0])
	}
}

// Every workflow that registered the shared close skeleton is
// reachable through the registry serve's CloseRun callback dispatches
// by; idea's bespoke close stays out.
func TestCloseRegistrationsCoverCloseCommandWorkflows(t *testing.T) {
	for _, wf := range []string{"sdlc", "kb", "chat", "twin", "hooks", "chores", "pulse"} {
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
// the prompt for every workflow's interactive stage — a sandbox-boundary
// stage (design, chat) passes neither SkipNextStage nor Headless, so a
// serve-spawned sitting that fell through to the chain prompt would
// wedge on the PTY stdin nobody types into.
func TestSkipPostTurnPromptServeHandshake(t *testing.T) {
	sandboxShaped := stageSessionOpts{NeedsSandbox: true, EnforceSandboxBoundary: true}
	if skipPostTurnPrompt(sandboxShaped) {
		t.Fatal("without the handshake an interactive sitting should reach the prompt")
	}
	t.Setenv("MOE_SERVE_AGENT", "1")
	if !skipPostTurnPrompt(sandboxShaped) {
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
