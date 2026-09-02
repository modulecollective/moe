package cli

import (
	"slices"
	"testing"
)

// The serve UI declarations are made in each workflow's init(), so the
// registry is fully populated by the time any test runs. These tests
// pin the composed shapes serve consumes — the same data cli/serve.go
// wires through Options.WorkflowUI / Options.TagWorkflows.

func TestLookupServeWorkflowUISdlc(t *testing.T) {
	ui, ok := lookupServeWorkflowUI("sdlc")
	if !ok {
		t.Fatal("sdlc should carry a serve declaration")
	}
	if want := []string{"design", "code", "test", "review"}; !slices.Equal(ui.Stages, want) {
		t.Errorf("sdlc web-visible stages = %v, want %v (push excluded)", ui.Stages, want)
	}
	if !ui.Close {
		t.Error("sdlc should report a registered close pipeline")
	}
}

// The operator-paced workflows all declare a serve UI. Each renders its
// web-visible stages — the set the advance mark is allowed to land on —
// and (where a close pipeline is registered) the close chip.
func TestLookupServeWorkflowUIOperatorPaced(t *testing.T) {
	cases := []struct {
		workflow string
		stages   []string
	}{
		{"sdlc", []string{"design", "code", "test", "review"}},
	}
	for _, tc := range cases {
		t.Run(tc.workflow, func(t *testing.T) {
			ui, ok := lookupServeWorkflowUI(tc.workflow)
			if !ok {
				t.Fatalf("%s should carry a serve declaration", tc.workflow)
			}
			if !slices.Equal(ui.Stages, tc.stages) {
				t.Errorf("%s web-visible stages = %v, want %v", tc.workflow, ui.Stages, tc.stages)
			}
			if !ui.Close {
				t.Errorf("%s should report a registered close pipeline", tc.workflow)
			}
		})
	}
}

// Workflows that declared nothing stay read-only in serve, even when
// they registered a CLI close (chat and chain do) or a cascade
// dispatcher (chat, pulse do). No serve declaration → no run-page
// affordances. chain is the deliberate case: it has no stages at all,
// so the head page offers no mark even though the operator can close
// and kick it from the CLI.
func TestLookupServeWorkflowUIUndeclared(t *testing.T) {
	for _, wf := range []string{"chat", "idea", "pulse", chainWorkflow, "nope"} {
		if _, ok := lookupServeWorkflowUI(wf); ok {
			t.Errorf("workflow %q should have no serve declaration", wf)
		}
	}
}

// The tag destinations serve offers, and their order: sdlc is pinned
// first because an untagged POST falls back to the list's head.
func TestServeTagWorkflows(t *testing.T) {
	got := serveTagWorkflows()
	if !slices.Equal(got, []string{"sdlc"}) {
		t.Errorf("tag workflows = %v, want [sdlc]", got)
	}
}

// Every workflow that registered the shared close skeleton is
// reachable through the registry serve's CloseRun callback dispatches
// by; idea's bespoke close stays out.
func TestCloseRegistrationsCoverCloseCommandWorkflows(t *testing.T) {
	for _, wf := range []string{"sdlc", "chat", "chain", "pulse"} {
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
	// The negative control below asserts the unset-env default, so clear
	// the handshake first: `moe serve` exports MOE_SERVE_AGENT=1 into
	// every agent it spawns, and this suite runs inside those stages.
	t.Setenv("MOE_SERVE_AGENT", "")
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
