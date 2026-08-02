package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// seedDoorRun stamps a single run of the given workflow and status onto
// a fresh bureaucracy, with MOE_HOME pointed at it.
func seedDoorRun(t *testing.T, projectID, runID, workflow, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, projectID)
	trailerstest.SeedRun(t, root, projectID, runID, workflow, status)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	return root
}

// stubOpenPulse swaps the survey opener for a counter, so the door tests
// assert on whether a session would have been opened without running an
// agent turn.
func stubOpenPulse(t *testing.T, calls *int) {
	t.Helper()
	prev := openPulse
	openPulse = func(_, _ string, _ bool, _ string, _ *pulseInterrupt, _, _ io.Writer) surveyOutcome {
		*calls++
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = prev })
}

// TestPulseStageRefusesTerminalRunAtInteractiveDoor: a closed sweep's
// filings can never land — they promote through the close harvest, and
// closeRunInProcess refuses an already-terminal run — so a session typed
// into one loses everything it writes. Happy-path sweeps auto-close,
// which puts most pulse runs in exactly this state.
func TestPulseStageRefusesTerminalRunAtInteractiveDoor(t *testing.T) {
	for _, status := range []string{run.StatusClosed, run.StatusMerged, run.StatusPromoted} {
		t.Run(status, func(t *testing.T) {
			seedDoorRun(t, "moe", "pulse-2026-07-31", pulseWorkflow, status)
			var calls int
			stubOpenPulse(t, &calls)

			var out, errb bytes.Buffer
			code := Run([]string{"pulse", "pulse", "moe/pulse-2026-07-31"}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s run; stdout=%q", status, out.String())
			}
			if calls != 0 {
				t.Fatalf("guard must refuse before the session opens; calls=%d", calls)
			}
			for _, want := range []string{
				"pulse pulse: moe/pulse-2026-07-31 is " + status + "; a sweep is not reopened",
				"hint: moe pulse log moe/pulse-2026-07-31 pulse",
				"hint: moe pulse new moe",
			} {
				if !strings.Contains(errb.String(), want) {
					t.Fatalf("missing %q:\n%s", want, errb.String())
				}
			}
		})
	}
}

// TestPulseStageRefusesWrongWorkflowRun: typing an sdlc slug at the
// pulse door used to write a survey skeleton into that run's document
// tree (in-progress) or refuse with sweep wording that never applied
// (terminal). The closed leg pins the ordering — workflow identity is
// the more fundamental mismatch, so it is checked before status.
func TestPulseStageRefusesWrongWorkflowRun(t *testing.T) {
	for _, status := range []string{run.StatusInProgress, run.StatusClosed} {
		t.Run(status, func(t *testing.T) {
			seedDoorRun(t, "moe", "some-feature", sdlcWorkflow, status)
			var calls int
			stubOpenPulse(t, &calls)

			var out, errb bytes.Buffer
			code := Run([]string{"pulse", "pulse", "moe/some-feature"}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s sdlc run; stdout=%q", status, out.String())
			}
			if calls != 0 {
				t.Fatalf("guard must refuse before the session opens; calls=%d", calls)
			}
			if want := "pulse pulse: moe/some-feature is a sdlc run, not pulse"; !strings.Contains(errb.String(), want) {
				t.Fatalf("missing %q:\n%s", want, errb.String())
			}
			if got := errb.String(); strings.Contains(got, "a sweep is not reopened") {
				t.Fatalf("workflow refusal must win over the terminal-sweep wording:\n%s", got)
			}
		})
	}
}

// TestPulseStageOpensInProgressRun: the door's remaining real use — a
// sweep that failed mid-flight and is still in-progress — is untouched.
func TestPulseStageOpensInProgressRun(t *testing.T) {
	seedDoorRun(t, "moe", "pulse-2026-07-31", pulseWorkflow, run.StatusInProgress)
	var calls int
	stubOpenPulse(t, &calls)

	var out, errb bytes.Buffer
	if code := Run([]string{"pulse", "pulse", "moe/pulse-2026-07-31"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", code, errb.String())
	}
	if calls != 1 {
		t.Fatalf("openPulse calls=%d, want 1", calls)
	}
}

// TestPulseStageMissingRunRefusesLoud: unlike twin, pulse has no
// downstream require* that owns the not-found wording — without this the
// operator got a raw error from inside runStageSession.
func TestPulseStageMissingRunRefusesLoud(t *testing.T) {
	seedDoorRun(t, "moe", "pulse-2026-07-31", pulseWorkflow, run.StatusInProgress)
	var calls int
	stubOpenPulse(t, &calls)

	var out, errb bytes.Buffer
	if code := Run([]string{"pulse", "pulse", "moe/nope"}, &out, &errb); code == 0 {
		t.Fatalf("expected non-zero; stdout=%q", out.String())
	}
	if calls != 0 {
		t.Fatalf("guard must refuse before the session opens; calls=%d", calls)
	}
	if want := "pulse pulse: run not found: moe/nope"; !strings.Contains(errb.String(), want) {
		t.Fatalf("missing %q:\n%s", want, errb.String())
	}
}
