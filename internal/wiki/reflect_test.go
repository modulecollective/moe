package wiki

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventsSinceCheckpointNoTruncation locks in the design decision
// to ditch the per-category caps. With ~30 commits and ~10 closed
// runs since the checkpoint SHA, every event should land in the
// rendered block and there should be no "(N more truncated)" tail.
func TestEventsSinceCheckpointNoTruncation(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Project repo: capture the pre-checkpoint SHA, then layer 30 more
	// commits on top so the SHA..HEAD walk is well past the old cap.
	projectRepo := newGitRepo(t)
	writeFile(t, filepath.Join(projectRepo, "seed.txt"), "seed\n")
	gitInRepo(t, projectRepo, "add", "seed.txt")
	gitInRepo(t, projectRepo, "commit", "-m", "seed file")

	revCmd := exec.Command("git", "rev-parse", "HEAD")
	revCmd.Dir = projectRepo
	revOut, err := revCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	checkpointSHA := strings.TrimSpace(string(revOut))

	const commitCount = 30
	for i := 0; i < commitCount; i++ {
		path := filepath.Join(projectRepo, fmt.Sprintf("file-%02d.txt", i))
		writeFile(t, path, "x\n")
		gitInRepo(t, projectRepo, "add", filepath.Base(path))
		gitInRepo(t, projectRepo, "commit", "-m", fmt.Sprintf("commit %02d", i))
	}

	// Closed runs: write 10 run.json files under the project's runs/
	// dir, all with status=closed, and one MoE-Run-trailered commit
	// per run dated after the checkpoint so LastActivityMap surfaces
	// each as "since reflect."
	runsRoot := filepath.Join(root, "projects", "p", "runs")
	const runCount = 10
	for i := 0; i < runCount; i++ {
		runID := fmt.Sprintf("run-%02d", i)
		runDir := filepath.Join(runsRoot, runID)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]string{
			"id":       runID,
			"project":  "p",
			"title":    "title " + runID,
			"status":   "closed",
			"workflow": "sdlc",
		})
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		commitWithRunTrailer(t, root, "Open run p/"+runID, runID,
			fmt.Sprintf("2026-04-02T%02d:00:00Z", i%24))
	}

	cp := Checkpoint{
		Version:        CheckpointVersion,
		LastIngestAt:   "2026-04-01T12:00:00Z",
		LastIngestRun:  "prior-reflect",
		Project:        "p",
		ProjectRepoSHA: &checkpointSHA,
	}
	if err := WriteCheckpoint(twinDir, cp); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ProjectRepoPath: projectRepo,
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := EventsSinceCheckpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "more truncated") {
		t.Errorf("events block should not truncate; got:\n%s", got)
	}
	// Each commit and run id should be named verbatim in the output.
	for i := 0; i < commitCount; i++ {
		want := fmt.Sprintf("commit %02d", i)
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in events block:\n%s", want, got)
		}
	}
	for i := 0; i < runCount; i++ {
		want := fmt.Sprintf("run-%02d", i)
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in events block:\n%s", want, got)
		}
	}
}

// TestEventsSinceCheckpointFirstReflectUnbounded covers the first-
// reflect path: no checkpoint, full project history listed verbatim
// (the old code windowed to 30 days). One commit dated well before
// the old window proves the window is gone.
func TestEventsSinceCheckpointFirstReflectUnbounded(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	projectRepo := newGitRepo(t)
	// Author a commit dated 90 days before "now" — outside the old
	// 30-day window. With no caps, it must still appear.
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T12:00:00Z")
	writeFile(t, filepath.Join(projectRepo, "ancient.txt"), "ancient\n")
	gitInRepo(t, projectRepo, "add", "ancient.txt")
	gitInRepo(t, projectRepo, "commit", "-m", "ancient commit")

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ProjectRepoPath: projectRepo,
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := EventsSinceCheckpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ancient commit") {
		t.Errorf("first reflect should list full history; missing 'ancient commit' in:\n%s", got)
	}
	if strings.Contains(got, "more truncated") {
		t.Errorf("first reflect should not truncate; got:\n%s", got)
	}
}

// ReflectPromptSection now carries roadmap conventions and the
// hygiene-walk framing that used to live in PlanPromptSection /
// LintPromptSection. Pin those so a future trim doesn't silently
// drop them.
func TestReflectPromptSectionCarriesRoadmapAndHygiene(t *testing.T) {
	got, err := ReflectPromptSection(Config{
		Mode:        Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Phrasing assertions are tolerant of the body's wrapped lines —
	// check for tokens that survive the line wrap rather than long
	// runs of prose.
	for _, want := range []string{
		"Reflect pass (closed-schema)",
		"Roadmap convention",
		"Mid term",
		"Long term",
		"Parked",
		"Hygiene findings",
		"refuses to seal",
		"don't manufacture work",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reflect prompt missing %q in:\n%s", want, got)
		}
	}
}

// TestEventsSinceCheckpointClosedRunsKeyOnGitHistory pins the design
// fix: closed-run inclusion is decided by the latest MoE-Run-trailered
// commit's committer time, not by the run dir's filesystem mtime. Two
// runs, one committed before the checkpoint and one after; the
// pre-checkpoint run's dir mtime is bumped to "now" by writing a stray
// file inside it. The mtime-based predecessor would surface the old
// run as new; the git-history one excludes it.
func TestEventsSinceCheckpointClosedRunsKeyOnGitHistory(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runsRoot := filepath.Join(root, "projects", "p", "runs")
	for _, r := range []struct {
		id     string
		commit string
	}{
		{id: "old-run", commit: "2026-03-15T10:00:00Z"},   // before checkpoint
		{id: "fresh-run", commit: "2026-04-15T10:00:00Z"}, // after checkpoint
	} {
		runDir := filepath.Join(runsRoot, r.id)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]string{
			"id":       r.id,
			"project":  "p",
			"title":    "title " + r.id,
			"status":   "closed",
			"workflow": "sdlc",
		})
		if err := os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		commitWithRunTrailer(t, root, "Open run p/"+r.id, r.id, r.commit)
	}
	// Bump the old run's dir mtime to "now" by writing a stray file
	// inside it. The mtime-based code would surface this as new; the
	// git-history code must not.
	writeFile(t, filepath.Join(runsRoot, "old-run", "stray.txt"), "touch\n")

	cp := Checkpoint{
		Version:       CheckpointVersion,
		LastIngestAt:  "2026-04-01T12:00:00Z",
		LastIngestRun: "prior-reflect",
		Project:       "p",
	}
	if err := WriteCheckpoint(twinDir, cp); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := EventsSinceCheckpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "fresh-run") {
		t.Errorf("expected post-checkpoint run in events block:\n%s", got)
	}
	if strings.Contains(got, "old-run") {
		t.Errorf("pre-checkpoint run should not appear despite bumped dir mtime:\n%s", got)
	}
}

// commitWithRunTrailer creates an empty bureaucracy commit carrying a
// MoE-Run trailer for runID, dated when (RFC3339). The trailer is what
// run.LastActivityMap keys on, so reflect tests use this to give a run
// a real activity time without writing files.
func commitWithRunTrailer(t *testing.T, root, subject, runID, when string) {
	t.Helper()
	msg := subject + "\n\nMoE-Run: " + runID + "\n"
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", msg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+when,
		"GIT_COMMITTER_DATE="+when,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit %s: %v\n%s", runID, err, out)
	}
}

func TestReadHistorySummaryMissingIsEmpty(t *testing.T) {
	root := t.TempDir()
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Mode: Closed, ContentDir: twinDir, Project: "p"}
	got, err := ReadHistorySummary(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("missing summary should read as empty, got %q", got)
	}
}

func TestReadHistorySummaryReadsContent(t *testing.T) {
	root := t.TempDir()
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, HistorySummaryPath(twinDir), "# History\n\nThe twin was seeded in 2026-Q1.\n")
	cfg := Config{Mode: Closed, ContentDir: twinDir, Project: "p"}
	got, err := ReadHistorySummary(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "seeded in 2026-Q1") {
		t.Errorf("summary content missing, got %q", got)
	}
}
