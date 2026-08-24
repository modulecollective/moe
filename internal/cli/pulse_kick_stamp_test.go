package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestPulseSurveyStampsTheKickOrder is the whole point of the change. A
// dynamic sweep's canvas — the artifact the operator reads when a thread
// looks stuck — now ends with the queue that sweep is about to run, in
// the close's own commit. Three diagnosis runs in four days each
// re-derived this by hand from a canvas that stopped at "parked".
func TestPulseSurveyStampsTheKickOrder(t *testing.T) {
	defer withRideMode(rideDynamic)()
	root, stages, _ := kickFixture(t)
	minted := groomFixture(t, root, "aa-board", "zz-gate-head", "zz-gate-tail")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID,
			`{"status":"ok","threads":[{"runs":["zz-gate-head","zz-gate-tail"]}]}`)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil /*pi*/, io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d, want 0; stderr=%q", code, errb.String())
	}

	closed := closedPulseRuns(t, root)
	if len(closed) != 1 {
		t.Fatalf("closed pulse runs = %v, want the swept run auto-closed", closed)
	}
	canvas := readPulseCanvas(t, root, "moe", closed[0])
	wantOrder := []string{
		"1. moe/" + minted["zz-gate-head"] + " — gate thread, queued",
		"2. moe/" + minted["aa-board"] + " — parked board, queued",
	}
	for _, want := range wantOrder {
		if !strings.Contains(canvas, want) {
			t.Errorf("canvas =\n%s\nwant %q", canvas, want)
		}
	}
	if !strings.Contains(canvas, "## Gate") {
		t.Errorf("canvas =\n%s\nwant the survey's own report kept intact", canvas)
	}
	if first, second := strings.Index(canvas, wantOrder[0]), strings.Index(canvas, wantOrder[1]); first > second {
		t.Errorf("canvas =\n%s\nwant the gate thread stamped ahead of the board root", canvas)
	}

	// The stamp rides the close's own commit. Separate commits could
	// diverge — the close succeeding while the section was lost is the
	// silence this change exists to remove.
	touched := strings.Fields(gittest.Output(t, root, "show", "--name-only", "--format=", "HEAD"))
	canvasRel := run.ContentPath("moe", closed[0], pulseDoc)
	runJSONRel := filepath.Join(run.Dir("moe", closed[0]), "run.json")
	if !slices.Contains(touched, canvasRel) || !slices.Contains(touched, runJSONRel) {
		t.Errorf("close commit touched %v, want both the kick section (%s) and the status flip (%s) in one commit",
			touched, canvasRel, runJSONRel)
	}
	if dirty, err := dirtyOutsidePaths(root); err != nil || dirty {
		t.Errorf("tree dirty after the stamp (dirty=%v, err=%v); it must be committed", dirty, err)
	}

	// And the loop still executes what the section describes.
	if got := kickStages(*stages); len(got) == 0 {
		t.Fatalf("nothing was driven; stderr=%q", errb.String())
	}
	for _, id := range []string{minted["zz-gate-head"], minted["aa-board"]} {
		if !strings.Contains(errb.String(), "pulse: kicking moe/"+id+" (dynamic)") {
			t.Errorf("stderr = %q, want %s kicked as the section says", errb.String(), id)
		}
	}
}

// TestPulseSurveyStampsNoKickWithoutDynamicConsent: a static sweep
// starts nothing, so a kick section would be a hypothetical — noise on
// the one artifact this change is trying to make trustworthy.
func TestPulseSurveyStampsNoKickWithoutDynamicConsent(t *testing.T) {
	defer withRideMode(rideStatic)()
	root, _, _ := kickFixture(t)
	groomFixture(t, root, "aa-board")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
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
	if canvas := readPulseCanvas(t, root, "moe", closed[0]); strings.Contains(canvas, "## Kick") {
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
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
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
	if canvas := readPulseCanvas(t, root, "moe", open[0]); strings.Contains(canvas, "## Kick") {
		t.Errorf("canvas =\n%s\nwant the survey's report untouched after a failed stamp", canvas)
	}
	if !strings.Contains(errb.String(), "stamp kick section") {
		t.Errorf("stderr = %q, want the stamp failure named", errb.String())
	}
	if dirty, err := dirtyOutsidePaths(root); err != nil || dirty {
		t.Errorf("tree dirty after a failed stamp (dirty=%v, err=%v); the repo-wide gate would wedge every later close", dirty, err)
	}
}
