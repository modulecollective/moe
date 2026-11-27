package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// boardText renders the board's sections — ACTIVE, INTENTS, CHORES,
// BACKLOG, COMPLETED and the footer — from the same gather and renderer
// moe ui paints with, for tests that assert what the run list does not
// carry.
func boardText(t *testing.T, root string, filter DashFilter) string {
	t.Helper()
	now := time.Now()
	snap, err := GatherDashSnapshot(root, now, filter)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	dash.RenderIndexedRows(&out, now, snap.Rows, snap.ProjectCount, snap.ActiveProjects)
	return out.String()
}

// runList runs `moe run list` with args and returns its stdout.
func runList(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run(append([]string{"run", "list"}, args...), &out, &errb); code != 0 {
		t.Fatalf("run list %v: exit=%d stderr=%q", args, code, errb.String())
	}
	return out.String()
}

// listLines is run list output split into its rows.
func listLines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

func TestDashEmptyBureaucracy(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	got := boardText(t, root, DashFilter{})
	for _, want := range []string{
		"ACTIVE (0)",
		"BACKLOG (0)",
		"COMPLETED (0)",
		"0 project(s) registered · 0 with active runs",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
	if out := runList(t, "--all"); out != "" {
		t.Fatalf("run list on an empty bureaucracy = %q, want nothing", out)
	}
}

// TestDashAfterCodeShowsCodeStage: design + code turns are in, no
// push turn yet. Under the forward-walking rule the run is parked at
// code (push has no successor turn after code's), so the row reads
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

	got := runList(t)
	if want := "tele/fix-it\tsdlc\tsdlc:build [blocked]\n"; got != want {
		t.Fatalf("run list = %q, want the parked build row with its unready gate badge %q", got, want)
	}
}

// TestDashPrereqReworkedShowsDesignStage: design is re-signed after
// the code turn. Under the forward-walking rule, design's latest turn
// is now newer than code's so design is parked, and the row shows
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

	got := runList(t)
	if len(listLines(got)) != 1 || !containsRunRow(got, "tele", "fix-it", "sdlc:design [blocked]") {
		t.Fatalf("expected one reworked design row with its missing gate badge, got:\n%s", got)
	}
}

// TestDashFreshRunShowsFirstStage: a seeded run with no work turns at
// all still shows up, with "design" as its next stage.
func TestDashFreshRunShowsFirstStage(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	got := runList(t)
	if len(listLines(got)) != 1 || !containsRunRow(got, "tele", "fix-it", "sdlc:design") {
		t.Fatalf("expected one run row with stage 'sdlc:design', got:\n%s", got)
	}
}

// TestDashPushedRunShowsAwaitingMerge: a run with StatusPushed stays
// active with an "awaiting merge: #<n>" label so the operator sees it
// still owes a click on GitHub. PR number comes from the MoE-PR trailer.
func TestDashPushedRunShowsAwaitingMerge(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusPushed)
	trailerstest.CommitTrailer(t, root, "push: fix-it",
		"MoE-Run: fix-it\nMoE-Project: tele\nMoE-PR: https://example.com/pr/42",
		time.Now().UTC().Add(-2*24*time.Hour))

	got := runList(t)
	if len(listLines(got)) != 1 || !containsRunRow(got, "tele", "fix-it", "sdlc:awaiting merge: #42") {
		t.Fatalf("expected the pushed run active as 'sdlc:awaiting merge: #42', got:\n%s", got)
	}
}

// TestDashMergedRunShowsMerged: a run with StatusMerged is completed:
// absent from the default list, "merged" under --all.
func TestDashMergedRunShowsMerged(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "push: fix-it merged",
		"MoE-Run: fix-it\nMoE-Project: tele\nMoE-Merged: abc1234567890",
		time.Now().UTC().Add(-2*24*time.Hour))

	if got := runList(t); got != "" {
		t.Fatalf("merged run listed as active:\n%s", got)
	}
	got := runList(t, "--all")
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

	if got := runList(t); got != "" {
		t.Fatalf("closed run listed as active:\n%s", got)
	}
	if got := runList(t, "--all"); !containsRunRow(got, "tele", "fix-it", "sdlc:closed · reopen?") {
		t.Fatalf("expected closed sdlc run to carry the reopen marker, got:\n%s", got)
	}
}

// TestDashStaleInProgressRunSurfacesInActive: a 60-day-old in-progress
// run shows up in ACTIVE, carrying its real age. There is no dormancy
// filter — stale in-progress work is surfaced for grooming, not hidden.
func TestDashStaleInProgressRunSurfacesInActive(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "old-one", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "work: update spec",
		"MoE-Run: old-one\nMoE-Project: tele\nMoE-Document: spec",
		time.Now().UTC().Add(-60*24*time.Hour))

	if got := runList(t); !strings.HasPrefix(got, "tele/old-one\t") {
		t.Fatalf("stale in-progress run should be listed as active, got:\n%s", got)
	}
	got := boardText(t, root, DashFilter{})
	if !strings.Contains(got, "ACTIVE (1)") {
		t.Fatalf("stale in-progress run should surface in ACTIVE, got:\n%s", got)
	}
	// The age is the grooming signal: 60 days reads as "60d ago".
	if !strings.Contains(got, "60d ago") {
		t.Fatalf("expected the stale run to show its real age '60d ago', got:\n%s", got)
	}
}

// TestDashStaleMergedRunCountsInCompleted is the regression guard for
// "what happened to 750?": a run merged 60 days ago must still count in
// COMPLETED. The old dormancy gate subtracted such runs from the
// completed total once they aged past 30 days, so the header drifted
// down day by day. The completed count is cumulative history — it should
// only climb.
func TestDashStaleMergedRunCountsInCompleted(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "long-merged", "sdlc", run.StatusMerged)
	trailerstest.CommitTrailer(t, root, "push: long-merged merged",
		"MoE-Run: long-merged\nMoE-Project: tele\nMoE-Merged: abc1234567890",
		time.Now().UTC().Add(-60*24*time.Hour))

	got := boardText(t, root, DashFilter{})
	if !strings.Contains(got, "COMPLETED (1)") {
		t.Fatalf("stale merged run should still count in COMPLETED, got:\n%s", got)
	}
	if !containsRunRow(runList(t, "--all"), "tele", "long-merged", "sdlc:merged · reopen?") {
		t.Fatalf("expected the stale merged run row")
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

	got := runList(t)
	newerIdx := strings.Index(got, "tele/newer\t")
	olderIdx := strings.Index(got, "tele/older\t")
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

	got := boardText(t, root, DashFilter{})
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
	// Open ideas are the backlog, not runs: the run list leaves them to
	// `moe idea list`.
	if out := runList(t, "--all"); out != "" {
		t.Fatalf("run list carried backlog ideas:\n%s", out)
	}
}

func TestDashBacklogEmptyShowsNone(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	if got := boardText(t, root, DashFilter{}); !strings.Contains(got, "BACKLOG (0)") {
		t.Fatalf("expected empty BACKLOG section, got:\n%s", got)
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

	if got := boardText(t, root, DashFilter{}); !strings.Contains(got, "3 project(s) registered") {
		t.Fatalf("expected 3 projects in footer, got:\n%s", got)
	}
}

// TestDashFooterActiveCountsProjectsNotRuns pins the footer semantic:
// "N with active runs" counts *projects* with at least one active run,
// not active rows. Two active runs in one project ⇒ 1, not 2. The
// board already shows the row count on the ACTIVE header, so the
// footer carries a fact you can't read off the section headers.
func TestDashFooterActiveCountsProjectsNotRuns(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-a", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "fix-b", "sdlc", run.StatusInProgress)

	got := boardText(t, root, DashFilter{})
	if !strings.Contains(got, "ACTIVE (2)") {
		t.Fatalf("expected two active rows, got:\n%s", got)
	}
	if !strings.Contains(got, "1 project(s) registered · 1 with active runs") {
		t.Fatalf("expected footer to count the single project once, got:\n%s", got)
	}
}

// TestDashSectionHeadingsDropRuns pins the operator-facing section
// labels: "ACTIVE" and "COMPLETED" — the bare nouns, without the
// implementation-flavored "RUNS" suffix that used to read like a
// schema label rather than a status.
func TestDashSectionHeadingsDropRuns(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	got := boardText(t, root, DashFilter{})
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

	got := runList(t, "--all")
	if !containsRunRow(got, "tele", "search-idea", "idea:promoted → tele/search-impl") {
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

	got := runList(t, "--all")
	if !containsRunRow(got, "tele", "ghost-idea", "idea:promoted") {
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

	got := runList(t, "--all")
	if strings.Contains(got, "↳") || !strings.Contains(got, "idea:promoted → tele/search-impl") {
		t.Fatalf("expected flat rows with inline lineage:\n%s", got)
	}
	ideaAt := strings.Index(got, "tele/search-idea\t")
	implAt := strings.Index(got, "tele/search-impl\t")
	if ideaAt < 0 || implAt < 0 || implAt > ideaAt {
		t.Fatalf("expected the newer successor first:\n%s", got)
	}
}

// TestDashFilterByProject: with two registered projects each holding an
// active run, `--project foo` narrows the list to foo's row only.
func TestDashFilterByProject(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "bar", "beta", "sdlc", run.StatusInProgress)

	if got := runList(t); len(listLines(got)) != 2 {
		t.Fatalf("expected both projects' rows without a filter, got:\n%s", got)
	}
	got := runList(t, "--project", "foo")
	if want := "foo/alpha\tsdlc\tsdlc:design\n"; got != want {
		t.Fatalf("run list --project foo = %q, want %q", got, want)
	}
}

// TestDashFilterByWorkflow: two runs in the same project on different
// workflows; `--workflow chat` keeps the chat row and drops the sdlc row.
func TestDashFilterByWorkflow(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "lookup", "chat", run.StatusInProgress)

	got := runList(t, "--workflow", "chat")
	if want := "tele/lookup\tchat\tchat:open · resume?\n"; got != want {
		t.Fatalf("run list --workflow chat = %q, want %q", got, want)
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

	got := runList(t, "--project", "foo", "--workflow", "chat")
	if want := "foo/lookup\tchat\tchat:open · resume?\n"; got != want {
		t.Fatalf("run list --project foo --workflow chat = %q, want %q", got, want)
	}
}

// TestDashFilterUnknownWorkflowReturnsEmpty: an unknown workflow isn't an
// error — filtering is read-only and an empty list is the obvious miss
// signal, as on the board.
func TestDashFilterUnknownWorkflowReturnsEmpty(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	if got := runList(t, "--workflow", "bogus", "--all"); got != "" {
		t.Fatalf("did not expect any rows for an unknown workflow:\n%s", got)
	}
}

// TestRunListUnknownProjectIsAnError: --project names a registered
// project, as `moe idea list --project` does; a typo is an error rather
// than an empty list that reads like "nothing running".
func TestRunListUnknownProjectIsAnError(t *testing.T) {
	root := newMoeHome(t)
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	if code := Run([]string{"run", "list", "--project", "bogus"}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1 (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), "project bogus not registered") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestRunListRejectsPositionalArgs: the list takes flags only.
func TestRunListRejectsPositionalArgs(t *testing.T) {
	newMoeHome(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"run", "list", "tele"}, &out, &errb); code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: moe run list [--project <id>] [--workflow <name>] [--all]") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// TestDashAllListsEveryCompletedRun: completed runs stay off the default
// list; `--all` adds every one of them, uncapped, newest first, after
// the active rows.
func TestDashAllListsEveryCompletedRun(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "live", "sdlc", run.StatusInProgress)
	for i := range 12 {
		slug := fmt.Sprintf("done-%02d", i)
		trailerstest.SeedRun(t, root, "tele", slug, "sdlc", run.StatusMerged)
		trailerstest.CommitTrailer(t, root, "push: "+slug+" merged",
			"MoE-Run: "+slug+"\nMoE-Project: tele\nMoE-Merged: deadbeef"+slug,
			time.Now().UTC().Add(-time.Duration(12-i)*time.Hour))
	}

	if got := runList(t); got != "tele/live\tsdlc\tsdlc:design\n" {
		t.Fatalf("default list = %q, want only the active run", got)
	}
	lines := strings.Split(strings.TrimSuffix(runList(t, "--all"), "\n"), "\n")
	if len(lines) != 13 || !strings.HasPrefix(lines[0], "tele/live\t") {
		t.Fatalf("--all = %q, want the active run then all 12 completed", lines)
	}
	for i, line := range lines[1:] {
		if want := fmt.Sprintf("tele/done-%02d\tsdlc\tsdlc:merged · reopen?", 11-i); line != want {
			t.Fatalf("--all line %d = %q, want %q", i+1, line, want)
		}
	}
}

// TestRunListShape: three tab-separated fields per line and, even with
// colour allowed, no escape bytes on a non-terminal stdout.
func TestRunListShape(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "lookup", "chat", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "gone", "sdlc", run.StatusClosed)

	got := runList(t, "--all")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("escape bytes on a non-terminal stdout: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	for _, line := range lines {
		if f := strings.Split(line, "\t"); len(f) != 3 || f[0] == "" || f[1] == "" || f[2] == "" {
			t.Fatalf("line %q is not <project>/<slug> TAB <workflow> TAB <note>", line)
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

	if got := runList(t); !strings.Contains(got, "sdlc:build [running]") {
		t.Fatalf("expected 'sdlc:build [running]' on the active row, got:\n%s", got)
	}
}

// TestDashOpenSessionDifferentDocMarksDocRunning: a code session open
// on a run still parked at design — the case that motivates this rail
// — renders "sdlc:design [build running]". Without the marker the row
// would say "design" while the operator knows code is in flight.
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

	if got := runList(t); !strings.Contains(got, "sdlc:design [build running]") {
		t.Fatalf("expected 'sdlc:design [build running]' on the active row, got:\n%s", got)
	}
}

// TestDashNoOpenSessionLeavesNoteUnchanged: with no session worktrees
// open, the active-run note carries no "[running]" suffix. Pins the
// no-marker default so a future change can't smuggle a marker into the
// no-session case.
func TestDashNoOpenSessionLeavesNoteUnchanged(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	if got := runList(t); strings.Contains(got, "running]") {
		t.Fatalf("expected no '[running]' marker without an open session, got:\n%s", got)
	}
}

// TestDashStarsLiveSession: end-to-end, an open stage session threads
// classify → Row.RunningDoc → dash.Live and caps today's histogram bar
// with the star; the same run with no session draws none.
func TestDashStarsLiveSession(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint("live=", live), func(t *testing.T) {
			root := newMoeHome(t)
			trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
			if live {
				sess, err := session.Open(root, "tele", "fix-it", "build")
				if err != nil {
					t.Fatalf("session.Open: %v", err)
				}
				t.Cleanup(func() { _ = session.Abandon(sess) })
			}

			snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
			if err != nil {
				t.Fatal(err)
			}
			chart := strings.Join(dash.BuildActivityHistogram(trailingDays(snap.Histogram, dash.HistDays), dash.Live(snap.Rows)), "\n")
			if n := strings.Count(chart, "*"); n != map[bool]int{false: 0, true: 1}[live] {
				t.Fatalf("live=%v: %d stars in the chart:\n%s", live, n, chart)
			}
		})
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
