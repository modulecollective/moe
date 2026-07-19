package cli

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func writeContent(t *testing.T, root, projectID, runID, docID, body string) {
	t.Helper()
	path := filepath.Join(root, run.ContentPath(projectID, runID, docID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// markBureaucracy writes the root-marker file so bureaucracy.Find picks
// up the test repo. newTestBureaucracy just initializes a git repo; the
// marker lives on top of it.
func markBureaucracy(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDashEmptyBureaucracy(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"MINISTRY OF EVERYTHING",
		"ACTIVE (0)",
		"BACKLOG (0)",
		"COMPLETED (0)",
		"0 project(s) registered · 0 with active runs",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestDashAfterCodeShowsCodeStage: design + code turns are in, no
// push turn yet. Under the forward-walking rule the run is parked at
// code (push has no successor turn after code's), so dash renders
// "sdlc:code". The operator either re-runs code, which fires the push
// chain prompt again, or types `moe sdlc push` directly to ship.
func TestDashAfterCodeShowsCodeStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")
	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active run row, got:\n%s", got)
	}
	if !strings.Contains(got, "fix-it") || !strings.Contains(got, "tele") {
		t.Fatalf("row missing project/run:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "sdlc:code") {
		t.Fatalf("expected run row with stage 'sdlc:code' (parked), got:\n%s", got)
	}
}

// TestDashPrereqReworkedShowsDesignStage: design is re-signed after
// the code turn. Under the forward-walking rule, design's latest turn
// is now newer than code's so design is parked. dash should show
// "design"; the previous backward-walking rule would have shown "code"
// (code stale because prereq newer).
func TestDashPrereqReworkedShowsDesignStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")

	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0.Add(2*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active run row, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "sdlc:design") {
		t.Fatalf("expected run row with stage 'sdlc:design', got:\n%s", got)
	}
}

// TestDashFreshRunShowsFirstStage: a seeded run with no work turns at
// all still shows up, with "design" as its next stage.
func TestDashFreshRunShowsFirstStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active run row, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "sdlc:design") {
		t.Fatalf("expected run row with stage 'sdlc:design', got:\n%s", got)
	}
}

// TestDashPushedRunShowsAwaitingMerge: a run with StatusPushed renders
// in ACTIVE with an "awaiting merge: #<n>" label so the operator
// sees it still owes a click on GitHub. PR number comes from the
// MoE-PR trailer.
func TestDashPushedRunShowsAwaitingMerge(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusPushed)
	trailerstest.CommitTrailer(t, root, "push: fix-it",
		"MoE-Run: fix-it\nMoE-Project: tele\nMoE-PR: https://example.com/pr/42",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected pushed run in ACTIVE, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "#42") {
		t.Fatalf("expected run row with PR number '#42', got:\n%s", got)
	}
	if !strings.Contains(got, "sdlc:awaiting merge: #42") {
		t.Fatalf("expected 'sdlc:awaiting merge: #42' label, got:\n%s", got)
	}
}

// TestDashMergedRunShowsMerged: a run with StatusMerged renders as
// "merged" in COMPLETED.
func TestDashMergedRunShowsMerged(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "push: fix-it merged",
		"MoE-Run: fix-it\nMoE-Project: tele\nMoE-Merged: abc1234567890",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (1)") {
		t.Fatalf("expected merged run in COMPLETED, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "sdlc:merged") {
		t.Fatalf("expected run row with stage 'sdlc:merged', got:\n%s", got)
	}
}

// TestDashClosedRunShowsClosed: a closed sdlc run that hasn't yet been
// reopened renders with a "· reopen?" marker so the operator sees it
// as a candidate to carry forward via `moe sdlc reopen`. The marker
// is gated on workflow ("sdlc") because reopen is an sdlc verb.
func TestDashClosedRunShowsClosed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusClosed)
	trailerstest.CommitTrailer(t, root, "push: fix-it closed",
		"MoE-Run: fix-it\nMoE-Project: tele\nMoE-Closed: https://example.com/pr/42",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (1)") {
		t.Fatalf("expected closed run in COMPLETED, got:\n%s", got)
	}
	if !strings.Contains(got, "sdlc:closed · reopen?") {
		t.Fatalf("expected closed sdlc run to carry the reopen marker, got:\n%s", got)
	}
}

// TestDashKBRunAfterSummarizeShowsDoneNeedsClose: a KB run that's
// walked the full ladder (research + summarize) has Next()==Done but
// Status==InProgress (KB has no push). The run still needs an operator
// action (`moe kb close`) to land in COMPLETED, so dash surfaces it in
// ACTIVE with a `· close?` action hint — same shape as the `· reopen?`
// hint on closed sdlc runs.
func TestDashKBRunAfterSummarizeShowsDoneNeedsClose(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)
	t0 := time.Now().UTC().Add(-2 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "lookup", "kb", "research", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "lookup", "kb", "summarize", t0.Add(time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected done KB run to surface in ACTIVE awaiting close, got:\n%s", got)
	}
	if !strings.Contains(got, "kb:done · close?") {
		t.Fatalf("expected 'kb:done · close?' action hint, got:\n%s", got)
	}
}

// TestDashKBRunAfterResearchShowsResearchParked is the mirror-image
// check: research is written but summarize isn't yet. Under the
// forward-walking rule research has no successor turn after it, so
// the run is parked at research and dash renders `kb:research` under
// ACTIVE — same intuition as the sdlc parked-at-stage cases above.
func TestDashKBRunAfterResearchShowsResearchParked(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)
	t0 := time.Now().UTC().Add(-2 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "lookup", "kb", "research", t0)

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !containsRunRow(got, "tele", "lookup", "kb:research") {
		t.Fatalf("expected KB run row with stage 'kb:research' (parked), got:\n%s", got)
	}
}

// TestDashStaleInProgressRunSurfacesInActive: a 60-day-old in-progress
// run shows up in ACTIVE *without* --all, carrying its real age. There
// is no longer a dormancy filter — stale in-progress work is surfaced
// for grooming, not hidden. (Inverts the former
// TestDashDormantHiddenWithoutAll, which asserted the opposite.)
func TestDashStaleInProgressRunSurfacesInActive(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "old-one", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update spec",
		"MoE-Run: old-one\nMoE-Project: tele\nMoE-Document: spec",
		time.Now().UTC().Add(-60*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("stale in-progress run should surface in ACTIVE, got:\n%s", got)
	}
	if !strings.Contains(got, "old-one") {
		t.Fatalf("expected the stale run row to be present, got:\n%s", got)
	}
	// The age is the grooming signal: 60 days reads as "60d ago".
	if !strings.Contains(got, "60d ago") {
		t.Fatalf("expected the stale run to show its real age '60d ago', got:\n%s", got)
	}
}

// TestDashStaleMergedRunCountsInCompleted is the regression guard for
// "what happened to 750?": a run merged 60 days ago must still count in
// COMPLETED without --all. The old dormancy gate subtracted such runs
// from the completed total once they aged past 30 days, so the header
// drifted down day by day. The completed count is cumulative history —
// it should only climb.
func TestDashStaleMergedRunCountsInCompleted(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "long-merged", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "push: long-merged merged",
		"MoE-Run: long-merged\nMoE-Project: tele\nMoE-Merged: abc1234567890",
		time.Now().UTC().Add(-60*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (1)") {
		t.Fatalf("stale merged run should still count in COMPLETED, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "long-merged", "sdlc:merged") {
		t.Fatalf("expected the stale merged run row, got:\n%s", got)
	}
}

func TestDashSortsNewestFirstWithinBucket(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "older", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update spec",
		"MoE-Run: older\nMoE-Project: tele\nMoE-Document: spec",
		time.Now().UTC().Add(-3*24*time.Hour))

	trailerstest.SeedRun(t, root, "tele", "newer", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update spec",
		"MoE-Run: newer\nMoE-Project: tele\nMoE-Document: spec",
		time.Now().UTC().Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	newerIdx := strings.Index(got, "newer")
	olderIdx := strings.Index(got, "older")
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("missing one of the rows: newer=%d older=%d in:\n%s", newerIdx, olderIdx, got)
	}
	if newerIdx > olderIdx {
		t.Fatalf("expected newer before older:\n%s", got)
	}
}

func TestDashBacklogShowsCapturedIdeas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	// Capture two ideas via the CLI so the commit shape comes straight
	// from production code paths.
	for _, slug := range []string{"cross-project-search", "faster-dash-load"} {
		if code := Run([]string{"idea", "new", "tele/" + slug}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("setup capture failed for %q", slug)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "BACKLOG (2)") {
		t.Fatalf("expected BACKLOG (2), got:\n%s", got)
	}
	for _, want := range []string{"cross-project-search", "faster-dash-load"} {
		if !strings.Contains(got, want) {
			t.Fatalf("backlog missing %q in:\n%s", want, got)
		}
	}
	// Each backlog row carries an `idea:capture` note so the workflow
	// identity is visible even on the backlog rail. The idea's title
	// already appears in the run-slug column, so the note surfaces the
	// stage instead of repeating it.
	captureIdx := strings.Index(got, "idea:capture")
	if captureIdx < 0 {
		t.Fatalf("backlog missing `idea:capture` stage note in:\n%s", got)
	}
	if strings.Index(got[captureIdx+len("idea:capture"):], "idea:capture") < 0 {
		t.Fatalf("expected two `idea:capture` rows in backlog, got:\n%s", got)
	}
	// Sections render top-to-bottom: ACTIVE → BACKLOG → COMPLETED.
	activeIdx := strings.Index(got, "ACTIVE")
	backlogIdx := strings.Index(got, "BACKLOG")
	completedIdx := strings.Index(got, "COMPLETED")
	if !(activeIdx >= 0 && backlogIdx >= 0 && completedIdx >= 0 && activeIdx < backlogIdx && backlogIdx < completedIdx) {
		t.Fatalf("section order wrong (active=%d backlog=%d completed=%d):\n%s", activeIdx, backlogIdx, completedIdx, got)
	}
}

func TestDashBacklogEmptyShowsNone(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "BACKLOG (0)") {
		t.Fatalf("expected empty BACKLOG section, got:\n%s", out.String())
	}
}

func TestDashProjectCountReflectsProjectJSON(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for _, p := range []string{"alpha", "beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(root, "projects", p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(root, "projects", p, "project.json"),
			[]byte(`{"id":"`+p+`"}`),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "3 project(s) registered") {
		t.Fatalf("expected 3 projects in footer, got:\n%s", out.String())
	}
}

// TestDashFooterActiveCountsProjectsNotRuns pins the footer semantic:
// "N with active runs" counts *projects* with at least one active run,
// not active rows. Two active runs in one project ⇒ 1, not 2. The
// dashboard already shows the row count on the ACTIVE header, so the
// footer carries a fact you can't read off the section headers.
func TestDashFooterActiveCountsProjectsNotRuns(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-a", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "fix-b", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (2)") {
		t.Fatalf("expected two active rows, got:\n%s", got)
	}
	if !strings.Contains(got, "1 project(s) registered · 1 with active runs") {
		t.Fatalf("expected footer to count the single project once, got:\n%s", got)
	}
}

// TestDashCompletedCapsAtCap seeds more completed runs than the
// dashboard cap and asserts (a) the header shows "N of total" and
// (b) only the newest CompletedCap rows render. The cap exists so the
// section doesn't grow unbounded and drown the live sections above it.
func TestDashCompletedCapsAtCap(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// 12 merged runs, oldest first so "newest-first" ordering pushes
	// the newer slugs to the top of the section.
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		trailerstest.SeedRun(t, root, "tele", slug, "sdlc", run.StatusMerged)
		trailerstest.CommitTrailer(t, root, "push: "+slug+" merged",
			"MoE-Run: "+slug+"\nMoE-Project: tele\nMoE-Merged: deadbeef"+slug,
			time.Now().UTC().Add(-time.Duration(12-i)*time.Hour))
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if want := fmt.Sprintf("COMPLETED (%d of 12)", dash.CompletedCap); !strings.Contains(got, want) {
		t.Fatalf("expected capped header %q, got:\n%s", want, got)
	}
	// The newest CompletedCap render; everything older is truncated.
	if !containsRunRow(got, "tele", "done-11", "sdlc:merged") {
		t.Fatalf("expected newest completed run to render, got:\n%s", got)
	}
	for i := 0; i < 12-dash.CompletedCap; i++ {
		dropped := fmt.Sprintf("done-%02d", i)
		if strings.Contains(got, dropped) {
			t.Fatalf("expected %q to be truncated below cap, got:\n%s", dropped, got)
		}
	}
}

// TestDashSectionHeadingsDropRuns pins the operator-facing section
// labels: "ACTIVE" and "COMPLETED" — the bare nouns, without the
// implementation-flavored "RUNS" suffix that used to read like a
// schema label rather than a status.
func TestDashSectionHeadingsDropRuns(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"ACTIVE (0)", "BACKLOG (0)", "COMPLETED (0)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
	for _, banned := range []string{"ACTIVE RUNS", "COMPLETED RUNS"} {
		if strings.Contains(got, banned) {
			t.Fatalf("unexpected %q in output:\n%s", banned, got)
		}
	}
}

// TestDashPromotedIdeaShowsSuccessorSlug: a promoted idea row gains a
// " → <slug>" suffix naming the run it was promoted to, sourced from
// the MoE-Promoted-To trailer. The slug points the operator at the
// destination run directly; the workflow is already visible once that
// run shows up in ACTIVE/COMPLETED.
func TestDashPromotedIdeaShowsSuccessorSlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "search-idea", dash.IdeaWorkflow, run.StatusPromoted)
	trailerstest.SeedRun(t, root, "tele", "search-impl", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "Promote idea tele/search-idea → tele/search-impl",
		"MoE-Run: search-idea\nMoE-Project: tele\nMoE-Workflow: idea\nMoE-Promoted-To: tele/search-impl",
		time.Time{})

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "idea:promoted → tele/search-impl") {
		t.Fatalf("expected 'idea:promoted → tele/search-impl' on the promoted row, got:\n%s", got)
	}
}

// TestDashPromotedIdeaMissingTargetFallsBack: when the successor run
// recorded on the trailer isn't present in the scanned set (deleted,
// not yet pulled, etc.), the row falls back to the bare
// "idea:promoted" label — the arrow only appears when we can name the
// destination run.
func TestDashPromotedIdeaMissingTargetFallsBack(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "ghost-idea", dash.IdeaWorkflow, run.StatusPromoted)
	trailerstest.CommitTrailer(t, root, "Promote idea tele/ghost-idea → tele/never-seeded",
		"MoE-Run: ghost-idea\nMoE-Project: tele\nMoE-Workflow: idea\nMoE-Promoted-To: tele/never-seeded",
		time.Time{})

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "idea:promoted") {
		t.Fatalf("expected promoted row, got:\n%s", got)
	}
	if strings.Contains(got, "→") {
		t.Fatalf("expected no arrow when destination is missing, got:\n%s", got)
	}
}

// TestDashPromotedIdeaNestsSettledSuccessor: once the successor run has
// merged, the promotion edge stops being an inline arrow and becomes a
// ladder — the idea row on top, the run nested beneath it as a "↳" row,
// the same grammar chain and spawn lineage already use.
func TestDashPromotedIdeaNestsSettledSuccessor(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "search-idea", dash.IdeaWorkflow, run.StatusPromoted)
	trailerstest.SeedRun(t, root, "tele", "search-impl", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "Promote idea tele/search-idea → tele/search-impl",
		"MoE-Run: search-idea\nMoE-Project: tele\nMoE-Workflow: idea\nMoE-Promoted-To: tele/search-impl",
		time.Now().UTC().Add(-21*24*time.Hour))
	trailerstest.CommitTrailer(t, root, "push: search-impl merged",
		"MoE-Run: search-impl\nMoE-Project: tele\nMoE-Merged: deadbeef",
		time.Now().UTC().Add(-2*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "↳ tele/search-impl") {
		t.Fatalf("expected the merged successor nested under its idea, got:\n%s", got)
	}
	if strings.Contains(got, "idea:promoted → ") {
		t.Fatalf("expected the inline arrow to drop once the pair folds, got:\n%s", got)
	}
	// Idea on top: the ancestor above, the continuation beneath.
	ideaAt := strings.Index(got, "tele/search-idea")
	implAt := strings.Index(got, "↳ tele/search-impl")
	if ideaAt < 0 || implAt < 0 || ideaAt > implAt {
		t.Fatalf("expected the idea row above its nested successor, got:\n%s", got)
	}
}

// TestDashFilterByProject: with two registered projects each holding an
// active run, `--project foo` narrows the dashboard to foo's row only.
// Empty-string default = no filter, so this also pins that the flag
// only kicks in when set.
func TestDashFilterByProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "bar", "beta", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--project", "foo"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active row after --project foo, got:\n%s", got)
	}
	if !containsRunRow(got, "foo", "alpha", "sdlc:design") {
		t.Fatalf("expected foo/alpha row, got:\n%s", got)
	}
	if strings.Contains(got, "beta") {
		t.Fatalf("did not expect bar/beta in --project foo view:\n%s", got)
	}
}

// TestDashFilterByWorkflow: two runs in the same project on different
// workflows; `--workflow kb` keeps the kb row and drops the sdlc row.
func TestDashFilterByWorkflow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--workflow", "kb"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active row after --workflow kb, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "lookup", "kb:research") {
		t.Fatalf("expected tele/lookup row, got:\n%s", got)
	}
	if strings.Contains(got, "fix-it") {
		t.Fatalf("did not expect sdlc/fix-it in --workflow kb view:\n%s", got)
	}
}

// TestDashFilterCombined: --project and --workflow compose to the
// intersection. Only the row matching both flags survives.
func TestDashFilterCombined(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "foo", "lookup", "kb", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "bar", "lookup", "kb", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--project", "foo", "--workflow", "kb"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one row at the intersection, got:\n%s", got)
	}
	if !containsRunRow(got, "foo", "lookup", "kb:research") {
		t.Fatalf("expected foo/lookup (kb) row, got:\n%s", got)
	}
	if strings.Contains(got, "alpha") {
		t.Fatalf("did not expect foo/alpha (sdlc) in combined view:\n%s", got)
	}
	if strings.Contains(got, "bar") {
		t.Fatalf("did not expect bar/lookup in combined view:\n%s", got)
	}
}

// TestDashFilterUnknownReturnsEmpty: an unknown filter value isn't an
// error — the dashboard renders with empty section bodies and exit 0.
// Filtering is read-only; "(none)" is the obvious miss signal.
func TestDashFilterUnknownReturnsEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--project", "bogus"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"ACTIVE (0)", "BACKLOG (0)", "COMPLETED (0)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected empty %q after unknown project, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fix-it") {
		t.Fatalf("did not expect any rows for unknown project:\n%s", got)
	}
}

// TestDashAllLiftsCompletedCap: with more completed runs than the cap,
// the default view truncates to completedCap (header reads "X of Y");
// `--all` lifts the truncation and renders the full list with a plain
// "COMPLETED (Y)" header.
func TestDashAllLiftsCompletedCap(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		trailerstest.SeedRun(t, root, "tele", slug, "sdlc", run.StatusMerged)
		trailerstest.CommitTrailer(t, root, "push: "+slug+" merged",
			"MoE-Run: "+slug+"\nMoE-Project: tele\nMoE-Merged: deadbeef"+slug,
			time.Now().UTC().Add(-time.Duration(12-i)*time.Hour))
	}

	// Default: capped at CompletedCap.
	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if want := fmt.Sprintf("COMPLETED (%d of 12)", dash.CompletedCap); !strings.Contains(out.String(), want) {
		t.Fatalf("expected capped header %q by default, got:\n%s", want, out.String())
	}

	// --all: every completed row renders, plain header.
	out.Reset()
	errb.Reset()
	code = Run([]string{"dash", "--all"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q --all stderr=%q", code, errb.String(), errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (12)") {
		t.Fatalf("expected uncapped header under --all, got:\n%s", got)
	}
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		if !strings.Contains(got, slug) {
			t.Fatalf("expected %q to render under --all, got:\n%s", slug, got)
		}
	}
}

// TestDashOpenSessionSameDocMarksRunning: a stage session whose doc
// matches the run's parked stage gets a terse "[running]" suffix —
// the parked-stage prefix already names the doc, so a "[code running]"
// repeat would buy nothing.
func TestDashOpenSessionSameDocMarksRunning(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
	// design + code signed: parked at code under the forward-walking rule.
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sdlc:code [running]") {
		t.Fatalf("expected 'sdlc:code [running]' on the active row, got:\n%s", out.String())
	}
}

// TestDashOpenSessionDifferentDocMarksDocRunning: a code session open
// on a run still parked at design — the case that motivates this rail
// — renders "sdlc:design [code running]". Without the marker the dash
// would say "design" while the operator knows code is in flight off
// the dashboard.
func TestDashOpenSessionDifferentDocMarksDocRunning(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Fresh run with no work turns: parked at design.
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sdlc:design [code running]") {
		t.Fatalf("expected 'sdlc:design [code running]' on the active row, got:\n%s", out.String())
	}
}

// TestDashNoOpenSessionLeavesNoteUnchanged: with no session worktrees
// open, the active-run note carries no "[running]" suffix — the dash's
// behaviour before this rail. Pins the no-marker default so a future
// change can't smuggle a marker into the no-session case.
func TestDashNoOpenSessionLeavesNoteUnchanged(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "running]") {
		t.Fatalf("expected no '[running]' marker without an open session, got:\n%s", out.String())
	}
}

// TestBuildFactoryArtEmpty: no backlog, no active, no completed →
// single-line dotted field, no rail and no smoke. Pinned because the
// dash's first-day state hits this exact shape.
func TestBuildFactoryArtEmpty(t *testing.T) {
	state := dash.FactoryState{}
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for empty state, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "·") {
		t.Fatalf("expected dotted empty-state field, got %q", lines[0])
	}
	for _, banned := range []string{"▦", "▣", "[", "▶", "━"} {
		if strings.Contains(lines[0], banned) {
			t.Fatalf("expected no rail glyph %q in empty state, got %q", banned, lines[0])
		}
	}
}

// TestBuildFactoryArtPopulatedShape: a mixed state (backlog, active
// runs of mixed stages, completed) renders three lines (drift, base,
// rail) whose rail carries the expected zone glyphs in zone order.
func TestBuildFactoryArtPopulatedShape(t *testing.T) {
	state := dash.FactoryState{
		BacklogCount: 2,
		ActiveStages: []dash.ActiveStation{
			{Stage: "design"},
			{Stage: "code"},
			{Stage: "awaiting merge"},
		},
		CompletedCount: 3,
	}
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (drift + base + rail), got %d: %q", len(lines), lines)
	}
	rail := lines[2]
	// Zones must appear in order: input → stations → output. Use the
	// first occurrence of each zone-distinguishing glyph as a proxy.
	idxIn := strings.Index(rail, "▦")
	idxDesign := strings.Index(rail, "⚒")
	idxCode := strings.Index(rail, "⚙")
	idxShip := strings.Index(rail, "[▶]")
	idxOut := strings.Index(rail, "▣")
	if idxIn < 0 || idxDesign < 0 || idxCode < 0 || idxShip < 0 || idxOut < 0 {
		t.Fatalf("missing zone glyph (in=%d design=%d code=%d ship=%d out=%d) in:\n%q",
			idxIn, idxDesign, idxCode, idxShip, idxOut, rail)
	}
	if !(idxIn < idxDesign && idxDesign < idxCode && idxCode < idxShip && idxShip < idxOut) {
		t.Fatalf("zones not in order in rail:\n%q", rail)
	}
	// The feed arrow follows the input zone when backlog is non-empty.
	if !strings.Contains(rail, "▦ ▦ ▶") {
		t.Fatalf("expected '▦ ▦ ▶' feed arrow after spaced input glyphs, got rail:\n%q", rail)
	}
}

// TestBuildFactoryArtOverflow: counts past their caps render `+N` tags
// rather than widening the line beyond budget.
func TestBuildFactoryArtOverflow(t *testing.T) {
	state := dash.FactoryState{
		BacklogCount: dash.InputCap + 3,
		ActiveStages: []dash.ActiveStation{ // dash.StationCap=4 + 2 over
			{Stage: "design"},
			{Stage: "code"},
			{Stage: "design"},
			{Stage: "code"},
			{Stage: "code"},
			{Stage: "design"},
		},
		CompletedCount: dash.OutputCap + 7,
	}
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	rail := lines[2]
	for _, want := range []string{"▦ ▦ ▦ ▦ +3", "+2", "▣ ▣ ▣ ▣ ▣ +7"} {
		if !strings.Contains(rail, want) {
			t.Fatalf("expected overflow tag %q in rail:\n%q", want, rail)
		}
	}
	// Bracketed stations capped: exactly dash.StationCap "[" should appear
	// before the "+2" station overflow tag.
	stationsRegion := rail
	if i := strings.Index(rail, "+2"); i >= 0 {
		stationsRegion = rail[:i]
	}
	if got, want := strings.Count(stationsRegion, "["), dash.StationCap; got != want {
		t.Fatalf("expected exactly %d bracketed stations before overflow, got %d in:\n%q",
			want, got, rail)
	}
}

// TestBuildFactoryArtUnknownStageFallsBack: an unrecognised stage
// (e.g. a future workflow) renders with the generic boiler glyph,
// not nothing. Single source of truth for the "new workflow doesn't
// silently disappear" guarantee.
func TestBuildFactoryArtUnknownStageFallsBack(t *testing.T) {
	state := dash.FactoryState{ActiveStages: []dash.ActiveStation{{Stage: "unknown-stage"}}}
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	rail := lines[2]
	if !strings.Contains(rail, "[◉]") {
		t.Fatalf("expected fallback boiler glyph '[◉]', got rail:\n%q", rail)
	}
}

// TestBuildFactoryArtNoSmokeWithoutSession: smoke is the liveness
// signal — stations whose run has no open session never smoke,
// whatever their parked stage. Pins the no-session-no-smoke invariant
// across design / code / awaiting-merge so a future change can't
// quietly resurrect stage-shaped smoke decoration.
func TestBuildFactoryArtNoSmokeWithoutSession(t *testing.T) {
	state := dash.FactoryState{ActiveStages: []dash.ActiveStation{
		{Stage: "design"},
		{Stage: "code"},
		{Stage: "awaiting merge"},
	}}
	// Sweep seeds so we exercise the RNG; any seed that paints a
	// fleck above a parked station on either smoke row fails the test.
	for seed := int64(1); seed <= 16; seed++ {
		r := rand.New(rand.NewSource(seed))
		lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
		if len(lines) != 3 {
			t.Fatalf("seed %d: expected 3 lines, got %d", seed, len(lines))
		}
		if strings.TrimSpace(lines[0]) != "" {
			t.Fatalf("seed %d: expected blank drift row above parked-only stations, got %q", seed, lines[0])
		}
		if strings.TrimSpace(lines[1]) != "" {
			t.Fatalf("seed %d: expected blank base row above parked-only stations, got %q", seed, lines[1])
		}
	}
}

// TestBuildFactoryArtWidth: every line is padded to at least dash.ArtWidth
// runes so the art row stands alone above the section table. Lines
// can exceed the budget under extreme overflow (e.g. backlog=99) —
// the caps + "+N" tags hold the layout to the budget for normal
// counts, and the extreme cases are rare enough that line-wrap on a
// narrow terminal is acceptable.
func TestBuildFactoryArtWidth(t *testing.T) {
	cases := []dash.FactoryState{
		{},
		{BacklogCount: 1},
		{ActiveStages: []dash.ActiveStation{{Stage: "design"}}},
		{CompletedCount: 1},
		{BacklogCount: 3, ActiveStages: []dash.ActiveStation{{Stage: "design"}, {Stage: "code"}}, CompletedCount: 4},
	}
	for i, st := range cases {
		r := rand.New(rand.NewSource(int64(i + 1)))
		for j, line := range dash.BuildFactoryArt(st, dash.ArtWidth, r) {
			n := utf8.RuneCountInString(line)
			if n < dash.ArtWidth {
				t.Errorf("case %d line %d: width=%d want ≥ %d, line=%q", i, j, n, dash.ArtWidth, line)
			}
		}
	}
}

// TestBuildFactoryArtSmokeContainsOnlyPaletteRunes: every non-space
// rune on either smoke row must come from the smoke palette. Pins
// that neither the drift wisp nor the base puff ever accidentally
// pulls a rune from the rail. Stations carry a runningDoc so the
// smoke path actually fires — otherwise the palette assertion is
// vacuous.
func TestBuildFactoryArtSmokeContainsOnlyPaletteRunes(t *testing.T) {
	state := dash.FactoryState{
		BacklogCount: 3,
		ActiveStages: []dash.ActiveStation{
			{Stage: "design", RunningDoc: "design"},
			{Stage: "code", RunningDoc: "code"},
			{Stage: "design", RunningDoc: "design"},
			{Stage: "code", RunningDoc: "code"},
		},
	}
	allowed := make(map[rune]struct{}, len(dash.SmokeGlyphs)+1)
	allowed[' '] = struct{}{}
	for _, g := range dash.SmokeGlyphs {
		allowed[g] = struct{}{}
	}
	// Iterate seeds so we explore the RNG; any seed that produces a
	// non-palette rune on either smoke row fails the test.
	for seed := int64(1); seed <= 8; seed++ {
		r := rand.New(rand.NewSource(seed))
		lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
		for row, label := range []string{"drift", "base"} {
			for _, ru := range lines[row] {
				if _, ok := allowed[ru]; !ok {
					t.Fatalf("seed %d: %s smoke row contains non-palette rune %q in %q", seed, label, ru, lines[row])
				}
			}
		}
	}
}

// TestBuildFactoryArtAlwaysSmokesWhenRunning: every running station
// gets a non-space rune on the base smoke row in its chimney column,
// for every seed. This is the p=1.0 base-puff guarantee — the
// dash-cooler-smoke contract that liveness is a *reliable* peripheral
// signal, not a flickering one. If a future change reintroduces a
// probability gate on the base row, this test fires immediately.
func TestBuildFactoryArtAlwaysSmokesWhenRunning(t *testing.T) {
	state := dash.FactoryState{ActiveStages: []dash.ActiveStation{
		{Stage: "design", RunningDoc: "design"},
		{Stage: "code", RunningDoc: "code"},
		{Stage: "awaiting merge", RunningDoc: "code"},
	}}
	for seed := int64(1); seed <= 32; seed++ {
		r := rand.New(rand.NewSource(seed))
		lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
		if len(lines) != 3 {
			t.Fatalf("seed %d: expected 3 lines, got %d", seed, len(lines))
		}
		baseRunes := []rune(lines[1])
		var chimneys []int
		for i, ru := range []rune(lines[2]) {
			if ru == '[' {
				chimneys = append(chimneys, i+1)
			}
		}
		if got, want := len(chimneys), len(state.ActiveStages); got != want {
			t.Fatalf("seed %d: expected %d chimneys, found %d in rail %q", seed, want, got, lines[2])
		}
		for _, c := range chimneys {
			if c >= len(baseRunes) {
				t.Fatalf("seed %d: chimney col %d out of range for base row %q", seed, c, lines[1])
			}
			if baseRunes[c] == ' ' {
				t.Fatalf("seed %d: expected non-space rune above chimney col %d on base row %q (rail %q)",
					seed, c, lines[1], lines[2])
			}
		}
	}
}

// TestBuildFactoryArtRunningDocOverridesParkedGlyph: a station whose
// run is parked at design but has an open code session shows the code
// glyph (⚙), not the design glyph (⚒). The art names what's live; the
// dashboard rows below carry the parked stage. Mirrors the text-side
// "[code running]" marker that motivates this rail.
func TestBuildFactoryArtRunningDocOverridesParkedGlyph(t *testing.T) {
	state := dash.FactoryState{ActiveStages: []dash.ActiveStation{
		{Stage: "design", RunningDoc: "code"},
	}}
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	rail := lines[2]
	if !strings.Contains(rail, "[⚙]") {
		t.Fatalf("expected running-doc glyph '[⚙]' on parked-design station, got rail:\n%q", rail)
	}
	if strings.Contains(rail, "[⚒]") {
		t.Fatalf("expected no parked-stage glyph '[⚒]' when running doc differs, got rail:\n%q", rail)
	}
}

// TestBuildFactoryArtAwaitingMergeRunningSmokesAndSwapsGlyph: an
// awaiting-merge station with an open session swaps to the running
// doc's glyph and earns smoke. Pre-rail awaiting-merge was always
// non-smoking; under liveness-as-smoke the rule is "smoke iff session,"
// so a session against a pushed run reads as work, not as shipped.
func TestBuildFactoryArtAwaitingMergeRunningSmokesAndSwapsGlyph(t *testing.T) {
	state := dash.FactoryState{ActiveStages: []dash.ActiveStation{
		{Stage: "awaiting merge", RunningDoc: "code"},
	}}
	// Glyph swap is deterministic.
	r := rand.New(rand.NewSource(1))
	lines := dash.BuildFactoryArt(state, dash.ArtWidth, r)
	rail := lines[2]
	if !strings.Contains(rail, "[⚙]") {
		t.Fatalf("expected running-doc glyph '[⚙]' on awaiting-merge station, got rail:\n%q", rail)
	}
	if strings.Contains(rail, "[▶]") {
		t.Fatalf("expected no parked '[▶]' glyph on running awaiting-merge station, got rail:\n%q", rail)
	}
	// Base puff is the load-bearing liveness signal (p=1.0). Sweep
	// seeds anyway as a regression net so a future change can't quietly
	// drop awaiting-merge from the smoke set.
	smokedAt := int64(-1)
	for seed := int64(1); seed <= 16; seed++ {
		r := rand.New(rand.NewSource(seed))
		ls := dash.BuildFactoryArt(state, dash.ArtWidth, r)
		if strings.TrimSpace(ls[1]) != "" {
			smokedAt = seed
			break
		}
	}
	if smokedAt < 0 {
		t.Fatal("expected base puff above running awaiting-merge station for at least one seed in [1,16], saw none")
	}
}

// TestDashOpenSessionSwapsArtGlyph: end-to-end check that an open
// session on a different doc threads through dash.FactoryStateFromRows and
// lands a running-doc glyph in the dash's rail. Pins the wiring
// classify → dashRow.runningDoc → dash.FactoryState → buildRail.
func TestDashOpenSessionSwapsArtGlyph(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Fresh sdlc run: parked at design.
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	titleIdx := strings.Index(got, "MINISTRY OF EVERYTHING")
	activeIdx := strings.Index(got, "ACTIVE (")
	if titleIdx < 0 || activeIdx < 0 {
		t.Fatalf("missing title or ACTIVE marker in:\n%s", got)
	}
	header := got[titleIdx:activeIdx]
	if !strings.Contains(header, "[⚙]") {
		t.Fatalf("expected '[⚙]' (running code) in art header, got:\n%q", header)
	}
	if strings.Contains(header, "[⚒]") {
		t.Fatalf("expected no '[⚒]' (parked design) glyph in art header when code session is open, got:\n%q", header)
	}
}

// TestDashRendersFactoryArt: dash output between the banner line and
// the ACTIVE section carries the factory art (one or two lines), not
// just a blank gap. Pinned at the empty-bureaucracy state because
// it's the easiest deterministic shape (dotted line, no RNG drift on
// stations).
func TestDashRendersFactoryArt(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	titleIdx := strings.Index(got, "MINISTRY OF EVERYTHING")
	activeIdx := strings.Index(got, "ACTIVE (")
	if titleIdx < 0 || activeIdx < 0 {
		t.Fatalf("missing title or ACTIVE marker in:\n%s", got)
	}
	header := got[titleIdx:activeIdx]
	if !strings.Contains(header, "·") {
		t.Fatalf("expected dotted empty-state art between title and ACTIVE, got:\n%q", header)
	}
}

// containsRunRow checks that dash output has a row for (project, run)
// whose last tabwriter field matches stage — ignores the humanAgo
// middle column so tests can be written without pinning wall-clock
// deltas. Project and run are joined in one slash-form column now.
func containsRunRow(out, project, runID, stage string) bool {
	target := project + "/" + runID
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != target {
			continue
		}
		if fields[len(fields)-1] == stage {
			return true
		}
	}
	return false
}
