package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestPulseSurveyStampsNoKickWithoutDynamicConsent: a sweep without
// dynamic consent starts nothing, so a next section would be a hypothetical — noise on
// the one artifact this change is trying to make trustworthy.
func TestPulseSurveyStampsNoKickWithoutDynamicConsent(t *testing.T) {
	defer withRideMode(rideNone)()
	root, _, _ := kickFixture(t)
	groomFixture(t, root, "aa-board")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, admission *pulseAdmission, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok"}`)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	if code := runPulseSurvey(root, "moe", "", nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0", code)
	}
	closed := closedPulseRuns(t, root)
	if len(closed) != 1 {
		t.Fatalf("closed pulse runs = %v, want the swept run auto-closed", closed)
	}
	if canvas := readPulseCanvas(t, root, "moe", closed[0]); strings.Contains(canvas, "## Next") {
		t.Errorf("canvas =\n%s\nwant no kick section on a sweep that starts nothing", canvas)
	}
}

// TestPulseSurveyKickStampFailureLeavesRunOpen: the stamp rides inside
// the close, so a stamp that cannot be written fails the close — and the
// run stays open on the dash's ACTIVE list rather than closing with a
// report that is silent about its own queue. The half-written canvas is
// put back, because the dirty-tree gate is repo-wide and a leftover here
// wedges every later close in the bureaucracy.
func TestPulseSurveyKickStampFailureLeavesRunOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	groomFixture(t, root, "aa-board")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, admission *pulseAdmission, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok"}`)
		// Wedge the canvas read-only after the survey's own work turn
		// committed it, so the stamp's write is what fails.
		if err := os.Chmod(filepath.Join(root, run.ContentPath(projectID, runID, pulseDoc)), 0o444); err != nil {
			t.Error(err)
		}
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	runPulseSurvey(root, "moe", "" /*emitRun*/, nil /*pi*/, io.Discard, &errb)

	if closed := closedPulseRuns(t, root); len(closed) != 0 {
		t.Fatalf("closed pulse runs = %v, want none — a failed stamp must not close the run silent about its queue", closed)
	}
	open := openPulseRuns(t, root, "moe")
	if len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want the unclosed sweep left for review", open)
	}
	if canvas := readPulseCanvas(t, root, "moe", open[0]); strings.Contains(canvas, "## Next") {
		t.Errorf("canvas =\n%s\nwant the survey's report untouched after a failed stamp", canvas)
	}
	if !strings.Contains(errb.String(), "stamp next section") {
		t.Errorf("stderr = %q, want the stamp failure named", errb.String())
	}
	if dirty, err := dirtyOutsidePaths(root); err != nil || dirty {
		t.Errorf("tree dirty after a failed stamp (dirty=%v, err=%v); the repo-wide gate would wedge every later close", dirty, err)
	}
}

// TestPulseSurveyKickCloseRefusedBeforeStampLeavesRunAlone: the mirror
// of the stamp failure. The close refuses at its dirty-tree gate, before
// the stamp ever runs, so there is nothing to put back — and the restore
// that runs anyway on a failed stamped close must be a no-op: the canvas
// as the survey left it, nothing staged, the run open, and the
// operator's own dirty file untouched.
func TestPulseSurveyKickCloseRefusedBeforeStampLeavesRunAlone(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, _, _ := kickFixture(t)
	stray := filepath.Join(root, "stray.txt")
	if err := os.WriteFile(stray, []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Output(t, root, "add", "stray.txt")
	gittest.Output(t, root, "commit", "-q", "-m", "stray")

	var surveyed string
	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, admission *pulseAdmission, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok"}`)
		surveyed = readPulseCanvas(t, root, projectID, runID)
		// A tracked file dirty outside the run trips the close's
		// repo-wide gate before its cleanup is reached.
		if err := os.WriteFile(stray, []byte("operator's edit\n"), 0o644); err != nil {
			t.Error(err)
		}
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	runPulseSurvey(root, "moe", "" /*emitRun*/, nil /*pi*/, io.Discard, &errb)

	if !strings.Contains(errb.String(), "uncommitted changes") {
		t.Fatalf("stderr = %q, want the close refused at its dirty-tree gate", errb.String())
	}
	open := openPulseRuns(t, root, "moe")
	if len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want the refused sweep left open", open)
	}
	if canvas := readPulseCanvas(t, root, "moe", open[0]); canvas != surveyed {
		t.Errorf("canvas =\n%s\nwant the survey's report exactly as written:\n%s", canvas, surveyed)
	}
	if staged := gittest.Output(t, root, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Errorf("index carries %q after a refused close, want nothing staged", staged)
	}
	if body, err := os.ReadFile(stray); err != nil || string(body) != "operator's edit\n" {
		t.Errorf("stray.txt = %q (err=%v), want the operator's edit left alone", body, err)
	}
}
