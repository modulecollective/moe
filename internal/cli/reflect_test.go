package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/wiki"
)

func writeWikiDoc(t *testing.T, dir, name, body string) error {
	t.Helper()
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

// TestReflectKickoffContextRendersAllPassSections covers the
// pass-scoped kickoff block every twin stage shares: hygiene findings
// (when non-empty), workflow feedback, idea backlog, history summary,
// events tail. Walks the assembly on real on-disk fixtures so the
// markdown the agent ultimately sees gets exercised end-to-end.
func TestReflectKickoffContextRendersAllPassSections(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeWikiDoc(t, twinDir, "vision.md", "# Vision\n\nReal content.\n"); err != nil {
		t.Fatal(err)
	}
	// history-summary present so the section renders the body.
	if err := writeWikiDoc(t, twinDir, "history-summary.md",
		"The twin was seeded in 2026-Q1; auth rewrite landed in 2026-Q2.\n"); err != nil {
		t.Fatal(err)
	}

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	got, err := reflectKickoffContext(root, "tele", cfg)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}
	for _, want := range []string{
		"## Pass context",
		"### Workflow feedback",
		"(no workflow feedback since the last reflect)",
		"### Idea backlog",
		"(no open ideas captured for this project)",
		"### History summary",
		// By-path pointer, not the body: the kickoff names the file and
		// tells the agent to read it.
		"Read the rolling history summary at",
		wiki.HistorySummaryPath(cfg),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff context missing %q in:\n%s", want, got)
		}
	}
	// The summary body must NOT be inlined — that 116 KB-prone string on
	// argv is what broke the launch with E2BIG. Only the path rides in
	// the kickoff now.
	if strings.Contains(got, "auth rewrite landed") {
		t.Errorf("kickoff inlined the history-summary body; want a by-path pointer only:\n%s", got)
	}
}

// TestReflectKickoffContextReferencesHistorySummaryByPath pins the
// de-inline contract directly: a large summary file is named by absolute
// path with a read imperative, and its body never lands in the kickoff
// (the oversized argv element that failed execve with E2BIG).
func TestReflectKickoffContextReferencesHistorySummaryByPath(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeWikiDoc(t, twinDir, "vision.md", "# Vision\n\nReal content.\n"); err != nil {
		t.Fatal(err)
	}
	// A body large enough to be the kind of string that overran the
	// per-argv ceiling. A unique marker lets us assert it stays out.
	marker := "UNIQUE-HISTORY-MARKER-9f3a"
	body := marker + "\n" + strings.Repeat("history detail line\n", 8000)
	if err := writeWikiDoc(t, twinDir, "history-summary.md", body); err != nil {
		t.Fatal(err)
	}

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	got, err := reflectKickoffContext(root, "tele", cfg)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}
	if strings.Contains(got, marker) {
		t.Errorf("kickoff inlined the history-summary body (marker present); want path only")
	}
	if !strings.Contains(got, wiki.HistorySummaryPath(cfg)) {
		t.Errorf("kickoff missing the history-summary path %q in:\n%s", wiki.HistorySummaryPath(cfg), got)
	}
	if !strings.Contains(got, "Read the rolling history summary at") {
		t.Errorf("kickoff missing the read imperative in:\n%s", got)
	}
}

// Hygiene findings — when the pre-flight scan surfaces issues, they
// land in the context block. Missing managed docs are the simplest
// trigger (the wiki dir doesn't have vision.md yet).
func TestReflectKickoffContextRendersHygieneFindings(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	got, err := reflectKickoffContext(root, "tele", cfg)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}
	for _, want := range []string{
		"### Hygiene findings",
		"refuses to ship a reflect with leftover findings",
		"vision.md",
		"architecture.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff context missing %q in:\n%s", want, got)
		}
	}
}

// Clean wiki — the hygiene section is omitted entirely rather than
// printed with an empty body. Same shape as the pre-redesign kickoff.
func TestReflectKickoffContextOmitsEmptyHygieneSection(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeWikiDoc(t, twinDir, "vision.md", "# Vision\n\nReal content.\n"); err != nil {
		t.Fatal(err)
	}

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	got, err := reflectKickoffContext(root, "tele", cfg)
	if err != nil {
		t.Fatalf("reflectKickoffContext: %v", err)
	}
	if strings.Contains(got, "### Hygiene findings") {
		t.Errorf("kickoff context should omit hygiene section when findings empty:\n%s", got)
	}
}

// TestFinalizeStageGateRefusesEmptySections is the anti-theater check
// for finalize. A committed skeleton (the seeded `(...)` placeholders)
// must not advance the stage; substantive content in both load-bearing
// sections must.
func TestFinalizeStageGateRefusesEmptySections(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "reflect-2026-05-14", Project: "tele", Workflow: "twin"}
	canvasRel := run.ContentPath(md.Project, md.ID, "finalize")
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(canvasRel)), 0o755); err != nil {
		t.Fatal(err)
	}
	// Skeleton with placeholders — anti-theater check should refuse.
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(finalizeCanvasSkeleton), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := finalizeStageGate(root, md)
	if err != nil {
		t.Fatalf("finalizeStageGate: %v", err)
	}
	if ok {
		t.Error("finalize gate should refuse a canvas left at the seeded skeleton")
	}

	// Filled — both load-bearing sections have substantive content.
	filled := `# Finalize

## What I fixed

- renamed glossary entry "X" to "Y" to match patterns.md

## What I left

- nothing left

## History-summary delta

- seeded with this pass.
`
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = finalizeStageGate(root, md)
	if err != nil {
		t.Fatalf("finalizeStageGate: %v", err)
	}
	if !ok {
		t.Error("finalize gate should advance with both load-bearing sections filled")
	}
}

// TestFinalizeStageGateRefusesMissingCanvas — a stage that hasn't run
// yet (no canvas on disk) is parked, not erroring. Mirrors test_gate's
// missing-canvas tolerance.
func TestFinalizeStageGateRefusesMissingCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	md := &run.Metadata{ID: "reflect-2026-05-14", Project: "tele", Workflow: "twin"}
	ok, err := finalizeStageGate(root, md)
	if err != nil {
		t.Fatalf("finalizeStageGate: %v", err)
	}
	if ok {
		t.Error("finalize gate should refuse a missing canvas")
	}
}

// TestTwinPriorStageWalksLadderForward pins the per-stage prereq
// lookup behind requireTwinPriorCanvas. Linear ladder, vision is
// first.
func TestTwinPriorStageWalksLadderForward(t *testing.T) {
	cases := []struct {
		stage, want string
	}{
		{"vision", ""},
		{"architecture", "vision"},
		{"patterns", "architecture"},
		{"operations", "patterns"},
		{"roadmap", "operations"},
		{"glossary", "roadmap"},
		{"finalize", "glossary"},
		{"unknown", ""},
	}
	for _, c := range cases {
		if got := twinPriorStage(c.stage); got != c.want {
			t.Errorf("twinPriorStage(%q) = %q, want %q", c.stage, got, c.want)
		}
	}
}

// loadTwinFeedback walks runs in projectID, picks up feedback/twin.md
// for each, and filters by the wiki checkpoint's LastIngestAt against
// the file's latest commit time. Build a small fixture with two runs
// (one fresh, one stale), commit each feedback file at a known time,
// and pin which one surfaces.
func TestLoadTwinFeedbackFiltersByCheckpoint(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Anchor the threshold between the two commits.
	threshold := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	staleAt := threshold.Add(-1 * time.Hour)
	freshAt := threshold.Add(1 * time.Hour)

	writeRunMeta(t, root, "tele", "stale-run", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "stale-run", "twin", "stale note", staleAt)

	writeRunMeta(t, root, "tele", "fresh-run", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "fresh-run", "twin", "fresh note", freshAt)

	// Seed the checkpoint with LastIngestAt = threshold; only freshAt
	// is After(threshold) and should land.
	if err := wiki.WriteCheckpoint(twinDir, wiki.Checkpoint{
		Version:      wiki.CheckpointVersion,
		LastIngestAt: threshold.Format(time.RFC3339),
		Project:      "tele",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs:     []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := loadTwinFeedback(root, "tele", cfg)
	if err != nil {
		t.Fatalf("loadTwinFeedback: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry surface (fresh-run), got %d: %+v", len(got), got)
	}
	if got[0].runID != "fresh-run" {
		t.Errorf("entry runID = %q, want fresh-run", got[0].runID)
	}
	if !strings.Contains(got[0].body, "fresh note") {
		t.Errorf("entry body = %q, want 'fresh note'", got[0].body)
	}
}

// No checkpoint means first reflect: every committed feedback file
// lands regardless of age. Same fixture shape as the filter test, but
// without a checkpoint on disk.
func TestLoadTwinFeedbackNoCheckpointReturnsAll(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	writeRunMeta(t, root, "tele", "alpha", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "alpha", "twin", "alpha note", t0)
	writeRunMeta(t, root, "tele", "beta", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "beta", "twin", "beta note", t0.Add(time.Hour))

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs:     []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := loadTwinFeedback(root, "tele", cfg)
	if err != nil {
		t.Fatalf("loadTwinFeedback: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries (no checkpoint = first reflect), got %d: %+v", len(got), got)
	}
	// Sorted freshest-first.
	if got[0].runID != "beta" || got[1].runID != "alpha" {
		t.Errorf("entries not sorted freshest-first: %q then %q", got[0].runID, got[1].runID)
	}
}

// Feedback for runs that aren't projectID's must not leak in. Pins
// the project-scoping leg of the walk.
func TestLoadTwinFeedbackIgnoresOtherProjects(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	writeRunMeta(t, root, "tele", "ours", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "ours", "twin", "ours note", t0)
	writeRunMeta(t, root, "other", "theirs", "sdlc")
	writeFeedbackAndCommit(t, root, "other", "theirs", "twin", "theirs note", t0)

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs:     []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := loadTwinFeedback(root, "tele", cfg)
	if err != nil {
		t.Fatalf("loadTwinFeedback: %v", err)
	}
	if len(got) != 1 || got[0].runID != "ours" {
		t.Fatalf("expected only the tele-project entry, got %+v", got)
	}
}

// A feedback file on disk that has never been committed is invisible
// to the journal — same contract as closedRunsSince. Guards against
// surfacing a draft that the agent wrote but the stage commit hasn't
// folded in yet.
func TestLoadTwinFeedbackSkipsUncommittedFiles(t *testing.T) {
	root := newTestBureaucracy(t)
	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunMeta(t, root, "tele", "draft", "sdlc")
	rel := run.FeedbackPath("tele", "draft", "twin")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("draft note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: no git add / commit — the file is on disk only.

	cfg := wiki.Config{
		Mode:            wiki.Closed,
		Name:            "twin",
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs:     []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got, err := loadTwinFeedback(root, "tele", cfg)
	if err != nil {
		t.Fatalf("loadTwinFeedback: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries (uncommitted draft invisible), got %+v", got)
	}
}

// writeRunMeta writes a minimal run.json under
// projects/<projectID>/runs/<runID>/ and commits it on main, so the
// run shows up in run.Scan. Workflow rounds out the metadata
// loadTwinFeedback consults for provenance.
func writeRunMeta(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	md := run.Metadata{
		ID: runID, Project: projectID, Status: run.StatusInProgress,
		Workflow: workflow, Created: "2026-05-10",
	}
	body, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	rel := filepath.Join(run.Dir(projectID, runID), "run.json")
	if err := os.MkdirAll(filepath.Join(root, run.Dir(projectID, runID)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), body, 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", rel)
	gittest.Run(t, root, "commit", "-m", "Open run "+projectID+"/"+runID)
}

// writeFeedbackAndCommit writes a feedback note for (projectID,
// runID, recipient) and commits it at `when`, so the file's
// LastFileActivity equals `when` in the journal.
func writeFeedbackAndCommit(t *testing.T, root, projectID, runID, recipient, body string, when time.Time) {
	t.Helper()
	rel := run.FeedbackPath(projectID, runID, recipient)
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", rel)
	trailerstest.CommitTrailer(t, root, "work: add feedback "+recipient,
		"MoE-Run: "+runID+"\nMoE-Project: "+projectID, when)
}

// Post-flight gate: a wiki with leftover findings returns an error
// (so runWikiSession skips FinalizeIngest and CommitStager); a clean
// wiki returns nil.
func TestReflectPostFlightGate(t *testing.T) {
	dir := t.TempDir()
	cfg := wiki.Config{
		Name:       "twin",
		ContentDir: dir,
		Mode:       wiki.Closed,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "patterns.md", Title: "Patterns"},
		},
	}

	var stderr bytes.Buffer
	if err := reflectPostFlightGate(&cfg, &stderr); err == nil {
		t.Fatal("expected gate error on missing managed docs")
	}
	if !strings.Contains(stderr.String(), "leftover hygiene findings") {
		t.Errorf("expected gate stderr to name the failure, got:\n%s", stderr.String())
	}

	// Stub both managed docs with real content so the post-flight
	// scan finds nothing.
	for _, name := range []string{"vision.md", "patterns.md"} {
		if err := writeWikiDoc(t, dir, name, "# "+name+"\n\nReal content.\n"); err != nil {
			t.Fatal(err)
		}
	}
	stderr.Reset()
	if err := reflectPostFlightGate(&cfg, &stderr); err != nil {
		t.Fatalf("clean wiki should pass the gate, got %v\nstderr=%s", err, stderr.String())
	}
}

// TestFindInProgressTwinRunDetectsExisting pins the guard
// reflectCommand uses to refuse opening a second pass while one is
// already in flight.
func TestFindInProgressTwinRunDetectsExisting(t *testing.T) {
	root := newTestBureaucracy(t)
	if got, err := findInProgressTwinRun(root, "tele"); err != nil {
		t.Fatalf("findInProgressTwinRun on empty repo: %v", err)
	} else if got != "" {
		t.Errorf("findInProgressTwinRun on empty repo = %q, want \"\"", got)
	}
	writeRunMeta(t, root, "tele", "reflect-2026-05-14", "twin")
	got, err := findInProgressTwinRun(root, "tele")
	if err != nil {
		t.Fatalf("findInProgressTwinRun: %v", err)
	}
	if got != "reflect-2026-05-14" {
		t.Errorf("findInProgressTwinRun = %q, want reflect-2026-05-14", got)
	}
	// A different project's twin run must not match.
	if other, err := findInProgressTwinRun(root, "other"); err != nil {
		t.Fatalf("findInProgressTwinRun other: %v", err)
	} else if other != "" {
		t.Errorf("findInProgressTwinRun(other) = %q, want \"\"", other)
	}
}
