package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// TestPulseRegistered partners with TestSDLCRegistered: a registration
// drift in init() would silently drop the pulse workflow. Walking the
// typed CLI to print the group's usage is the cheapest check that both
// the CommandGroup and the Workflow registry hold the wiring.
func TestPulseRegistered(t *testing.T) {
	if _, err := LookupWorkflow(pulseWorkflow); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup(pulseWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("pulse group summary should not be empty")
	}
	var out, errb bytes.Buffer
	code := Run([]string{pulseWorkflow}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", pulseDoc, "close", "cat", "log"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("pulse usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestPulseWorkflowSingleStage confirms the one-stage shape: no prereqs,
// no successor. Adding a stage should be a deliberate edit that updates
// this test.
func TestPulseWorkflowSingleStage(t *testing.T) {
	wf, err := LookupWorkflow(pulseWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	got := wf.Stages()
	if len(got) != 1 || got[0] != pulseDoc {
		t.Fatalf("stages=%v want [%s]", got, pulseDoc)
	}
	if pre := wf.Prereqs(pulseDoc); len(pre) != 0 {
		t.Fatalf("pulse prereqs=%v want empty", pre)
	}
	if succ := wf.Successor(pulseDoc); succ != "" {
		t.Fatalf("pulse successor=%q want empty (single terminal stage)", succ)
	}
}

// TestBuildSystemPromptInjectsPulseFragment is the wiring check:
// workflows/pulse/pulse.md lands in the prompt at the pulse stage.
// Sentinels on the stage heading and the one idiom the fragment owns
// (the Pull next grammar) so the assertion flags a fragment rename or a
// dropped idiom.
func TestBuildSystemPromptInjectsPulseFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "pulse-2026-07-17",
		Project:  "moe",
		Workflow: pulseWorkflow,
	}
	got, err := buildSystemPrompt(root, md, pulseDoc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: pulse") {
		t.Fatalf("prompt missing pulse fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "Pull next") {
		t.Fatalf("pulse.md missing the Pull next grammar it owns:\n%s", got)
	}
}

// TestPulseCascadeDispatcherRegistered confirms the cascade driver can
// reach the pulse stage via the workflow-agnostic registry — the same
// invariant every workflow but idea satisfies.
func TestPulseCascadeDispatcherRegistered(t *testing.T) {
	if d := lookupCascadeDispatcher(pulseWorkflow); d == nil {
		t.Fatal("pulse workflow has no cascade dispatcher registered")
	}
}

// stubFirePulse replaces the fire hook with a recorder for the duration
// of a test, returning the accumulator.
func stubFirePulse(t *testing.T) *[]string {
	t.Helper()
	var fired []string
	orig := firePulse
	firePulse = func(root, projectID string, stdout, stderr io.Writer) {
		fired = append(fired, projectID)
	}
	t.Cleanup(func() { firePulse = orig })
	return &fired
}

// TestPulseFiresFromSDLCClose: closing an sdlc run — run traffic — tails
// a pulse for the run's project.
func TestPulseFiresFromSDLCClose(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	// sdlc close's cleanup tears down the run's workspace; a plain dir
	// stands in so the test needs no live submodule.
	if err := os.MkdirAll(sandbox.Path(root, "tele", "ship-it"), 0o755); err != nil {
		t.Fatal(err)
	}
	fired := stubFirePulse(t)

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if len(*fired) != 1 || (*fired)[0] != "tele" {
		t.Fatalf("firePulse fired %v, want one fire for tele", *fired)
	}
}

// TestPulseDoesNotFireFromServeClose: serve dispatches closes through the
// same closeRunInProcess seam, but a browser POST has no Ctrl-C for the
// blocking survey and the chore auto-open would bypass serve's --insecure
// spawn gate — so serve passes tailPulse=false. Driving the seam exactly
// as serve's CloseRun callback does (registry lookup, skipEdit=true,
// tailPulse=false) pins that an sdlc close through serve stays quiet.
func TestPulseDoesNotFireFromServeClose(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := os.MkdirAll(sandbox.Path(root, "tele", "ship-it"), 0o755); err != nil {
		t.Fatal(err)
	}
	fired := stubFirePulse(t)

	reg, ok := lookupCloseRegistration("sdlc")
	if !ok {
		t.Fatal("sdlc has no close registration")
	}
	if err := closeRunInProcess(root, "sdlc", reg.subject, reg.cleanup,
		"tele", "ship-it", true /*skipEdit*/, false /*tailPulse*/, io.Discard, io.Discard); err != nil {
		t.Fatalf("closeRunInProcess: %v", err)
	}
	if len(*fired) != 0 {
		t.Fatalf("firePulse fired %v, want no fire for a serve close", *fired)
	}
}

// TestPulseDoesNotFireFromChatClose: chat is not run traffic — closing a
// chat run must not pulse. This is the workflow guard that also keeps
// chat/kb/hooks/chores/idea and pulse itself out.
func TestPulseDoesNotFireFromChatClose(t *testing.T) {
	root := seedCloseFixture(t, "tele", "just-chatting", "chat", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	fired := stubFirePulse(t)

	var out, errb bytes.Buffer
	if code := Run([]string{"chat", "close", "--no-edit", "tele/just-chatting"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if len(*fired) != 0 {
		t.Fatalf("firePulse fired %v, want no fire for a chat close", *fired)
	}
}

// TestPulseDoesNotFireFromEnterTerminal is the sync-exclusion guard.
// sync's reconcile flips a merged PR's status via enterTerminal
// directly; keeping the hook out of that shared helper is what excludes
// sync. Driving enterTerminal and asserting no fire pins the mechanism.
func TestPulseDoesNotFireFromEnterTerminal(t *testing.T) {
	root := seedCloseFixture(t, "tele", "reconciled", "sdlc", run.StatusPushed)
	fired := stubFirePulse(t)

	md, err := run.Load(root, "tele", "reconciled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enterTerminal(root, md, run.StatusMerged, true); err != nil {
		t.Fatalf("enterTerminal: %v", err)
	}
	if len(*fired) != 0 {
		t.Fatalf("firePulse fired %v, want none from enterTerminal (sync's path)", *fired)
	}
}

// TestPulseSurveySingleFlight: an open pulse run gates the survey. The
// manual path refuses loudly and names the run; the hook path skips
// quietly. Neither reaches the agent turn, so no survey stub is needed.
func TestPulseSurveySingleFlight(t *testing.T) {
	root := seedCloseFixture(t, "moe", "pulse-open", pulseWorkflow, run.StatusInProgress)

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", true /*manual*/, io.Discard, &errb); code != 1 {
		t.Fatalf("manual survey exit=%d, want 1 (refusal); stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "already has an open pulse run") {
		t.Fatalf("manual refusal should name the open run, got %q", errb.String())
	}

	if code := runPulseSurvey(root, "moe", false /*hook*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("hook survey exit=%d, want 0 (quiet skip)", code)
	}
}
