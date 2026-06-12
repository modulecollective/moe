package wiki

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
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

func TestReflectPromptSectionRefusesOpen(t *testing.T) {
	if _, err := reflectPromptSection(Config{Mode: Open}); err == nil {
		t.Fatal("reflectPromptSection should refuse open-schema")
	}
}

func TestReflectPromptSectionRendersClosed(t *testing.T) {
	got, err := reflectPromptSection(Config{
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
		"canvas; the operator confirms it in an interactive reflect",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reflect prompt missing %q in:\n%s", want, got)
		}
	}
}

// The glossary inclusion bar lives in the reflect kickoff prompt, not
// in operations.md. Pin that the convention block surfaces both the
// 2+-doc rule and the code-seam carve-out so the agent applies the
// rule from the prompt verbatim.
func TestReflectPromptSectionRendersGlossaryConvention(t *testing.T) {
	got, err := reflectPromptSection(Config{
		Mode:       Closed,
		Name:       "twin",
		ContentDir: "/x/projects/p/digital-twin",
		ManagedDocs: []ManagedDoc{
			{Filename: "glossary.md", Title: "Glossary", Purpose: "vocabulary"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Glossary convention",
		"alphabetical list",
		"Inclusion bar",
		"2+ twin docs",
		"code seam",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reflect prompt missing %q in:\n%s", want, got)
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

	// architecture.md's latest commit carries a `MoE-Workflow: twin`
	// trailer (recorded). vision.md's latest commit doesn't (operator
	// edit → unrecorded). Trailer presence is the discriminator; commit
	// times are irrelevant.
	gittest.Run(t, root, "add", "projects/p/digital-twin/architecture.md")
	gittest.Run(t, root, "commit", "-m", "reflect updates architecture\n\nMoE-Workflow: twin")

	gittest.Run(t, root, "add", "projects/p/digital-twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "operator edits vision")

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

// TestDetectUnrecordedEditsTrailerOverridesLaterCommitTime pins the
// production failure roadmap-edits diagnosed: FinalizeIngest stamps
// `last_ingest_at = time.Now()`, the per-turn CommitStager that
// follows lands ~1s later, and the next reflect mis-flags the engine's
// own commit. Trailer-based attribution makes that race vanish — a
// twin commit ahead of `last_ingest_at` is still recorded. This test
// pins the override against the exact production failure.
func TestDetectUnrecordedEditsTrailerOverridesLaterCommitTime(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")

	// Engine's commit lands one second after `last_ingest_at`. Under
	// the old timestamp comparison this would trip; the trailer says
	// otherwise.
	t.Setenv("GIT_AUTHOR_DATE", "2026-05-02T12:00:01Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-05-02T12:00:01Z")
	gittest.Run(t, root, "add", "projects/p/digital-twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "reflect updates vision\n\nMoE-Workflow: twin")

	cp := Checkpoint{
		Version:       CheckpointVersion,
		LastIngestAt:  "2026-05-02T12:00:00Z",
		LastIngestRun: "reflect-prior",
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
	det, err := DetectUnrecordedEdits(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.UnrecordedDocs) != 0 {
		t.Errorf("twin-trailered commit should not be flagged even when newer than last_ingest_at, got %v",
			det.UnrecordedDocs)
	}
}

// TestDetectUnrecordedEditsIgnoresNetNoopRevert pins the design
// promise that a post-checkpoint commit reverted by a later commit
// (so the doc's tree state at HEAD matches the checkpoint SHA) does
// NOT trip the unrecorded-edits guardrail. Without this, every revert
// of a managed-doc commit would force the operator through a claim
// pass with nothing to actually record.
func TestDetectUnrecordedEditsIgnoresNetNoopRevert(t *testing.T) {
	root := newGitRepo(t)
	twinDir := filepath.Join(root, "projects", "p", "digital-twin")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n")

	t.Setenv("GIT_AUTHOR_DATE", "2026-04-01T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-04-01T12:00:00Z")
	gittest.Run(t, root, "add", "projects/p/digital-twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "seed twin")

	checkpointSHA := gittest.HeadSHA(t, root)

	t.Setenv("GIT_AUTHOR_DATE", "2026-04-02T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-04-02T12:00:00Z")
	writeFile(t, filepath.Join(twinDir, "vision.md"), "# Vision\n\nedited\n")
	gittest.Run(t, root, "add", "projects/p/digital-twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "edit vision")

	t.Setenv("GIT_AUTHOR_DATE", "2026-04-03T12:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-04-03T12:00:00Z")
	gittest.Run(t, root, "revert", "--no-edit", "HEAD")

	cp := Checkpoint{
		Version:        CheckpointVersion,
		LastIngestAt:   "2026-04-01T18:00:00Z",
		LastIngestRun:  "reflect-prior",
		BureaucracySHA: &checkpointSHA,
		Project:        "p",
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
	det, err := DetectUnrecordedEdits(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(det.UnrecordedDocs) != 0 {
		t.Errorf("net-noop revert should not be flagged as unrecorded, got %v",
			det.UnrecordedDocs)
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
		"glossary.md",
		"twin wins until a reflect pass updates it",
		"`moe-bureaucracy`",
		"`moe-context`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("twin reference missing %q in:\n%s", want, got)
		}
	}
}
