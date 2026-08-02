package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// seedPulseRun stamps a single pulse run in the given status onto a
// fresh bureaucracy, with MOE_HOME pointed at it.
func seedPulseRun(t *testing.T, projectID, runID, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, projectID)
	trailerstest.SeedRun(t, root, projectID, runID, pulseWorkflow, status)
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
			seedPulseRun(t, "moe", "pulse-2026-07-31", status)
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

// TestPulseStageOpensInProgressRun: the door's remaining real use — a
// sweep that failed mid-flight and is still in-progress — is untouched.
func TestPulseStageOpensInProgressRun(t *testing.T) {
	seedPulseRun(t, "moe", "pulse-2026-07-31", run.StatusInProgress)
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
	seedPulseRun(t, "moe", "pulse-2026-07-31", run.StatusInProgress)
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
