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

func TestReflectKickoffRendersHistorySummary(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	summary := "The twin was seeded in 2026-Q1; auth rewrite landed in 2026-Q2."
	got := reflectKickoff(cfg, summary, "", nil, nil, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	if !strings.Contains(got, "## History summary") {
		t.Errorf("kickoff missing history summary heading:\n%s", got)
	}
	if !strings.Contains(got, summary) {
		t.Errorf("kickoff missing summary body:\n%s", got)
	}
	if !strings.Contains(got, "updated `history-summary.md`") {
		t.Errorf("kickoff missing closing instruction asking the agent to update the summary:\n%s", got)
	}
}

// When the summary is absent (fresh wiki, or migration from a wiki
// that has a checkpoint but no summary file) the kickoff should still
// render the heading and prompt the agent to seed the file from the
// events block at end of pass.
func TestReflectKickoffFreshSummaryFraming(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got := reflectKickoff(cfg, "", "## Events since last reflect\n\n- abc1234 first commit\n", nil, nil, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	if !strings.Contains(got, "## History summary") {
		t.Errorf("kickoff missing history summary heading:\n%s", got)
	}
	if !strings.Contains(got, "no rolling summary yet") {
		t.Errorf("kickoff missing fresh-summary framing:\n%s", got)
	}
	if !strings.Contains(got, "seed `history-summary.md`") {
		t.Errorf("kickoff should tell the agent to seed history-summary.md:\n%s", got)
	}
	// Events block still rendered alongside the empty summary.
	if !strings.Contains(got, "abc1234 first commit") {
		t.Errorf("kickoff missing events body:\n%s", got)
	}
}

// Idea backlog and hygiene findings are the two synthesis inputs
// that used to live in plan / lint and now ride into reflect's
// kickoff. Pin both: empty inputs collapse silently; populated
// inputs render their named sections.
func TestReflectKickoffRendersIdeaBacklog(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "roadmap.md", Title: "Roadmap"}},
	}
	ideas := []ideaSummary{
		{slug: "fix-auth", title: "Fix auth race", body: "Auth tokens leak under load."},
		{slug: "tidy-cli", title: "Tidy CLI errors", body: "Errors mention internals."},
	}
	got := reflectKickoff(cfg, "", "", ideas, nil, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	for _, want := range []string{
		"## Idea backlog",
		"### fix-auth — Fix auth race",
		"Auth tokens leak under load.",
		"### tidy-cli — Tidy CLI errors",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, got)
		}
	}
}

func TestReflectKickoffRendersHygieneFindings(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	findings := wiki.Findings{
		EmptyDocs:          []string{"patterns.md"},
		MissingManagedDocs: []string{"roadmap.md"},
	}
	got := reflectKickoff(cfg, "", "", nil, nil, findings, "projects/p/runs/r/documents/reflect/content.md")
	for _, want := range []string{
		"## Hygiene findings",
		"refuses to seal a reflect with leftover findings",
		"patterns.md",
		"roadmap.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, got)
		}
	}
}

// Empty hygiene findings should not render the hygiene heading at
// all — a clean wiki shouldn't pad the kickoff with an empty section.
func TestReflectKickoffOmitsEmptyHygieneSection(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got := reflectKickoff(cfg, "", "", nil, nil, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	if strings.Contains(got, "## Hygiene findings") {
		t.Errorf("kickoff should omit hygiene section when findings empty:\n%s", got)
	}
}

// The canvas-write instruction is the load-bearing prompt change for
// the "reflect-is-broken-by-checks" fix: the agent must know to write
// the end-of-pass summary to the per-run canvas, and the path it sees
// in the prompt is the same path commitReflectTurn stages and
// session.Close reads at the branch tip.
func TestReflectKickoffInstructsCanvasWrite(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	canvasRel := "projects/tele/runs/reflect-2026-05-11-120000/documents/reflect/content.md"
	got := reflectKickoff(cfg, "", "", nil, nil, wiki.Findings{}, canvasRel)
	for _, want := range []string{
		"end-of-pass summary",
		canvasRel,
		"refuses to seal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, got)
		}
	}
}

// TestBuildReflectSystemPromptSectionsEndWithNewline pins the same
// trailing-newline contract as TestBuildSystemPromptSectionsEndWithNewline,
// but for buildReflectSystemPrompt's three-section join (soul, twin
// reference, reflect body). Closed-schema only — reflect refuses
// other modes — so the fixture mirrors the closed-schema fixture in
// stage_test.go.
func TestBuildReflectSystemPromptSectionsEndWithNewline(t *testing.T) {
	root := newTestBureaucracy(t)

	twinDir := wiki.TwinDir(root, "tele")
	if err := os.MkdirAll(twinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(twinDir, "vision.md"), []byte("# vision\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wikiCfg := &wiki.Config{
		Name:            "twin",
		Mode:            wiki.Closed,
		ContentDir:      twinDir,
		Project:         "tele",
		BureaucracyPath: root,
		ManagedDocs: []wiki.ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "what this is."},
		},
	}

	got, err := buildReflectSystemPrompt(wikiCfg)
	if err != nil {
		t.Fatal(err)
	}
	assertPromptSectionsEndWithNewline(t, got, 3)
}

// Workflow feedback section: present runs render with provenance; an
// empty feedback list collapses to the "(no workflow feedback …)"
// placeholder. Mirrors the idea-backlog / hygiene shape so the
// section is always present (legible empty state) but doesn't pad
// the kickoff with anything more than one line when there is nothing
// to say.
func TestReflectKickoffRendersWorkflowFeedback(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "patterns.md", Title: "Patterns"}},
	}
	feedback := []twinFeedbackEntry{
		{
			runID:    "fix-auth",
			runTitle: "Fix auth race",
			body:     "patterns.md says X is load-bearing; cli/auth.go:99 no longer matches.",
			when:     time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC),
		},
		{
			runID:    "tidy-cli",
			runTitle: "",
			body:     "Vision claims non-goal Y; this run pushed against it. Flag for operator.",
			when:     time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC),
		},
	}
	got := reflectKickoff(cfg, "", "", nil, feedback, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	for _, want := range []string{
		"## Workflow feedback",
		"### fix-auth — Fix auth race (2026-05-11)",
		"cli/auth.go:99 no longer matches",
		"### tidy-cli — tidy-cli (2026-05-10)", // falls back to runID when title is empty
		"Vision claims non-goal Y",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q in:\n%s", want, got)
		}
	}
}

func TestReflectKickoffEmptyWorkflowFeedbackCollapses(t *testing.T) {
	cfg := wiki.Config{
		Mode:        wiki.Closed,
		Name:        "twin",
		ContentDir:  "/x/projects/p/digital-twin",
		ManagedDocs: []wiki.ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	got := reflectKickoff(cfg, "", "", nil, nil, wiki.Findings{}, "projects/p/runs/r/documents/reflect/content.md")
	if !strings.Contains(got, "## Workflow feedback") {
		t.Errorf("kickoff missing workflow feedback heading on empty input:\n%s", got)
	}
	if !strings.Contains(got, "no workflow feedback since the last reflect") {
		t.Errorf("kickoff missing empty-feedback placeholder:\n%s", got)
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

	writeRunMeta(t, root, "tele", "stale-run", "Stale run", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "stale-run", "twin", "stale note", staleAt)

	writeRunMeta(t, root, "tele", "fresh-run", "Fresh run", "sdlc")
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
	writeRunMeta(t, root, "tele", "alpha", "Alpha", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "alpha", "twin", "alpha note", t0)
	writeRunMeta(t, root, "tele", "beta", "Beta", "sdlc")
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
	writeRunMeta(t, root, "tele", "ours", "Ours", "sdlc")
	writeFeedbackAndCommit(t, root, "tele", "ours", "twin", "ours note", t0)
	writeRunMeta(t, root, "other", "theirs", "Theirs", "sdlc")
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
	writeRunMeta(t, root, "tele", "draft", "Draft", "sdlc")
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
// run shows up in run.Scan. Title and workflow round out the metadata
// loadTwinFeedback consults for provenance.
func writeRunMeta(t *testing.T, root, projectID, runID, title, workflow string) {
	t.Helper()
	md := run.Metadata{
		ID: runID, Project: projectID, Title: title, Status: run.StatusInProgress,
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
