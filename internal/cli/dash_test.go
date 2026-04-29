package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
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

// TestDashReadyToPushShowsPushStage: design + code turns are in, no
// push turn yet. dash should render the run with next stage "push".
func TestDashReadyToPushShowsPushStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
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
	if !containsRunRow(got, "tele", "fix-it", "sdlc:push") {
		t.Fatalf("expected run row with stage 'sdlc:push', got:\n%s", got)
	}
}

// TestDashPrereqReworkedShowsCodeStage: design is re-signed after the
// code turn, so Next() points back at code. dash should show "code".
func TestDashPrereqReworkedShowsCodeStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "code", "// implementation\n")

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
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
	if !containsRunRow(got, "tele", "fix-it", "sdlc:code") {
		t.Fatalf("expected run row with stage 'sdlc:code', got:\n%s", got)
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

// TestDashKBRunAfterResearchShowsSummarizePending is the mirror-image
// check: research is written but summarize isn't yet. Next() returns
// summarize, so the dash renders `kb:summarize` under ACTIVE.
func TestDashKBRunAfterResearchShowsSummarizePending(t *testing.T) {
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
	if !containsRunRow(got, "tele", "lookup", "kb:summarize") {
		t.Fatalf("expected KB run row with stage 'kb:summarize', got:\n%s", got)
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
