package cli

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/queue"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/wiki"
)

// seedRun writes a minimal run.json + project.json pair under root so
// moe dash's scan finds it. The opening commit is what newTestBureaucracy
// plus commitTrailer supply — tests add work/sign trailers on top.
func seedRun(t *testing.T, root, projectID, runID, workflow, status string) *run.Metadata {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "projects", projectID, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{
		ID:        runID,
		Project:   projectID,
		Title:     "T",
		Status:    status,
		Workflow:  workflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	// Commit it so git log --grep=MoE-Run finds the run at all.
	runJSONRel := filepath.Join(run.Dir(projectID, runID), "run.json")
	projectJSONRel := filepath.Join("projects", projectID, "project.json")
	addCmd := exec.Command("git", "-C", root, "add", runJSONRel, projectJSONRel)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	commitTrailer(t, root, "Open run "+projectID+"/"+runID+": T",
		"MoE-Run: "+runID+"\nMoE-Project: "+projectID, time.Time{})
	return md
}

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
		"Ministry of Everything",
		"ACTIVE (0)",
		"BACKLOG (0)",
		"COMPLETED (0)",
		"0 project(s) registered · 0 active",
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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")
	// Relative to now so the work-turn timestamps stay inside the
	// 30-day dormancy cutoff regardless of when the suite runs. The
	// fixture used to use a hard-coded April 10 2026; once "now" was
	// 30+ days past that, dash filtered the run out and the test
	// failed for date-decay reasons unrelated to what it's checking.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))

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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")

	// Relative to now so the work-turn timestamps stay inside the
	// 30-day dormancy cutoff regardless of when the suite runs. The
	// fixture used to use a hard-coded April 10 2026; once "now" was
	// 30+ days past that, dash filtered the run out and the test
	// failed for date-decay reasons unrelated to what it's checking.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0.Add(2*time.Hour))

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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusPushed)
	commitTrailer(t, root, "push: fix-it",
		"MoE-Run: fix-it\nMoE-PR: https://example.com/pr/42",
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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusMerged)
	commitTrailer(t, root, "push: fix-it merged",
		"MoE-Run: fix-it\nMoE-Merged: abc1234567890",
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

// TestDashClosedRunShowsClosed: a run with StatusClosed (PR closed
// without merging) renders as "closed" in COMPLETED.
func TestDashClosedRunShowsClosed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusClosed)
	commitTrailer(t, root, "push: fix-it closed",
		"MoE-Run: fix-it\nMoE-Closed: https://example.com/pr/42",
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
	if !containsRunRow(got, "tele", "fix-it", "sdlc:closed") {
		t.Fatalf("expected run row with stage 'sdlc:closed', got:\n%s", got)
	}
}

// TestDashKBRunAfterSummarizeShowsDone is the regression for the
// disappearing-KB-run bug: a KB run that's walked the full ladder
// (research + summarize) has Next()==Done but Status==InProgress (KB
// has no push), and must still render as "done" — landing in
// COMPLETED. summarize is the terminal stage in the wiki-engine
// reshape; signing it is publication.
func TestDashKBRunAfterSummarizeShowsDone(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)
	t0 := time.Now().UTC().Add(-2 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "tele", "lookup", "kb", "research", t0)
	commitWorkTurnAt(t, root, "tele", "lookup", "kb", "summarize", t0.Add(time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (1)") {
		t.Fatalf("expected KB run to stay visible after summarize, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "lookup", "kb:done") {
		t.Fatalf("expected KB run row with stage 'kb:done', got:\n%s", got)
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

	seedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)
	t0 := time.Now().UTC().Add(-2 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "tele", "lookup", "kb", "research", t0)

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

func TestDashDormantHiddenWithoutAll(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "old-one", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "work: update spec",
		"MoE-Run: old-one\nMoE-Document: spec",
		time.Now().UTC().Add(-60*24*time.Hour))

	// Default: hidden.
	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ACTIVE (0)") {
		t.Fatalf("dormant run should be hidden, got:\n%s", out.String())
	}

	// --all: shown.
	out.Reset()
	errb.Reset()
	code = Run([]string{"dash", "--all"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ACTIVE (1)") {
		t.Fatalf("--all should reveal dormant run, got:\n%s", out.String())
	}
}

func TestDashSortsNewestFirstWithinBucket(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "older", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "work: update spec",
		"MoE-Run: older\nMoE-Document: spec",
		time.Now().UTC().Add(-3*24*time.Hour))

	seedRun(t, root, "tele", "newer", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "work: update spec",
		"MoE-Run: newer\nMoE-Document: spec",
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
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	// Capture two ideas via the CLI so the commit shape comes straight
	// from production code paths.
	for _, title := range []string{"Cross-project search", "Faster dash load"} {
		if code := Run([]string{"idea", "new", "tele", title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("setup capture failed for %q", title)
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

// TestDashCompletedCapsAtTen seeds more completed runs than the
// dashboard cap and asserts (a) the header shows "N of total" and
// (b) only the newest ten rows render. The cap exists so the section
// doesn't grow unbounded and drown the live sections above it.
func TestDashCompletedCapsAtTen(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// 12 merged runs, oldest first so "newest-first" ordering pushes
	// the newer slugs to the top of the section.
	for i := 0; i < 12; i++ {
		slug := fmt.Sprintf("done-%02d", i)
		seedRun(t, root, "tele", slug, "sdlc", run.StatusMerged)
		commitTrailer(t, root, "push: "+slug+" merged",
			"MoE-Run: "+slug+"\nMoE-Merged: deadbeef"+slug,
			time.Now().UTC().Add(-time.Duration(12-i)*time.Hour))
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "COMPLETED (10 of 12)") {
		t.Fatalf("expected capped header, got:\n%s", got)
	}
	// Oldest two (done-00, done-01) should be dropped; newest (done-11) shown.
	if !containsRunRow(got, "tele", "done-11", "sdlc:merged") {
		t.Fatalf("expected newest completed run to render, got:\n%s", got)
	}
	for _, dropped := range []string{"done-00", "done-01"} {
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

	idea := seedRun(t, root, "tele", "search-idea", ideaWorkflow, run.StatusPromoted)
	idea.Title = "Cross-project search"
	if err := run.Save(root, idea); err != nil {
		t.Fatal(err)
	}
	seedRun(t, root, "tele", "search-impl", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "Promote idea tele/search-idea → tele/search-impl",
		"MoE-Run: search-idea\nMoE-Project: tele\nMoE-Workflow: idea\nMoE-Promoted-To: tele/search-impl",
		time.Time{})

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "idea:promoted → search-impl") {
		t.Fatalf("expected 'idea:promoted → search-impl' on the promoted row, got:\n%s", got)
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

	seedRun(t, root, "tele", "ghost-idea", ideaWorkflow, run.StatusPromoted)
	commitTrailer(t, root, "Promote idea tele/ghost-idea → tele/never-seeded",
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

// TestDashFilterByProject: with two registered projects each holding an
// active run, `--project foo` narrows the dashboard to foo's row only.
// Empty-string default = no filter, so this also pins that the flag
// only kicks in when set.
func TestDashFilterByProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "bar", "beta", "sdlc", run.StatusInProgress)

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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)

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

	seedRun(t, root, "foo", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "foo", "lookup", "kb", run.StatusInProgress)
	seedRun(t, root, "bar", "lookup", "kb", run.StatusInProgress)

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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

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
		seedRun(t, root, "tele", slug, "sdlc", run.StatusMerged)
		commitTrailer(t, root, "push: "+slug+" merged",
			"MoE-Run: "+slug+"\nMoE-Merged: deadbeef"+slug,
			time.Now().UTC().Add(-time.Duration(12-i)*time.Hour))
	}

	// Default: capped at 10.
	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "COMPLETED (10 of 12)") {
		t.Fatalf("expected capped header by default, got:\n%s", out.String())
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

// seedTwinSession writes a touched file under projects/<project>/digital-twin/
// and commits it with a twin-rail trailer block, simulating one of the
// commits a real reflect/lint/claim session lays down. dash's
// recentTwinSessions scans these by `MoE-Workflow: twin` plus the
// project path scope, so the trailer set has to match what the
// production facades commit.
func seedTwinSession(t *testing.T, root, projectID, slug, docID string, when time.Time) {
	t.Helper()
	twinDir := filepath.Join(root, "projects", projectID, "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One file per commit so each twin session is a real path-scoped
	// commit, not an --allow-empty stub. Use the slug as the marker so
	// repeated calls to the same slug append distinct content.
	marker := filepath.Join(twinDir, "log.md")
	body := fmt.Sprintf("- %s touched %s\n", slug, when.Format(time.RFC3339))
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	addCmd := exec.Command("git", "-C", root, "add",
		filepath.Join("projects", projectID, "digital-twin", "log.md"))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	trailers := fmt.Sprintf("MoE-Run: %s\nMoE-Project: %s\nMoE-Workflow: twin\nMoE-Document: %s",
		slug, projectID, docID)
	commitTrailer(t, root, "twin: "+slug, trailers, when)
}

// seedTwinProject sets up a project.json + bare digital-twin/ dir so
// buildTwinRows emits a row for it. Without a checkpoint the row's
// note is "never reflected", which is enough to exercise the recent
// sub-line.
func seedTwinProject(t *testing.T, root, projectID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID, "digital-twin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "projects", projectID, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
}

// TestDashTwinNoSessionsSuppressesContinuation: a project with a twin
// dir but no twin commits renders the freshness line only — no
// "recent: …" continuation line.
func TestDashTwinNoSessionsSuppressesContinuation(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "TWIN (1)") {
		t.Fatalf("expected TWIN section, got:\n%s", got)
	}
	if !strings.Contains(got, "never reflected") {
		t.Fatalf("expected freshness line for unbootstrapped twin, got:\n%s", got)
	}
	if strings.Contains(got, "recent:") {
		t.Fatalf("did not expect a recent sub-line with no twin commits, got:\n%s", got)
	}
}

// TestDashTwinFreshTwinShowsRecents: a project with a valid checkpoint
// (no unrecorded edits, no closed runs since) and recent twin commits
// renders a single TWIN row with the recents inline. The attention
// path is silent, so the recents line *is* the row — no synthetic
// "fresh — last reflected …" prefix, no two-line shape.
func TestDashTwinFreshTwinShowsRecents(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")
	// Checkpoint dated yesterday: twinStatusNote returns "" because
	// there are no closed runs since and DetectUnrecordedEdits sees no
	// managed-doc commits in the bureaucracy.
	if err := wiki.WriteCheckpoint(
		filepath.Join(root, "projects", "moe", "digital-twin"),
		wiki.Checkpoint{
			Version:      wiki.CheckpointVersion,
			LastIngestAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
			Project:      "moe",
		},
	); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-100000", "reflect", now.Add(-3*time.Hour))
	seedTwinSession(t, root, "moe", "lint-2026-04-29-110000", "lint", now.Add(-2*time.Hour))
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-120000", "reflect", now.Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "TWIN (1)") {
		t.Fatalf("expected TWIN section for fresh twin with recents, got:\n%s", got)
	}
	if !strings.Contains(got, "recent:") {
		t.Fatalf("expected recents line on the fresh row, got:\n%s", got)
	}
	for _, banned := range []string{"never reflected", "last reflected", "unrecorded edits"} {
		if strings.Contains(got, banned) {
			t.Fatalf("unexpected attention text %q on a fresh row, got:\n%s", banned, got)
		}
	}
	// The single-line shape puts the project id and the recents text on
	// the same line — no continuation row with a blank project column.
	var freshLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "recent:") {
			freshLine = line
			break
		}
	}
	fields := strings.Fields(freshLine)
	if len(fields) == 0 || fields[0] != "moe" {
		t.Fatalf("expected the recents line to lead with the project id 'moe', got %q in:\n%s", freshLine, got)
	}
}

// TestDashTwinFreshAndNoRecentsSuppressesRow: a fresh checkpoint and
// no twin commits in the journal — both signals are empty — drops the
// row. With moe as the only twin project, the whole TWIN section
// vanishes. Pins the both-empty case so a future change can't bring
// back a content-less row.
func TestDashTwinFreshAndNoRecentsSuppressesRow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")
	if err := wiki.WriteCheckpoint(
		filepath.Join(root, "projects", "moe", "digital-twin"),
		wiki.Checkpoint{
			Version:      wiki.CheckpointVersion,
			LastIngestAt: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339),
			Project:      "moe",
		},
	); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, "TWIN (") {
		t.Fatalf("expected no TWIN section when both signals are empty, got:\n%s", got)
	}
}

// TestDashTwinRecentListsVerbsNewestFirst: three twin sessions
// (reflect, lint, claim) at distinct times produce a "recent: …"
// continuation line listing the verbs newest-first.
func TestDashTwinRecentListsVerbsNewestFirst(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")

	now := time.Now().UTC()
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-100000", "reflect", now.Add(-3*time.Hour))
	seedTwinSession(t, root, "moe", "lint-2026-04-29-110000", "lint", now.Add(-2*time.Hour))
	seedTwinSession(t, root, "moe", "claim-2026-04-29-120000", "claim", now.Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	tail := recentLine(got)
	if tail == "" {
		t.Fatalf("expected a recent sub-line, got:\n%s", got)
	}
	claimIdx := strings.Index(tail, "claim ")
	lintIdx := strings.Index(tail, "lint ")
	reflectIdx := strings.Index(tail, "reflect ")
	if claimIdx < 0 || lintIdx < 0 || reflectIdx < 0 {
		t.Fatalf("missing one of the verbs claim=%d lint=%d reflect=%d in %q",
			claimIdx, lintIdx, reflectIdx, tail)
	}
	if !(claimIdx < lintIdx && lintIdx < reflectIdx) {
		t.Fatalf("expected newest-first ordering claim < lint < reflect, got %d %d %d in %q",
			claimIdx, lintIdx, reflectIdx, tail)
	}
}

// recentLine returns the first "recent: …" sub-line in dash output,
// trimmed to that line. Returns "" if no such line exists.
func recentLine(out string) string {
	idx := strings.Index(out, "recent:")
	if idx < 0 {
		return ""
	}
	tail := out[idx:]
	if nl := strings.Index(tail, "\n"); nl >= 0 {
		tail = tail[:nl]
	}
	return tail
}

// TestDashTwinRecentCapsAtThree: more than three twin sessions still
// renders only the three newest. Older sessions stay on the journal —
// the cap is hard, no `--all` lift, since twin activity isn't
// "dormant" the way runs are.
func TestDashTwinRecentCapsAtThree(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")

	now := time.Now().UTC()
	// Five sessions; oldest two should drop. Distinct slugs so each
	// counts as its own session group.
	seedTwinSession(t, root, "moe", "reflect-2026-04-25-100000", "reflect", now.Add(-5*time.Hour))
	seedTwinSession(t, root, "moe", "reflect-2026-04-26-100000", "reflect", now.Add(-4*time.Hour))
	seedTwinSession(t, root, "moe", "lint-2026-04-27-100000", "lint", now.Add(-3*time.Hour))
	seedTwinSession(t, root, "moe", "claim-2026-04-28-100000", "claim", now.Add(-2*time.Hour))
	seedTwinSession(t, root, "moe", "lint-2026-04-29-100000", "lint", now.Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	recentIdx := strings.Index(got, "recent:")
	if recentIdx < 0 {
		t.Fatalf("expected a recent sub-line, got:\n%s", got)
	}
	// Slice off the rest of the recent line through the next newline.
	tail := got[recentIdx:]
	if nl := strings.Index(tail, "\n"); nl >= 0 {
		tail = tail[:nl]
	}
	// Three verb tokens, separated by ", " — count commas as the proxy
	// (two commas → three entries). Anything more means the cap leaked.
	if got, want := strings.Count(tail, ", "), 2; got != want {
		t.Fatalf("expected %d separators (3 verbs), got %d in %q", want, got, tail)
	}
}

// TestDashTwinGroupsCommitsBySlug: multiple commits sharing the same
// MoE-Run slug count as one session. The session's "when" is the
// latest commit's time, so the verb appears once at that timestamp,
// not three times.
func TestDashTwinGroupsCommitsBySlug(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "moe")

	now := time.Now().UTC()
	// One reflect session lays three commits across its run (per-doc
	// turns + the finalize commit). The dash should fold them into one
	// "reflect" entry, not three.
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-100000", "vision", now.Add(-3*time.Hour))
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-100000", "architecture", now.Add(-2*time.Hour))
	seedTwinSession(t, root, "moe", "reflect-2026-04-29-100000", "operations", now.Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	recentIdx := strings.Index(got, "recent:")
	if recentIdx < 0 {
		t.Fatalf("expected a recent sub-line, got:\n%s", got)
	}
	tail := got[recentIdx:]
	if nl := strings.Index(tail, "\n"); nl >= 0 {
		tail = tail[:nl]
	}
	if n := strings.Count(tail, "reflect "); n != 1 {
		t.Fatalf("expected exactly one 'reflect' entry, got %d in %q", n, tail)
	}
	if strings.Contains(tail, ", ") {
		t.Fatalf("expected single grouped entry, got list in %q", tail)
	}
}

// TestDashTwinRecentScopedToProject: a twin commit on a different
// project must not leak into another project's recent sub-line. Each
// row is keyed by project, and the path-scope on the git query should
// guarantee that.
func TestDashTwinRecentScopedToProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedTwinProject(t, root, "alpha")
	seedTwinProject(t, root, "beta")

	now := time.Now().UTC()
	seedTwinSession(t, root, "alpha", "reflect-2026-04-29-100000", "reflect", now.Add(-1*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	// alpha's row should have a recent line; beta's row should not —
	// the path-scoped git query on each project's twin dir keeps them
	// independent. Counting "recent:" lines is the cheap proof.
	if n := strings.Count(got, "recent:"); n != 1 {
		t.Fatalf("expected exactly one recent: line (alpha only), got %d in:\n%s", n, got)
	}
	tail := recentLine(got)
	if !strings.Contains(tail, "reflect ") {
		t.Fatalf("expected alpha's recent line to mention reflect, got %q", tail)
	}
}

// TestDashQueuedRunGetsMarker: with two active runs, only the one in
// .moe/queue.json picks up the "[queued]" suffix on its note column.
func TestDashQueuedRunGetsMarker(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "queued-one", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "loose-one", "sdlc", run.StatusInProgress)
	if err := queue.Save(root, []queue.Item{
		{Workflow: "sdlc", Project: "tele", Run: "queued-one"},
	}); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "sdlc:design [queued]") {
		t.Fatalf("expected 'sdlc:design [queued]' on the queued row, got:\n%s", got)
	}
	// The non-queued row's note should be plain "sdlc:design" — find
	// the row line and assert no "[queued]" appears on it.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "loose-one") && strings.Contains(line, "[queued]") {
			t.Fatalf("non-queued row should not be marked, got line: %q", line)
		}
	}
}

// TestDashMissingQueueFileNoError: an absent .moe/queue.json is the
// common case (no queue ever used) and must produce no markers and no
// error printed to stderr.
func TestDashMissingQueueFileNoError(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("expected silent stderr with missing queue.json, got: %q", errb.String())
	}
	if strings.Contains(out.String(), "[queued]") {
		t.Fatalf("expected no markers when queue.json is missing, got:\n%s", out.String())
	}
}

// TestDashCorruptQueueFileSilent: a corrupt queue.json is best-effort
// — dash drops the marker pass and renders the rest of the output
// without printing an error. Loud handling of a corrupt queue belongs
// in `moe queue add/list/run`, where the operator can act.
func TestDashCorruptQueueFileSilent(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queue.Path(root), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("expected silent stderr with corrupt queue.json, got: %q", errb.String())
	}
	if !strings.Contains(out.String(), "ACTIVE (1)") {
		t.Fatalf("expected dash to render despite corrupt queue.json, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "[queued]") {
		t.Fatalf("expected no markers when queue.json is corrupt, got:\n%s", out.String())
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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	// Relative to now so the work-turn timestamps stay inside the
	// 30-day dormancy cutoff regardless of when the suite runs. The
	// fixture used to use a hard-coded April 10 2026; once "now" was
	// 30+ days past that, dash filtered the run out and the test
	// failed for date-decay reasons unrelated to what it's checking.
	t0 := time.Now().UTC().Add(-3 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
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
	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
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

// TestDashOpenSessionAndQueuedStackInOrder: when a run is both queued
// and has an open session on a different doc, both markers render and
// the liveness signal lands in front — "[code running]" before
// "[queued]" — so the "what's happening now" answer arrives ahead of
// the playlist membership.
func TestDashOpenSessionAndQueuedStackInOrder(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	if err := queue.Save(root, []queue.Item{
		{Workflow: "sdlc", Project: "tele", Run: "fix-it"},
	}); err != nil {
		t.Fatal(err)
	}
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
	if !strings.Contains(out.String(), "sdlc:design [code running] [queued]") {
		t.Fatalf("expected '[code running]' before '[queued]', got:\n%s", out.String())
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

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

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
	if !strings.Contains(rail, "▦▦ ▶") {
		t.Fatalf("expected '▦▦ ▶' feed arrow after input glyphs, got rail:\n%q", rail)
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
	for _, want := range []string{"+3", "+2", "+7"} {
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
	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
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
	titleIdx := strings.Index(got, "Ministry of Everything")
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

// TestDashRendersFactoryArt: dash output between the title line and
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
	titleIdx := strings.Index(got, "Ministry of Everything")
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
// deltas. tabwriter pads with spaces so we scan each line for the
// three tokens in order and require no other tokens after stage.
func containsRunRow(out, project, runID, stage string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[0] != project || fields[1] != runID {
			continue
		}
		if fields[len(fields)-1] == stage {
			return true
		}
	}
	return false
}
