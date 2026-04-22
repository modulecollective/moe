package cli

import (
	"bytes"
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
		"BACKLOG (0)",
		"RUNS (0)",
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
	commitWorkTurnAt(t, root, "fix-it", "design", t0)
	commitWorkTurnAt(t, root, "fix-it", "code", t0.Add(time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "RUNS (1)") {
		t.Fatalf("expected one run row, got:\n%s", got)
	}
	if !strings.Contains(got, "fix-it") || !strings.Contains(got, "tele") {
		t.Fatalf("row missing project/run:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "push") {
		t.Fatalf("expected run row with stage 'push', got:\n%s", got)
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
	commitWorkTurnAt(t, root, "fix-it", "design", t0)
	commitWorkTurnAt(t, root, "fix-it", "code", t0.Add(time.Hour))
	commitWorkTurnAt(t, root, "fix-it", "design", t0.Add(2*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "RUNS (1)") {
		t.Fatalf("expected one run row, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "code") {
		t.Fatalf("expected run row with stage 'code', got:\n%s", got)
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
	if !strings.Contains(got, "RUNS (1)") {
		t.Fatalf("expected one run row, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "design") {
		t.Fatalf("expected run row with stage 'design', got:\n%s", got)
	}
}

// TestDashPushedRunShowsDone: a run with StatusPushed renders as "done"
// in RUNS alongside in-progress runs — terminal runs are no longer
// segregated into a separate RECENT bucket.
func TestDashPushedRunShowsDone(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusPushed)
	commitTrailer(t, root, "push: fix-it",
		"MoE-Run: fix-it\nMoE-PR: https://example.com/pr/1",
		time.Now().UTC().Add(-2*24*time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "RUNS (1)") {
		t.Fatalf("expected one run row, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "fix-it", "done") {
		t.Fatalf("expected run row with stage 'done', got:\n%s", got)
	}
}

// TestDashKBRunAfterSummarizeShowsDone is the regression for the
// disappearing-KB-run bug: a KB run with both research and summarize
// turns committed has Next()==Done but Status==InProgress (KB has no
// push), and must still render as "done" in RUNS.
func TestDashKBRunAfterSummarizeShowsDone(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	seedRun(t, root, "tele", "lookup", "kb", run.StatusInProgress)
	t0 := time.Now().UTC().Add(-2 * 24 * time.Hour)
	commitWorkTurnAt(t, root, "lookup", "research", t0)
	commitWorkTurnAt(t, root, "lookup", "summarize", t0.Add(time.Hour))

	var out, errb bytes.Buffer
	code := Run([]string{"dash"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "RUNS (1)") {
		t.Fatalf("expected KB run to stay visible after summarize, got:\n%s", got)
	}
	if !containsRunRow(got, "tele", "lookup", "done") {
		t.Fatalf("expected KB run row with stage 'done', got:\n%s", got)
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
	if !strings.Contains(out.String(), "RUNS (0)") {
		t.Fatalf("dormant run should be hidden, got:\n%s", out.String())
	}

	// --all: shown.
	out.Reset()
	errb.Reset()
	code = Run([]string{"dash", "--all"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "RUNS (1)") {
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
	noEditor(t)

	// Capture two ideas via the CLI so the file shape and commit
	// trailers come straight from production code paths.
	for _, title := range []string{"Cross-project search", "Faster dash load"} {
		if code := Run([]string{"idea", "add", "tele", title}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
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
	for _, want := range []string{"cross-project-search", "Cross-project search", "faster-dash-load", "Faster dash load"} {
		if !strings.Contains(got, want) {
			t.Fatalf("backlog missing %q in:\n%s", want, got)
		}
	}
	// BACKLOG sits above RUNS.
	backlogIdx := strings.Index(got, "BACKLOG")
	runsIdx := strings.Index(got, "RUNS")
	if !(backlogIdx >= 0 && runsIdx >= 0 && backlogIdx < runsIdx) {
		t.Fatalf("section order wrong (backlog=%d runs=%d):\n%s", backlogIdx, runsIdx, got)
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
