package wiki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanClosedSchema(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n\nthe bet\n")
	// architecture.md missing → MissingManagedDocs
	// patterns.md present but stub → EmptyDocs
	writeFile(t, filepath.Join(twinDir, "patterns.md"), "# Patterns\n")
	// operations.md links to a missing sibling → BrokenLinks
	writeFile(t, filepath.Join(twinDir, "operations.md"),
		"# Operations\n\nSee [missing](missing.md) for details.\n")

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
			{Filename: "patterns.md", Title: "Patterns"},
			{Filename: "operations.md", Title: "Operations"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.MissingManagedDocs, []string{"architecture.md"}; !equalStrings(got, want) {
		t.Errorf("MissingManagedDocs: got %v want %v", got, want)
	}
	if got, want := f.EmptyDocs, []string{"patterns.md"}; !equalStrings(got, want) {
		t.Errorf("EmptyDocs: got %v want %v", got, want)
	}
	if len(f.BrokenLinks) != 1 || f.BrokenLinks[0].From != "operations.md" || f.BrokenLinks[0].Target != "missing.md" {
		t.Errorf("BrokenLinks: got %+v", f.BrokenLinks)
	}
	// No orphans / index — closed-schema doesn't have the concept.
	if len(f.Orphans) != 0 || len(f.MissingFromIndex) != 0 {
		t.Errorf("closed-schema scan should not surface open-schema fields: %+v", f)
	}
}

func TestScanClosedSchemaFootnotes(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	// Reference one valid run (real dir) and one broken (no dir).
	if err := os.MkdirAll(filepath.Join(root, "projects", "p", "runs", "real-run"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(twinDir, "architecture.md"),
		"# Architecture\n\nThe load-bearing decision[^real-run] still stands.\n"+
			"An older claim[^missing-run] cited a run that no longer exists.\n\n"+
			"[^real-run]: Run real-run, 2026-04-27.\n"+
			"[^missing-run]: Run missing-run, 2026-04-20.\n")
	writeFile(t, filepath.Join(twinDir, "patterns.md"), "# Patterns\n\nbody\n")
	writeFile(t, filepath.Join(twinDir, "operations.md"), "# Operations\n\nbody\n")

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
			{Filename: "patterns.md", Title: "Patterns"},
			{Filename: "operations.md", Title: "Operations"},
		},
	}
	f, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.BrokenFootnotes) != 1 {
		t.Fatalf("expected 1 broken footnote, got %+v", f.BrokenFootnotes)
	}
	if f.BrokenFootnotes[0].From != "architecture.md" || f.BrokenFootnotes[0].RunID != "missing-run" {
		t.Errorf("BrokenFootnotes: got %+v", f.BrokenFootnotes)
	}
}

func TestReflectPromptSectionRefusesOpen(t *testing.T) {
	if _, err := ReflectPromptSection(Config{Mode: Open}); err == nil {
		t.Fatal("ReflectPromptSection should refuse open-schema")
	}
}

func TestReflectPromptSectionRendersClosed(t *testing.T) {
	got, err := ReflectPromptSection(Config{
		Mode:       Closed,
		Name:       "twin",
		ContentDir: "/x/projects/p/digital-twin",
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "north star"},
			{Filename: "architecture.md", Title: "Architecture", Purpose: "shape"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Wiki: twin (closed-schema)",
		"vision.md — Vision",
		"Reflect pass (closed-schema)",
		"`moe twin claim`",
		"[^run-id]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reflect prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestClaimPromptSectionRefusesOpen(t *testing.T) {
	if _, err := ClaimPromptSection(Config{Mode: Open}); err == nil {
		t.Fatal("ClaimPromptSection should refuse open-schema")
	}
}

func TestClaimPromptSectionRendersClosed(t *testing.T) {
	got, err := ClaimPromptSection(Config{
		Mode:       Closed,
		Name:       "twin",
		ContentDir: "/x/projects/p/digital-twin",
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision", Purpose: "north star"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Claim pass (closed-schema)",
		"_For:",
		"log.md",
		"do not edit",
	} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("claim prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestDetectUnrecordedEditsNoCheckpoint(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	det, err := DetectUnrecordedEdits(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.UnrecordedDocs) != 0 {
		t.Errorf("expected no unrecorded edits without checkpoint, got %v", det.UnrecordedDocs)
	}
}

func TestDetectUnrecordedEditsFlagsPostCheckpointEdits(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(twinDir, "architecture.md"), "# Architecture\n")

	// First commit: both files seeded with deterministic timestamps so
	// the last_ingest_at threshold can sit strictly between the two
	// per-file edits.
	t.Setenv("GIT_AUTHOR_DATE", "2026-04-01T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-04-01T12:00:00Z")
	gitInRepo(t, root, "add", "projects/p/digital-twin/vision.md", "projects/p/digital-twin/architecture.md")
	gitInRepo(t, root, "commit", "-m", "seed twin")

	// Second commit: only vision.md edited, with a later timestamp.
	t.Setenv("GIT_AUTHOR_DATE", "2026-04-02T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-04-02T12:00:00Z")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n\nupdated bet\n")
	gitInRepo(t, root, "add", "projects/p/digital-twin/vision.md")
	gitInRepo(t, root, "commit", "-m", "operator edits vision")

	// Checkpoint sits between the two commits. architecture.md's last
	// commit is at 2026-04-01 (before threshold → recorded); vision.md's
	// at 2026-04-02 (after threshold → unrecorded).
	cp := Checkpoint{
		Version:       CheckpointVersion,
		LastIngestAt:  "2026-04-01T18:00:00Z",
		LastIngestRun: "claim-test",
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
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	det, err := DetectUnrecordedEdits(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.UnrecordedDocs) != 1 || det.UnrecordedDocs[0] != "vision.md" {
		t.Errorf("expected vision.md as unrecorded, got %v", det.UnrecordedDocs)
	}
}

func TestTwinReferenceSectionEmptyWithoutDir(t *testing.T) {
	root := t.TempDir()
	got := TwinReferenceSectionAt(root, "p")
	if got != "" {
		t.Errorf("expected empty for missing twin dir, got %q", got)
	}
}

func TestTwinReferenceSectionRendersWithDocs(t *testing.T) {
	root := t.TempDir()
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	got := TwinReferenceSectionAt(root, "p")
	for _, want := range []string{
		"## Project digital twin",
		twinDir,
		"vision.md",
		"architecture.md",
		"patterns.md",
		"operations.md",
		"`moe twin claim`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("twin reference missing %q in:\n%s", want, got)
		}
	}
}

func TestFinalizeClaimAdvancesCheckpoint(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")
	// Pre-populate log.md with a header line — the agent's "synthesis"
	// in the test harness is just any non-managed-doc edit.
	writeFile(t, filepath.Join(twinDir, "log.md"), "# Changelog\n\n## 2026-04-29 — claim-...\n_handoff_\n\nbody\n")

	cfg := Config{
		Mode:            Closed,
		ContentDir:      twinDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     []ManagedDoc{{Filename: "vision.md", Title: "Vision"}},
	}
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID: "claim-test",
		Now:   now,
		Claim: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.CheckpointWritten {
		t.Fatal("claim finalize should always advance checkpoint")
	}
	if res.LogEntryWritten {
		t.Fatal("claim finalize should not append a log entry — agent did")
	}
	cp, ok, err := ReadCheckpoint(twinDir)
	if err != nil || !ok {
		t.Fatalf("expected checkpoint after claim, ok=%v err=%v", ok, err)
	}
	if cp.LastIngestRun != "claim-test" || !strings.HasPrefix(cp.LastIngestAt, "2026-04-29") {
		t.Errorf("checkpoint not advanced: %+v", cp)
	}
}
