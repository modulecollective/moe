package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
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

// newMoeHome is the standard cli fixture: a seeded bureaucracy root,
// marked, and bound as MOE_HOME so Run(...) resolves it and nothing
// leaks to the ambient bureaucracy.
func newMoeHome(t *testing.T) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	return root
}

func TestDashEmptyBureaucracy(t *testing.T) {
	newMoeHome(t)
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
// "sdlc:build". The operator either re-runs code, which fires the push
// chain prompt again, or types `moe sdlc push` directly to ship.
func TestDashAfterCodeShowsCodeStage(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "build", "// implementation\n")
	writeReadyDesign(t, root, stageMetadata("tele", "fix-it", "sdlc"))
	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "build", t0.Add(time.Hour))

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
	if !containsRunRow(got, "tele", "fix-it", "sdlc:build [blocked]") {
		t.Fatalf("expected parked build row with its unready gate badge, got:\n%s", got)
	}
}

// TestDashPrereqReworkedShowsDesignStage: design is re-signed after
// the code turn. Under the forward-walking rule, design's latest turn
// is now newer than code's so design is parked. dash should show
// "design"; the previous backward-walking rule would have shown "build"
// (code stale because prereq newer).
func TestDashPrereqReworkedShowsDesignStage(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "build", "// implementation\n")

	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "build", t0.Add(time.Hour))
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
	if !containsRunRow(got, "tele", "fix-it", "sdlc:design [blocked]") {
		t.Fatalf("expected reworked design row with its missing gate badge, got:\n%s", got)
	}
}

// TestDashFreshRunShowsFirstStage: a seeded run with no work turns at
// all still shows up, with "design" as its next stage.
func TestDashFreshRunShowsFirstStage(t *testing.T) {
	root := newMoeHome(t)
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
	root := newMoeHome(t)
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
	root := newMoeHome(t)
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
	// Unextended reopen chain, so the merged row carries the reopen hint
	// too — the merged row is where "how do I keep going" gets answered
	// now that re-typing the stage verb refuses.
	if !containsRunRow(got, "tele", "fix-it", "sdlc:merged · reopen?") {
		t.Fatalf("expected run row with stage 'sdlc:merged · reopen?', got:\n%s", got)
	}
}

// TestDashClosedRunShowsClosed: a closed sdlc run that hasn't yet been
// reopened renders with a "· reopen?" marker so the operator sees it
// as a candidate to carry forward via `moe sdlc reopen`. The marker
// is gated on workflow ("sdlc") because reopen is an sdlc verb.
func TestDashClosedRunShowsClosed(t *testing.T) {
	root := newMoeHome(t)
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

// TestDashStaleInProgressRunSurfacesInActive: a 60-day-old in-progress
// run shows up in ACTIVE *without* --all, carrying its real age. There
// is no longer a dormancy filter — stale in-progress work is surfaced
// for grooming, not hidden. (Inverts the former
// TestDashDormantHiddenWithoutAll, which asserted the opposite.)
func TestDashStaleInProgressRunSurfacesInActive(t *testing.T) {
	root := newMoeHome(t)
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
	root := newMoeHome(t)
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
	if !containsRunRow(got, "tele", "long-merged", "sdlc:merged · reopen?") {
		t.Fatalf("expected the stale merged run row, got:\n%s", got)
	}
}

func TestDashSortsNewestFirstWithinBucket(t *testing.T) {
	root := newMoeHome(t)
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
	_, after, ok := strings.Cut(got, "idea:capture")
	if !ok {
		t.Fatalf("backlog missing `idea:capture` stage note in:\n%s", got)
	}
	if !strings.Contains(after, "idea:capture") {
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
	newMoeHome(t)
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
	root := newMoeHome(t)
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
	root := newMoeHome(t)
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
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	// 12 merged runs, oldest first so "newest-first" ordering pushes
	// the newer slugs to the top of the section.
	for i := range 12 {
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
	if !containsRunRow(got, "tele", "done-11", "sdlc:merged · reopen?") {
		t.Fatalf("expected newest completed run to render, got:\n%s", got)
	}
	for i := range 12 - dash.CompletedCap {
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
	newMoeHome(t)
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
	root := newMoeHome(t)
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
	root := newMoeHome(t)
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

// Completed promotion pairs keep their own recency slots and inline lineage.
func TestDashPromotedIdeaKeepsSettledSuccessorFlat(t *testing.T) {
	root := newMoeHome(t)
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
	if strings.Contains(got, "↳") || !strings.Contains(got, "idea:promoted → tele/search-impl") {
		t.Fatalf("expected flat rows with inline lineage:\n%s", got)
	}
	ideaAt := strings.Index(got, "tele/search-idea")
	implAt := strings.Index(got, "tele/search-impl")
	if ideaAt < 0 || implAt < 0 || implAt > ideaAt {
		t.Fatalf("expected the newer successor first:\n%s", got)
	}
}

// TestDashFilterByProject: with two registered projects each holding an
// active run, `--project foo` narrows the dashboard to foo's row only.
// Empty-string default = no filter, so this also pins that the flag
// only kicks in when set.
func TestDashFilterByProject(t *testing.T) {
	root := newMoeHome(t)
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
// workflows; `--workflow chat` keeps the chat row and drops the sdlc row.
func TestDashFilterByWorkflow(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "lookup", "chat", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--workflow", "chat"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one active row after --workflow chat, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "lookup", "chat:open · resume?") {
		t.Fatalf("expected tele/lookup row, got:\n%s", got)
	}
	if strings.Contains(got, "fix-it") {
		t.Fatalf("did not expect sdlc/fix-it in --workflow chat view:\n%s", got)
	}
}

// TestDashFilterCombined: --project and --workflow compose to the
// intersection. Only the row matching both flags survives.
func TestDashFilterCombined(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "foo", "lookup", "chat", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "bar", "lookup", "chat", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash", "--project", "foo", "--workflow", "chat"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("expected one row at the intersection, got:\n%s", got)
	}
	if !containsRunRow(got, "foo", "lookup", "chat:open · resume?") {
		t.Fatalf("expected foo/lookup (chat) row, got:\n%s", got)
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
	root := newMoeHome(t)
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
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	for i := range 12 {
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
	for i := range 12 {
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
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeReadyDesign(t, root, stageMetadata("tele", "fix-it", "sdlc"))
	// Timestamps are relative to now rather than hard-coded so the
	// fixture doesn't decay as the suite ages.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	writeContent(t, root, "tele", "fix-it", "build", "// implementation\n")
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "build", t0.Add(time.Hour))
	// design + code signed: parked at code under the forward-walking rule.
	sess, err := session.Open(root, "tele", "fix-it", "build")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sdlc:build [running]") {
		t.Fatalf("expected 'sdlc:build [running]' on the active row, got:\n%s", out.String())
	}
}

// TestDashOpenSessionDifferentDocMarksDocRunning: a code session open
// on a run still parked at design — the case that motivates this rail
// — renders "sdlc:design [build running]". Without the marker the dash
// would say "design" while the operator knows code is in flight off
// the dashboard.
func TestDashOpenSessionDifferentDocMarksDocRunning(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	// Fresh run with no work turns: parked at design.
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "build")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "sdlc:design [build running]") {
		t.Fatalf("expected 'sdlc:design [build running]' on the active row, got:\n%s", out.String())
	}
}

// TestDashNoOpenSessionLeavesNoteUnchanged: with no session worktrees
// open, the active-run note carries no "[running]" suffix — the dash's
// behaviour before this rail. Pins the no-marker default so a future
// change can't smuggle a marker into the no-session case.
func TestDashNoOpenSessionLeavesNoteUnchanged(t *testing.T) {
	root := newMoeHome(t)
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

// TestDashStarsLiveSession: end-to-end, an open stage session threads
// classify → Row.RunningDoc → dash.Live and caps today's histogram bar
// with the star; the same run with no session draws none. The board
// keeps no factory rail.
func TestDashStarsLiveSession(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint("live=", live), func(t *testing.T) {
			root := newMoeHome(t)
			t.Setenv("NO_COLOR", "1")
			trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
			if live {
				sess, err := session.Open(root, "tele", "fix-it", "build")
				if err != nil {
					t.Fatalf("session.Open: %v", err)
				}
				t.Cleanup(func() { _ = session.Abandon(sess) })
			}

			var out, errb bytes.Buffer
			if code := Run([]string{"dash"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			got := out.String()
			capIdx := strings.Index(got, "activity · last 60 days")
			activeIdx := strings.Index(got, "ACTIVE (")
			if capIdx < 0 || activeIdx < 0 {
				t.Fatalf("missing caption or ACTIVE marker in:\n%s", got)
			}
			chart := got[:capIdx]
			if n := strings.Count(chart, "*"); n != map[bool]int{false: 0, true: 1}[live] {
				t.Fatalf("live=%v: %d stars in the chart:\n%s", live, n, chart)
			}
			for _, rail := range []string{"[⚙]", "[⚒]", "━━━"} {
				if strings.Contains(got, rail) {
					t.Fatalf("factory rail %q still drawn:\n%s", rail, got)
				}
			}
			// The caption is followed by one blank line, then ACTIVE: no
			// art block in between.
			if between := got[capIdx:activeIdx]; strings.Count(between, "\n") != 2 {
				t.Fatalf("expected the caption, one blank, then ACTIVE; got %q", between)
			}
		})
	}
}

func TestDashRejectsRemovedWatchFlag(t *testing.T) {
	newMoeHome(t)
	t.Setenv("NO_COLOR", "1")
	for _, arg := range []string{"--watch", "--watch=false"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run([]string{"dash", arg}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit=%d want 2 (stdout=%q stderr=%q)", code, out.String(), errb.String())
			}
			if !strings.Contains(errb.String(), "flag provided but not defined: -watch") {
				t.Fatalf("expected unknown watch flag error, got:\n%s", errb.String())
			}
			if out.Len() != 0 {
				t.Fatalf("expected no frame on a rejected watch flag, got:\n%q", out.String())
			}
		})
	}
}

func TestDashHelpListsSnapshotFlags(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"dash", "--help"}, &out, &errb); code != 2 {
		t.Fatalf("exit=%d want 2 (stderr=%q)", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("help printed a dashboard: %q", out.String())
	}
	if strings.Contains(errb.String(), "watch") {
		t.Fatalf("help advertises removed watch flag: %s", errb.String())
	}
	for _, flag := range []string{"--all", "--project", "--workflow"} {
		if !strings.Contains(errb.String(), flag) {
			t.Errorf("help lacks %s: %s", flag, errb.String())
		}
	}
}

// TestTTYFrameBufferKeepsColourGate: buffering a UI frame must not make
// its eventual terminal destination disappear from cliout.Enabled.
func TestTTYFrameBufferKeepsColourGate(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	f, err := os.Open("/dev/ptmx")
	if err != nil {
		t.Skipf("no pty device to stand in for the operator's terminal: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if !cliout.Enabled(f) {
		t.Skipf("/dev/ptmx does not classify as a terminal here; nothing to unwrap to")
	}

	frame := ttyFrameBuffer{terminal: f}
	if !cliout.Enabled(&frame) {
		t.Fatal("cliout.Enabled(ttyFrameBuffer{tty}) = false; buffered UI frames would render unstyled")
	}
}

// TestDashBannerTimestampIsLocal pins the banner's render stamp to the
// box's wall clock. The stamp carries no zone marker, so an operator
// reads it as local time — printing UTC made the banner wrong by the
// offset, and late enough in the evening wrong by a whole day.
//
// time.Local is swapped rather than TZ set: the runtime resolves TZ
// once, on first use, which in a test binary has long since happened.
// Without the swap this assertion is vacuous on a UTC host, which is
// most of them. Safe here because no test in this package is parallel.
func TestDashBannerTimestampIsLocal(t *testing.T) {
	newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	// An offset no host is plausibly in, and not a whole number of
	// hours, so a UTC stamp can't slip through on coincidence.
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	time.Local = time.FixedZone("MOETEST", 7*3600+1800)

	var out, errb bytes.Buffer
	before := time.Now()
	code := Run([]string{"dash"}, &out, &errb)
	after := time.Now()
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	// The banner is the logo and the clock, nothing else.
	const marker = "░▒▓  "
	var stamp string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if !strings.Contains(line, "MINISTRY OF EVERYTHING") {
			continue
		}
		_, after, ok := strings.Cut(line, marker)
		if !ok {
			t.Fatalf("banner line has no %q marker: %q", marker, line)
		}
		stamp = strings.TrimSpace(after)
	}
	if stamp == "" {
		t.Fatalf("no banner line in output:\n%s", out.String())
	}
	// Minute-truncated, so it names either end of the call window. A UTC
	// stamp would be off by the zone's 7h30m and match neither.
	if stamp != dash.Clock(before) && stamp != dash.Clock(after) {
		t.Fatalf("banner stamp %q, want the local clock %q", stamp, dash.Clock(after))
	}
}

// TestDashOpensWithThePulseSection: the serve cluster moved off the
// banner into the PULSE section, which sits first on the board — above
// the art and ACTIVE — and carries a dead serve's state and the
// operator's pending pulse notes.
func TestDashOpensWithThePulseSection(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")
	now := time.Now()
	pid := deadPid(t)
	writeServeState(t, root, serve.ActivitySnapshot{Pid: pid, Armed: true, Started: now.Add(-time.Hour), WrittenAt: now.Add(-3 * time.Hour)})
	if _, err := input.AddPulse(root, "do the lightpool fix next\n\nit blocks the release"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	lines := strings.Split(out.String(), "\n")
	if strings.Contains(lines[0], "serve") || strings.Contains(lines[0], "·") {
		t.Fatalf("banner carries more than logo and clock: %q", lines[0])
	}
	if lines[1] != "PULSE" {
		t.Fatalf("board does not open with PULSE:\n%s", out.String())
	}
	if want := fmt.Sprintf("  serve dead (pid %d) · stale 3h 0m", pid); lines[2] != want {
		t.Fatalf("status line = %q, want %q", lines[2], want)
	}
	if !strings.HasPrefix(lines[3], "  ▒ #1  do the lightpool fix next  · waiting — serve is not armed") {
		t.Fatalf("pending note line = %q", lines[3])
	}
	if pulse, active := strings.Index(out.String(), "PULSE"), strings.Index(out.String(), "ACTIVE ("); pulse > active {
		t.Fatalf("PULSE is not above ACTIVE:\n%s", out.String())
	}
}

// containsRunRow checks that dash output has a row for (project, run)
// whose last tabwriter field matches stage — ignores the humanAgo
// middle column so tests can be written without pinning wall-clock
// deltas. Project and run are joined in one slash-form column now.
func containsRunRow(out, project, runID, stage string) bool {
	target := project + "/" + runID
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != target {
			continue
		}
		// Suffix rather than last-field so a note carrying an action hint
		// ("sdlc:merged · reopen?") is assertable as one string. Anchored
		// at end-of-row either way.
		if strings.HasSuffix(strings.Join(fields, " "), stage) {
			return true
		}
	}
	return false
}
