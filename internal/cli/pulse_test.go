package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
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
// of a test, returning the accumulator. Each entry is "<project> <spawner>"
// so a test can assert both that the pulse fired and which run it threads
// as the spawner (a trailing space when no spawner is passed).
func stubFirePulse(t *testing.T) *[]string {
	t.Helper()
	var fired []string
	orig := firePulse
	firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) {
		fired = append(fired, projectID+" "+spawner)
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
	openPulse = func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
		calls++
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, io.Discard, &errb); code != 0 {
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
	openPulse = func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
		calls++
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
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, io.Discard, &errb); code != 0 {
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
	openPulse = func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
		return 0
	}
	t.Cleanup(func() { openPulse = orig })

	if code := runPulseSurvey(root, "moe", "ship-it" /*spawner*/, io.Discard, io.Discard); code != 0 {
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
	openPulse = func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
		calls++
		return 1
	}
	t.Cleanup(func() { openPulse = orig })

	// Failure is not a verb failure…
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("survey exit=%d, want 0 (failure not propagated)", code)
	}
	// …but the run stays open for a manual look.
	if open := openPulseRuns(t, root, "moe"); len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want exactly one left open by the failed sweep", open)
	}

	// No single-flight: the next auto-fire still reaches the agent turn
	// and opens a second run, so the broken sweeps pile up (visible on the
	// dash) instead of blocking.
	if code := runPulseSurvey(root, "moe", "" /*spawner*/, io.Discard, io.Discard); code != 0 {
		t.Fatalf("second survey exit=%d, want 0 (failure not propagated)", code)
	}
	if calls != 2 {
		t.Fatalf("openPulse calls=%d, want 2 — the second fire must run a fresh survey", calls)
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 2 {
		t.Fatalf("open pulse runs = %v, want two piled up after two failed sweeps", open)
	}
}
