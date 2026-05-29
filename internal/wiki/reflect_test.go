package wiki

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
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
	gittest.Run(t, projectRepo, "add", "seed.txt")
	gittest.Run(t, projectRepo, "commit", "-m", "seed file")

	checkpointSHA := gittest.HeadSHA(t, projectRepo)

	const commitCount = 30
	for i := 0; i < commitCount; i++ {
		path := filepath.Join(projectRepo, fmt.Sprintf("file-%02d.txt", i))
		writeFile(t, path, "x\n")
		gittest.Run(t, projectRepo, "add", filepath.Base(path))
		gittest.Run(t, projectRepo, "commit", "-m", fmt.Sprintf("commit %02d", i))
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
	gittest.Run(t, projectRepo, "add", "ancient.txt")
	gittest.Run(t, projectRepo, "commit", "-m", "ancient commit")

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

// TestEventsSinceCheckpointFirstReflectCommitCap covers the soft cap
// on the first-reflect path: with no checkpoint and a project repo
// past the cap, the rendered block must list the newest cap-worth of
// commits and finish with an "(N earlier commits omitted)" footer.
// The cap value and footer wording are part of the surface the agent
// reads, so pin both the boundary subjects and the footer text here.
//
// firstReflectCommitCap is overridden to 10 for this test so the
// fixture stays cheap. The production value (500) would force ~1,200
// git forks per run, which under -race -count=1 ./... races with the
// rest of the git-heavy suite for fork/pipe/fd resources and was
// observed to flake on CI. Same shape as git.go's writeRetryCap test
// seam.
func TestEventsSinceCheckpointFirstReflectCommitCap(t *testing.T) {
	prev := firstReflectCommitCap
	firstReflectCommitCap = 10
	t.Cleanup(func() { firstReflectCommitCap = prev })

	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// newGitRepo seeds a single empty commit; layer 10 content commits
	// on top so total non-merge commits = 11 and the seed alone falls
	// outside the cap.
	projectRepo := newGitRepo(t)
	const content = 10
	for i := 0; i < content; i++ {
		path := filepath.Join(projectRepo, fmt.Sprintf("file-%03d.txt", i))
		writeFile(t, path, "x\n")
		gittest.Run(t, projectRepo, "add", filepath.Base(path))
		gittest.Run(t, projectRepo, "commit", "-m", fmt.Sprintf("commit %03d", i))
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

	// Newest content commit is in; cap-edge (commit 000, the 10th-newest)
	// is in; the pre-content seed commit falls outside the cap.
	for _, want := range []string{"commit 009", "commit 000"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in events block:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "(1 earlier commits omitted)") {
		t.Errorf("expected omitted-count footer in events block:\n%s", got)
	}
	// Vocabulary regression guard: the old per-category windowing used
	// "more truncated"; the new footer uses "earlier commits omitted".
	if strings.Contains(got, "more truncated") {
		t.Errorf("events block should not use the legacy truncation phrasing:\n%s", got)
	}
}

// reflectPromptSection carries the glossary convention and the
// hygiene-walk framing that used to live in PlanPromptSection /
// LintPromptSection. Pin those so a future trim doesn't silently
// drop them.
func TestReflectPromptSectionCarriesConventionsAndHygiene(t *testing.T) {
	got, err := reflectPromptSection(Config{
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
		"Glossary convention",
		"Inclusion bar",
		"Hygiene findings",
		"refuses to seal",
		"don't manufacture work",
		"Primer-plus-reference",
		"Single-home discipline",
		"Reference drift",
		"Intent drift",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reflect prompt missing %q in:\n%s", want, got)
		}
	}
	// The roadmap convention is gone with the roadmap doc. Pin its
	// absence so the dropped prose can't creep back in.
	if strings.Contains(got, "Roadmap convention") {
		t.Errorf("reflect prompt still carries the dropped roadmap convention:\n%s", got)
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
	if !strings.Contains(got, "p/fresh-run (2026-04-15)") {
		t.Errorf("expected post-checkpoint run in events block:\n%s", got)
	}
	if strings.Contains(got, "- fresh-run (2026-04-15)") {
		t.Errorf("closed run should include project prefix:\n%s", got)
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
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_DATE=" + when,
	}, "commit", "--allow-empty", "-m", msg)
}

// TestProjectCommitsSince_StatErrorPropagates pins the design fix for
// site 1: a non-NotExist stat error on cfg.ProjectRepoPath must surface
// rather than smooth into an empty events block. We nest ProjectRepoPath
// under a regular file so os.Stat returns ENOTDIR — errors.Is treats
// that as distinct from fs.ErrNotExist, so the propagate branch fires.
// This shape also runs unchanged whether the test user is root or not.
func TestProjectCommitsSince_StatErrorPropagates(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	writeFile(t, blocker, "")

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ProjectRepoPath: filepath.Join(blocker, "repo"),
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	_, err := EventsSinceCheckpoint(cfg)
	if err == nil {
		t.Fatal("expected stat error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "stat project repo") {
		t.Errorf("expected wrapped 'stat project repo' in error, got: %v", err)
	}
}

// TestProjectCommitsSince_StatNotExistStillSilent pins the legitimate
// case the design preserves: a project repo path that simply doesn't
// exist (twin running before the repo lands) returns empty without
// erroring. Regression guard so a future "always propagate" refactor
// can't accidentally break the first-time-twin flow.
func TestProjectCommitsSince_StatNotExistStillSilent(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ProjectRepoPath: filepath.Join(t.TempDir(), "missing"),
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := EventsSinceCheckpoint(cfg)
	if err != nil {
		t.Fatalf("expected nil error on missing project repo, got: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty events block, got:\n%s", got)
	}
}

// TestProjectCommitsSince_IncrementalGitFailurePropagates pins the
// design fix for site 2: when the checkpoint SHA is unreachable in the
// project repo (history rewrite, shallow clone), `git log SHA..HEAD`
// failure must surface so the operator can reset the checkpoint or
// deepen the clone — not silently render an empty commits block.
func TestProjectCommitsSince_IncrementalGitFailurePropagates(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectRepo := newGitRepo(t)
	writeFile(t, filepath.Join(projectRepo, "x.txt"), "x\n")
	gittest.Run(t, projectRepo, "add", "x.txt")
	gittest.Run(t, projectRepo, "commit", "-m", "x")

	unreachable := "0123456789abcdef0123456789abcdef01234567"
	cp := Checkpoint{
		Version:        CheckpointVersion,
		LastIngestAt:   "2026-04-01T12:00:00Z",
		LastIngestRun:  "prior-reflect",
		Project:        "p",
		ProjectRepoSHA: &unreachable,
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
	_, err := EventsSinceCheckpoint(cfg)
	if err == nil {
		t.Fatal("expected unreachable-SHA git failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "project commit log") {
		t.Errorf("expected wrapped 'project commit log' in error, got: %v", err)
	}
}

// TestClosedRunsSince_MalformedLastIngestAtPropagates pins the design
// fix for site 4: a non-RFC3339 LastIngestAt on the checkpoint must
// surface as a parse error rather than silently leaving threshold at
// zero (which would surface every terminal run as new and corrupt the
// reflect agent's view of the tail).
func TestClosedRunsSince_MalformedLastIngestAtPropagates(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cp := Checkpoint{
		Version:       CheckpointVersion,
		LastIngestAt:  "not-a-timestamp",
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
	_, err := EventsSinceCheckpoint(cfg)
	if err == nil {
		t.Fatal("expected malformed LastIngestAt to propagate as parse error, got nil")
	}
	if !strings.Contains(err.Error(), "LastIngestAt") {
		t.Errorf("expected 'LastIngestAt' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Errorf("expected bad value quoted in error, got: %v", err)
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
	writeFile(t, historySummaryPath(twinDir), "# History\n\nThe twin was seeded in 2026-Q1.\n")
	cfg := Config{Mode: Closed, ContentDir: twinDir, Project: "p"}
	got, err := ReadHistorySummary(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "seeded in 2026-Q1") {
		t.Errorf("summary content missing, got %q", got)
	}
}
