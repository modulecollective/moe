package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	got := reflectKickoff(cfg, summary, "", nil, wiki.Findings{})
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
	got := reflectKickoff(cfg, "", "## Events since last reflect\n\n- abc1234 first commit\n", nil, wiki.Findings{})
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
	got := reflectKickoff(cfg, "", "", ideas, wiki.Findings{})
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
	got := reflectKickoff(cfg, "", "", nil, findings)
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
	got := reflectKickoff(cfg, "", "", nil, wiki.Findings{})
	if strings.Contains(got, "## Hygiene findings") {
		t.Errorf("kickoff should omit hygiene section when findings empty:\n%s", got)
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
