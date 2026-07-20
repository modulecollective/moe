package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// newTestPulseInterrupt builds a pulseInterrupt driven by an injected
// channel (never a real SIGINT), torn down on cleanup. Its latch starts
// clear; latchedPulseInterrupt returns one already tripped.
func newTestPulseInterrupt(t *testing.T) *pulseInterrupt {
	t.Helper()
	sigCh := make(chan os.Signal, 1)
	var once sync.Once
	pi := startPulseInterrupt(sigCh, func() { once.Do(func() { close(sigCh) }) })
	t.Cleanup(pi.Close)
	return pi
}

func latchedPulseInterrupt(t *testing.T) *pulseInterrupt {
	t.Helper()
	pi := newTestPulseInterrupt(t)
	pi.mark()
	return pi
}

// TestPulseInterruptLatchesThenReleasesWatcher pins the two-Ctrl-C shape:
// the first signal latches and then steps the watcher out of the way
// (signal.Stop) so a second Ctrl-C would get Go's default disposition.
func TestPulseInterruptLatchesThenReleasesWatcher(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	var stopped atomic.Bool
	var once sync.Once
	pi := startPulseInterrupt(sigCh, func() {
		once.Do(func() {
			stopped.Store(true)
			close(sigCh)
		})
	})
	if pi.interrupted() {
		t.Fatal("latched before any signal")
	}
	sigCh <- os.Interrupt
	<-pi.done // the watcher latches, stops, and exits when sigCh closes
	if !pi.interrupted() {
		t.Fatal("first Ctrl-C did not latch")
	}
	if !stopped.Load() {
		t.Fatal("watcher did not step out of the way after the first Ctrl-C")
	}
	pi.Close() // idempotent
}

// TestPulseSurveyLatchBeforeOpenMintsNothing: a Ctrl-C that lands before
// the run is minted skips with nothing to clean — no run opened, no
// agent turn.
func TestPulseSurveyLatchBeforeOpenMintsNothing(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	var calls int
	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		calls++
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, latchedPulseInterrupt(t), io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d, want 0; stderr=%q", code, errb.String())
	}
	if calls != 0 {
		t.Fatalf("openPulse calls=%d, want 0 — a pre-Open Ctrl-C mints nothing", calls)
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 0 {
		t.Fatalf("pulse runs %v opened despite a pre-Open skip", open)
	}
	if !strings.Contains(errb.String(), "no run opened") {
		t.Errorf("stderr=%q, want a 'no run opened' skip line", errb.String())
	}
}

// TestPulseSurveySkipDuringSetupDisposesRun: a Ctrl-C in the gap between
// minting the run and the agent executor trips the pre-executor belt
// (the prompt builder returns errPulseSkipped, so openPulse exits 1 ≠
// 130). The just-minted run is disposed via the registered close, not
// left dangling on the dash.
func TestPulseSurveySkipDuringSetupDisposesRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		// Model the belt: the Ctrl-C latched during setup and the
		// bootstrap-failure path returned 1.
		pi.mark()
		return 1
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, newTestPulseInterrupt(t), io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d, want 0; stderr=%q", code, errb.String())
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 0 {
		t.Fatalf("pulse runs %v left open; the skip should have disposed the run", open)
	}
	var closed int
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Workflow == pulseWorkflow && md.Status == run.StatusClosed {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("closed pulse runs=%d, want 1 (disposed via the registered close)", closed)
	}
	if !strings.Contains(errb.String(), "skipped — closed") {
		t.Errorf("stderr=%q, want a 'skipped — closed' disposal line", errb.String())
	}
}

// TestPulseSurveyMidAgentInterruptLeavesRunOpen: a Ctrl-C that reaches
// the running agent (openPulse == 130) leaves the run open — a partial
// sweep may hold real findings — but still propagates the interrupt out
// so a cascade halts. The stub does not touch the latch; the survey's
// own 130 branch must mark it.
func TestPulseSurveyMidAgentInterruptLeavesRunOpen(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		return exitInterrupted
	}
	t.Cleanup(func() { openPulse = orig })

	pi := newTestPulseInterrupt(t)
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, pi, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0 (interrupt not a verb failure)", code)
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 1 {
		t.Fatalf("open pulse runs=%v, want exactly one left open by the mid-agent interrupt", open)
	}
	if !pi.interrupted() {
		t.Fatal("the 130 branch did not propagate the interrupt (latch not marked)")
	}
}

// TestPulseNewExitsInterruptedOnSkip: `moe pulse new` is the one verb the
// pulse *is*, so a skipped sweep is the verb's own outcome — exit 130.
func TestPulseNewExitsInterruptedOnSkip(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	orig := runPulseSurvey
	runPulseSurvey = func(root, projectID, spawner string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		pi.mark() // the operator Ctrl-C'd the sweep
		return 0
	}
	t.Cleanup(func() { runPulseSurvey = orig })

	var out, errb bytes.Buffer
	if code := Run([]string{"pulse", "new", "moe"}, &out, &errb); code != exitInterrupted {
		t.Fatalf("`moe pulse new` skip exit=%d, want exitInterrupted (%d); stderr=%q", code, exitInterrupted, errb.String())
	}
}

// TestBareCloseExitsZeroOnPulseSkip: the bare `moe sdlc close` verb's own
// durable work (close committed + pushed) succeeded, so a Ctrl-C'd tail
// pulse is a successful skip — exit 0, not 130.
func TestBareCloseExitsZeroOnPulseSkip(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	if err := os.MkdirAll(sandbox.Path(root, "tele", "ship-it"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := firePulse
	firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) bool { return true }
	t.Cleanup(func() { firePulse = orig })

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("bare close exit=%d, want 0 (a skipped tail pulse is not a close failure); stderr=%q", code, errb.String())
	}
}

// writePulseGate stands in for the survey agent turn's canvas write: it
// overwrites the run's pulse canvas with a `## Gate` section carrying the
// given JSON payload and commits it (the real turn commits its canvas, so
// the working tree is clean when a downstream reflect mint runs). Leaving
// the seeded skeleton untouched instead models an unfilled gate.
func writePulseGate(t *testing.T, root, projectID, runID, gateJSON string) {
	t.Helper()
	rel := run.ContentPath(projectID, runID, pulseDoc)
	body := "# Pulse\n\n## Gate\n\n```json\n" + gateJSON + "\n```\n"
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", rel)
	gittest.Run(t, root, "commit", "-m", "work: pulse survey "+runID)
}

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
// (the lane bar) so the assertion flags a fragment rename or a dropped
// idiom.
func TestBuildSystemPromptInjectsPulseFragment(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{
		ID:       "pulse-2026-07-17",
		Project:  "moe",
		Workflow: pulseWorkflow,
	}
	got, err := buildSystemPrompt(root, md, pulseDoc, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Stage: pulse") {
		t.Fatalf("prompt missing pulse fragment heading:\n%s", got)
	}
	if !strings.Contains(got, "The lane bar") {
		t.Fatalf("pulse.md missing the lane bar it owns:\n%s", got)
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
// of a test, returning the accumulator. Each entry is "<project> <spawner>"
// so a test can assert both that the pulse fired and which run it threads
// as the spawner (a trailing space when no spawner is passed).
func stubFirePulse(t *testing.T) *[]string {
	t.Helper()
	var fired []string
	orig := firePulse
	firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) bool {
		fired = append(fired, projectID+" "+spawner)
		return false
	}
	t.Cleanup(func() { firePulse = orig })
	return &fired
}

// openPulseRuns returns the ids of a project's in-progress pulse runs.
// The single-flight guard (and its findInProgressPulseRun helper) is
// gone, so tests scan directly to observe how many sweeps are piled up.
func openPulseRuns(t *testing.T, root, projectID string) []string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var open []string
	for _, md := range mds {
		if md.Project == projectID && md.Workflow == pulseWorkflow && md.Status == run.StatusInProgress {
			open = append(open, md.ID)
		}
	}
	return open
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
	if len(*fired) != 1 || (*fired)[0] != "tele ship-it" {
		t.Fatalf("firePulse fired %v, want one fire for tele spawned by ship-it", *fired)
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

// TestPulseSurveyAllowsConcurrentRuns: the single-flight guard is gone.
// With a pulse run already open, a fresh survey still opens a second run
// with a distinct slug — it neither refuses nor skips. The stubbed sweep
// exits clean, so the new run auto-closes while the pre-existing one
// stays open beside it.
func TestPulseSurveyAllowsConcurrentRuns(t *testing.T) {
	root := seedCloseFixture(t, "moe", "pulse-open", pulseWorkflow, run.StatusInProgress)

	orig := openPulse
	var calls int
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		calls++
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d, want 0 (no single-flight refusal); stderr=%q", code, errb.String())
	}
	if calls != 1 {
		t.Fatalf("openPulse calls=%d, want 1 — the open pulse-open run must not gate the sweep", calls)
	}

	// Two distinct pulse runs now exist: the pre-seeded pulse-open (still
	// open) and the freshly opened one (auto-closed on the clean exit).
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == pulseWorkflow {
			ids[md.ID] = md.Status
		}
	}
	if len(ids) != 2 {
		t.Fatalf("pulse runs = %v, want two distinct runs (pulse-open plus a fresh sweep)", ids)
	}
	if _, ok := ids["pulse-open"]; !ok {
		t.Fatalf("pre-seeded pulse-open run missing from %v", ids)
	}
}

// TestPulseSurveyAutoClosesOnSuccess: a clean (exit 0) survey auto-closes
// its own run so no pulse run is left lingering on the dash. The stubbed
// agent turn files one followup, so the assertion also pins that the
// skipEdit auto-close harvests filings into ideas (review moves from a
// $EDITOR prune at close to scrapping on the dash).
func TestPulseSurveyAutoClosesOnSuccess(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	var calls int
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		calls++
		writePulseGate(t, root, projectID, runID, `{"status": "ok"}`)
		fp := filepath.Join(root, run.FollowupsPath(projectID, runID))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte("- [ ] `tidy-pulse` — Tidy the pulse survey\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d stderr=%q", code, errb.String())
	}
	if calls != 1 {
		t.Fatalf("openPulse calls=%d, want 1", calls)
	}

	// No pulse run left open — the auto-close fired.
	if open := openPulseRuns(t, root, "moe"); len(open) != 0 {
		t.Fatalf("pulse runs %v still open after a clean survey; auto-close did not fire", open)
	}

	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var pulses, ideas int
	for _, md := range mds {
		switch md.Workflow {
		case pulseWorkflow:
			pulses++
			if md.Status != run.StatusClosed {
				t.Fatalf("pulse run %s status=%q, want closed", md.ID, md.Status)
			}
		case dash.IdeaWorkflow:
			if md.Project == "moe" {
				ideas++
			}
		}
	}
	if pulses != 1 {
		t.Fatalf("want exactly one pulse run, got %d", pulses)
	}
	if ideas != 1 {
		t.Fatalf("want the filed followup harvested into one idea, got %d ideas", ideas)
	}
}

// TestPulseSurveyRecordsSpawner: a survey fired with a spawner slug
// threads it onto the pulse run's MoE-Spawned-By edge — both the run.json
// metadata field and the greppable journal index, the two the dash reads
// to nest the pulse under its parent.
func TestPulseSurveyRecordsSpawner(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	if code := runPulseSurvey(root, "moe", "ship-it" /*spawner*/, nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0", code)
	}

	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var pulseID string
	for _, md := range mds {
		if md.Workflow == pulseWorkflow && md.Project == "moe" {
			pulseID = md.ID
			if md.SpawnedBy != "moe/ship-it" {
				t.Fatalf("pulse run SpawnedBy = %q, want moe/ship-it (qualified spawner)", md.SpawnedBy)
			}
		}
	}
	if pulseID == "" {
		t.Fatal("no pulse run opened")
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.SpawnedBy["moe/"+pulseID]; got != "moe/ship-it" {
		t.Fatalf("index SpawnedBy[moe/%s] = %q, want moe/ship-it (MoE-Spawned-By trailer missing?)", pulseID, got)
	}
}

// TestPulseSurveyFailureLeavesRunOpenButDoesNotBlock: a non-zero survey
// (agent failure or SIGINT) is not propagated and does not auto-close —
// the run stays open on the dash for a human to look at. Escalation is
// now by visibility, not by blocking: the next auto-fire still runs a
// fresh survey, so a persistently broken sweep piles up open runs rather
// than silently starving the pulse.
func TestPulseSurveyFailureLeavesRunOpenButDoesNotBlock(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	var calls int
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		calls++
		return 1
	}
	t.Cleanup(func() { openPulse = orig })

	// Failure is not a verb failure…
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0 (failure not propagated)", code)
	}
	// …but the run stays open for a manual look.
	if open := openPulseRuns(t, root, "moe"); len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want exactly one left open by the failed sweep", open)
	}

	// No single-flight: the next auto-fire still reaches the agent turn
	// and opens a second run, so the broken sweeps pile up (visible on the
	// dash) instead of blocking.
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("second survey exit=%d, want 0 (failure not propagated)", code)
	}
	if calls != 2 {
		t.Fatalf("openPulse calls=%d, want 2 — the second fire must run a fresh survey", calls)
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 2 {
		t.Fatalf("open pulse runs = %v, want two piled up after two failed sweeps", open)
	}
}

// twinRuns returns id→status for a project's twin-workflow runs — the
// reflect runs the pulse gate can auto-spawn.
func twinRuns(t *testing.T, root, projectID string) map[string]string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, md := range mds {
		if md.Project == projectID && md.Workflow == "twin" {
			out[md.ID] = md.Status
		}
	}
	return out
}

// TestPulseSurveyUnfilledGateLeavesRunOpen: a survey that exits 0 but
// leaves the gate on its unparsable skeleton placeholder (a no-op turn,
// or a crash after writing nothing) must NOT auto-close. The run lingers
// on the dash's ACTIVE list — escalation by visibility — and no reflect
// is spawned off a sweep that never concluded.
func TestPulseSurveyUnfilledGateLeavesRunOpen(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	// Stub writes nothing — the run opens with the skeleton's unparsable
	// `## Gate` placeholder, which is exactly the no-op shape.
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, &errb); code != 0 {
		t.Fatalf("survey exit=%d, want 0; stderr=%q", code, errb.String())
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want one left open by the unfilled gate", open)
	}
	if tw := twinRuns(t, root, "moe"); len(tw) != 0 {
		t.Fatalf("twin runs = %v, want none minted off an unfilled gate", tw)
	}
	if !strings.Contains(errb.String(), "unfilled gate") {
		t.Errorf("stderr = %q, want an unfilled-gate warning", errb.String())
	}
}

// TestPulseSurveyTwinSpawnMintsReflect: a spawn entry asking for
// workflow "twin" mints a parked twin reflect run stamped with the pulse
// as its spawner, and the pulse still auto-closes — opening rides the
// pulse, execution stays a human pull.
func TestPulseSurveyTwinSpawnMintsReflect(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		writePulseGate(t, root, projectID, runID,
			`{"status": "ok", "spawn": [{"slug": "reflect", "workflow": "twin", "why": "boundary move the twin docs miss"}]}`)
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0", code)
	}

	// The pulse auto-closed…
	if open := openPulseRuns(t, root, "moe"); len(open) != 0 {
		t.Fatalf("open pulse runs = %v, want none — a due verdict must not block auto-close", open)
	}
	// …and a single parked reflect run was minted.
	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want exactly one auto-spawned reflect", tw)
	}
	var reflectID string
	for id, status := range tw {
		reflectID = id
		if status != run.StatusInProgress {
			t.Errorf("reflect run %s status=%q, want in_progress (parked)", id, status)
		}
	}
	// Find the pulse run's slug so we can assert the spawn edge points at it.
	var pulseID string
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == pulseWorkflow {
			pulseID = md.ID
		}
	}
	if pulseID == "" {
		t.Fatal("no pulse run found")
	}
	var reflectMD *run.Metadata
	for i := range mds {
		if mds[i].ID == reflectID {
			reflectMD = mds[i]
		}
	}
	wantSpawner := "moe/" + pulseID
	if reflectMD.SpawnedBy != wantSpawner {
		t.Fatalf("reflect SpawnedBy = %q, want the qualified pulse slug %q", reflectMD.SpawnedBy, wantSpawner)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.SpawnedBy["moe/"+reflectID]; got != wantSpawner {
		t.Fatalf("index SpawnedBy[moe/%s] = %q, want %q (MoE-Spawned-By trailer missing?)", reflectID, got, wantSpawner)
	}
}

// TestPulseSurveyTwinSpawnSkipsWhenTwinInProgress: a twin spawn entry is
// a silent no-op when a twin pass is already open (a parked prior reflect
// counts — parked runs are in_progress). No second reflect is minted, and
// the pulse still auto-closes.
func TestPulseSurveyTwinSpawnSkipsWhenTwinInProgress(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")
	// A reflect already parked for this project.
	writeRunMeta(t, root, "moe", "reflect-2026-05-14", "twin")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
		writePulseGate(t, root, projectID, runID,
			`{"status": "ok", "spawn": [{"slug": "reflect", "workflow": "twin", "why": "drift piled up"}]}`)
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	if code := runPulseSurvey(root, "moe", "" /*spawner*/, nil /*pi*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0", code)
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 0 {
		t.Fatalf("open pulse runs = %v, want none — the skip must not block auto-close", open)
	}
	tw := twinRuns(t, root, "moe")
	if len(tw) != 1 {
		t.Fatalf("twin runs = %v, want the single pre-existing reflect (no second mint)", tw)
	}
	if _, ok := tw["reflect-2026-05-14"]; !ok {
		t.Fatalf("twin runs = %v, want the pre-seeded reflect-2026-05-14 untouched", tw)
	}
}

// TestTaggedFollowupHarvestPromoteGroomAndKick is the composition
// regression for the gap this feature closes. A real harvested followup
// becomes a tagged idea; a real headless pulse turn (fake claude on PATH)
// names that live idea in spawn + chain; the harness promotes it, grooms
// it, and reaches the self-kick door. The fake refuses the destination's
// design turn so the test stops after proving the kick was attempted —
// stage execution itself is covered by the cascade tests.
func TestTaggedFollowupHarvestPromoteGroomAndKick(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("MOE_AGENT", "claude")
	t.Setenv("NO_COLOR", "1")

	source, err := run.New(root, "moe", run.Options{ID: "source-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	writeFollowups(t, root, "moe", source.ID, strings.Join([]string{
		"- [ ] `tagged-fix` (sdlc) — Apply the mechanical fix",
		"",
		"  The idea canvas is the only promotion seed.",
		"",
	}, "\n"))
	if err := harvestFollowups(root, "moe", source.ID, "sdlc", true); err != nil {
		t.Fatalf("harvestFollowups: %v", err)
	}
	if err := run.StageAndCommit(root, "test: land harvested audit line", run.FollowupsPath("moe", source.ID)); err != nil {
		t.Fatalf("commit harvested followup: %v", err)
	}

	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
ticks=$(printf '\140\140\140')
case "$canvas" in
  */documents/pulse/content.md)
    printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","reflect":{"due":false},"spawn":[{"slug":"tagged-fix","title":"Apply the mechanical fix","why":"captured followup clears the bar"}],"chain":[{"runs":["tagged-fix"],"kick":true}]}' "$ticks" > "$canvas"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`)

	var out, errb bytes.Buffer
	defer withRideMode(rideDynamic)()
	if code := runPulseSurvey(root, "moe", "" /*unchained spawner*/, nil, &out, &errb); code != 0 {
		t.Fatalf("pulse exit=%d stderr=%q", code, errb.String())
	}

	idea, err := run.Load(root, "moe", "tagged-fix")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusPromoted {
		t.Fatalf("idea status=%q, want promoted", idea.Status)
	}
	var promoted *run.Metadata
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == "sdlc" && md.ID != source.ID {
			promoted = md
		}
	}
	if promoted == nil || promoted.SpawnedBy == "" {
		t.Fatalf("promoted run = %+v, want machine lineage", promoted)
	}
	if !strings.Contains(errb.String(), "pulse: kicking moe/"+promoted.ID+" (dynamic)") {
		t.Fatalf("pulse never reached self-kick for promoted run; stderr=%q", errb.String())
	}
}
