package wiki

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestIngestPromptSectionRendersSchemaRules(t *testing.T) {
	cfg := Config{Name: "twin", ContentDir: "/some/path/projects/p/digital-twin"}
	got := IngestPromptSection(cfg)
	for _, want := range []string{
		"## Wiki: twin",
		"/some/path/projects/p/digital-twin",
		"log.md",
		"checkpoint.json",
		"doc set is fixed",
		"No index.md, no topics/",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestIngestPromptSectionRendersManagedDocSoftBudget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), strings.Repeat("x", 1025))
	writeFile(t, filepath.Join(dir, "patterns.md"), "small")
	cfg := Config{
		Name:       "twin",
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision", SoftBudgetKB: 1},
			{Filename: "patterns.md", Title: "Patterns", SoftBudgetKB: 1},
			{Filename: "operations.md", Title: "Operations"},
		},
	}
	got := IngestPromptSection(cfg)
	for _, want := range []string{
		"vision.md — Vision. (1.0 KB; soft budget 1 KB) ⚠ over budget — compress this pass",
		"patterns.md — Patterns. (0.0 KB; soft budget 1 KB)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("closed-schema prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "operations.md — Operations. (") {
		t.Errorf("doc without a budget should not render a size annotation:\n%s", got)
	}
}

func TestAssertSchemaInvariantsRequiresManagedDocs(t *testing.T) {
	err := assertSchemaInvariantsPreFinalize(Config{})
	if err == nil || !strings.Contains(err.Error(), "ManagedDocs") {
		t.Fatalf("no ManagedDocs should refuse with a managed-docs message, got %v", err)
	}
}

func TestAssertSchemaInvariantsRefusesMissingDoc(t *testing.T) {
	dir := t.TempDir()
	// Only one of two docs present — invariants flag the missing one.
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	err := assertSchemaInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), "architecture.md") {
		t.Fatalf("expected missing-doc error naming architecture.md, got %v", err)
	}
}

func TestAssertSchemaInvariantsBootstrapTolerantOfMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	if err := assertSchemaInvariantsBootstrap(cfg); err != nil {
		t.Fatalf("bootstrap should tolerate missing docs, got %v", err)
	}
}

func TestAssertSchemaInvariantsRefusesUnexpectedDoc(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(dir, "stray.md"), "# Stray\n")
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	err := assertSchemaInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), "stray.md") {
		t.Fatalf("expected unexpected-doc error naming stray.md, got %v", err)
	}
}

func TestAssertSchemaInvariantsRefusesTopicsDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	if err := os.MkdirAll(filepath.Join(dir, topicsSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	err := assertSchemaInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), topicsSubdir) {
		t.Fatalf("expected topics-dir refusal, got %v", err)
	}
}

func TestAssertSchemaInvariantsAllowsHistorySummary(t *testing.T) {
	// All managed docs present plus history-summary.md, no log.md and
	// no checkpoint.json. This is the post-reflect-failure state on
	// disk that today's invariants check rejects with "unexpected
	// top-level doc history-summary.md" — pinning that the rolling
	// summary is engine-aware-allowed alongside log.md.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(dir, "architecture.md"), "# Architecture\n")
	writeFile(t, filepath.Join(dir, historySummaryName), "# History\n\nthings happened\n")
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	if err := assertSchemaInvariantsPreFinalize(cfg); err != nil {
		t.Fatalf("invariants should accept history-summary.md, got %v", err)
	}
}

func TestEnsureManagedDocsCreatesStubs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "twin")
	cfg := Config{
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	stubbed, err := EnsureManagedDocs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !stubbed {
		t.Fatal("expected stubbed=true on first run")
	}
	for _, name := range []string{"vision.md", "architecture.md"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("missing stub %s: %v", name, err)
		}
		if !strings.HasPrefix(string(body), "# ") {
			t.Errorf("stub %s missing title heading: %q", name, body)
		}
	}
	// Second run is a no-op — existing files aren't clobbered.
	if err := os.WriteFile(filepath.Join(dir, "vision.md"), []byte("# Vision\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubbed, err = EnsureManagedDocs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stubbed {
		t.Fatal("expected stubbed=false when all docs present")
	}
	body, err := os.ReadFile(filepath.Join(dir, "vision.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "body") {
		t.Errorf("EnsureManagedDocs clobbered an existing file: %q", body)
	}
}

func TestWriteAndReadCheckpoint(t *testing.T) {
	dir := t.TempDir()
	bSHA, pSHA := "abc123", "def456"
	cp := Checkpoint{
		Version:        CheckpointVersion,
		LastIngestAt:   "2026-04-27T15:30:00Z",
		LastIngestRun:  "wiki-engine",
		BureaucracySHA: &bSHA,
		Project:        "moe",
		ProjectRepoSHA: &pSHA,
	}
	if err := WriteCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("checkpoint should exist after write")
	}
	if got.Version != cp.Version || got.Project != cp.Project ||
		got.LastIngestRun != cp.LastIngestRun || got.LastIngestAt != cp.LastIngestAt {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, cp)
	}
	if got.BureaucracySHA == nil || *got.BureaucracySHA != bSHA {
		t.Fatalf("bureaucracy_sha not preserved: %v", got.BureaucracySHA)
	}
}

func TestReadCheckpointMissing(t *testing.T) {
	dir := t.TempDir()
	got, ok, err := ReadCheckpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("expected missing checkpoint, got %+v", got)
	}
}

func TestCheckpointMarshalsNullSHAs(t *testing.T) {
	cp := Checkpoint{
		Version:       CheckpointVersion,
		LastIngestAt:  "2026-04-27T15:30:00Z",
		LastIngestRun: "wiki-engine",
		Project:       "moe",
		// SHAs intentionally nil
	}
	body, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"bureaucracy_sha":null`) {
		t.Errorf("expected null bureaucracy_sha, got %s", s)
	}
	if !strings.Contains(s, `"project_repo_sha":null`) {
		t.Errorf("expected null project_repo_sha, got %s", s)
	}
}

// finalize_test wires up a real tiny git repo so we exercise the
// `git.Status` path. Same shape as cli's newTestBureaucracy: scoped
// git config, throwaway tempdir, one initial commit. The wiki dir
// lives at <root>/wiki/ and the project repo (when used) is a sibling
// tempdir.

func newGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// twinDocs is the managed-doc set the finalize fixtures declare. The
// engine refuses a finalize whose declared docs aren't on disk, so every
// fixture writes the files it names here.
func twinDocs(names ...string) []ManagedDoc {
	docs := make([]ManagedDoc, 0, len(names))
	for _, n := range names {
		docs = append(docs, ManagedDoc{Filename: n, Title: n})
	}
	return docs
}

func TestFinalizeIngestNoChangesIsNoOp(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	// Managed doc committed, working tree clean — nothing for finalize
	// to record.
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nbody\n")
	gittest.Run(t, root, "add", "twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "seed twin")
	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md"),
	}
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID: "test-run",
		Now:   time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatalf("FinalizeIngest: %v", err)
	}
	if res.LogEntryWritten || res.CheckpointWritten {
		t.Fatalf("expected no writes for empty change set, got %+v", res)
	}
	if _, err := os.Stat(logPath(wikiDir)); err == nil {
		t.Fatal("log.md should not exist after no-op finalize")
	}
	if _, err := os.Stat(checkpointPath(wikiDir)); err == nil {
		t.Fatal("checkpoint.json should not exist after no-op finalize")
	}
}

func TestFinalizeIngestWritesLogAndCheckpoint(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	// Two managed docs authored this turn — both new files. The agent
	// hasn't staged anything yet; finalize is supposed to see them
	// via `git status` regardless.
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nwhat this is for...\n")
	writeFile(t, filepath.Join(wikiDir, "architecture.md"), "# Architecture\n\ncomponents...\n")

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md", "architecture.md"),
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	var stderr bytes.Buffer
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID:    "wiki-engine",
		RunTitle: "Wiki engine — applied to the twin",
		Now:      now,
	}, &stderr)
	if err != nil {
		t.Fatalf("FinalizeIngest: %v", err)
	}
	if !res.LogEntryWritten || !res.CheckpointWritten {
		t.Fatalf("expected writes, got %+v", res)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("expected 2 changes, got %+v", res.Changes)
	}
	for _, c := range res.Changes {
		if c.Status != Added {
			t.Fatalf("expected Added for %s, got %s", c.Path, c.Status)
		}
	}

	// Log entry includes the dated H2, the run id, the run title, and
	// the per-status bullets. Check shape, not exact bytes — the
	// changelog format is allowed to evolve as long as the heading and
	// run-id remain greppable.
	logBody, err := os.ReadFile(logPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Changelog",
		"## 2026-04-27 — wiki-engine",
		"_Wiki engine — applied to the twin_",
		"added: architecture.md, vision.md",
	} {
		if !strings.Contains(string(logBody), want) {
			t.Errorf("log.md missing %q:\n%s", want, logBody)
		}
	}

	cp, ok, err := ReadCheckpoint(wikiDir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("checkpoint should be present after finalize")
	}
	if cp.Version != CheckpointVersion {
		t.Errorf("checkpoint version=%d want %d", cp.Version, CheckpointVersion)
	}
	if cp.LastIngestRun != "wiki-engine" {
		t.Errorf("checkpoint last_ingest_run=%q want %q", cp.LastIngestRun, "wiki-engine")
	}
	if cp.LastIngestAt != "2026-04-27T15:30:00Z" {
		t.Errorf("checkpoint last_ingest_at=%q want %q", cp.LastIngestAt, "2026-04-27T15:30:00Z")
	}
	if cp.Project != "p" {
		t.Errorf("checkpoint project=%q want %q", cp.Project, "p")
	}
	if cp.BureaucracySHA == nil || *cp.BureaucracySHA == "" {
		t.Errorf("checkpoint bureaucracy_sha should be captured: %v", cp.BureaucracySHA)
	}
	if cp.ProjectRepoSHA != nil {
		t.Errorf("checkpoint project_repo_sha should be nil when ProjectRepoPath empty, got %v", cp.ProjectRepoSHA)
	}
}

func TestFinalizeIngestSkipsManagedFiles(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	// Only engine-managed files dirty in the working tree. That's
	// not a real scenario in production (the engine writes them
	// during finalize) but it lets us assert excludeManaged works.
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nbody\n")
	gittest.Run(t, root, "add", "twin/vision.md")
	gittest.Run(t, root, "commit", "-m", "seed twin")
	writeFile(t, filepath.Join(wikiDir, "log.md"), "# Changelog\n")
	writeFile(t, filepath.Join(wikiDir, "checkpoint.json"), "{}\n")

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md"),
	}
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID: "test",
		Now:   time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.LogEntryWritten || res.CheckpointWritten {
		t.Fatalf("expected no writes when only managed files differ, got %+v", res)
	}
}

func TestFinalizeIngestRecordsProjectRepoSHA(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nbody\n")

	// A separate clean git repo standing in for the project's
	// submodule checkout. capturedSHA reads HEAD here.
	projectRepo := newGitRepo(t)

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		ProjectRepoPath: projectRepo,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md"),
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	if _, err := FinalizeIngest(cfg, FinalizeContext{RunID: "r", Now: now}, nil); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := ReadCheckpoint(wikiDir)
	if err != nil || !ok {
		t.Fatalf("read checkpoint: ok=%v err=%v", ok, err)
	}
	if cp.ProjectRepoSHA == nil || *cp.ProjectRepoSHA == "" {
		t.Fatalf("expected project_repo_sha to be captured, got %v", cp.ProjectRepoSHA)
	}
}

func TestFinalizeIngestNullsDirtyProjectRepoSHA(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nbody\n")

	projectRepo := newGitRepo(t)
	// Dirty the project repo's tree so capturedSHA records null.
	writeFile(t, filepath.Join(projectRepo, "stray.txt"), "uncommitted\n")

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		ProjectRepoPath: projectRepo,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md"),
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	var stderr bytes.Buffer
	if _, err := FinalizeIngest(cfg, FinalizeContext{RunID: "r", Now: now}, &stderr); err != nil {
		t.Fatal(err)
	}
	cp, ok, err := ReadCheckpoint(wikiDir)
	if err != nil || !ok {
		t.Fatalf("read checkpoint: ok=%v err=%v", ok, err)
	}
	if cp.ProjectRepoSHA != nil {
		t.Fatalf("expected nil project_repo_sha for dirty repo, got %v", *cp.ProjectRepoSHA)
	}
	if !strings.Contains(stderr.String(), "dirty") {
		t.Errorf("expected dirty-repo warning on stderr, got %q", stderr.String())
	}
}

// TestFinalizeIngestClosedSchemaWithHistorySummary pins the bug this
// run was opened for: a closed-schema reflect pass that touches a
// managed doc and writes history-summary.md must finalize cleanly —
// log.md and checkpoint.json get written, history-summary.md rides
// into the change set as a real ingest output. Before the
// invariants-exemption fix, finalize aborted at the invariant check
// with "unexpected top-level doc history-summary.md", silently
// dropping the checkpoint write and producing the dash's "never
// reflected" misreport.
func TestFinalizeIngestClosedSchemaWithHistorySummary(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nbody\n")
	writeFile(t, filepath.Join(wikiDir, "architecture.md"), "# Architecture\n")
	writeFile(t, filepath.Join(wikiDir, historySummaryName),
		"# History\n\nThe twin was reseeded in 2026-Q2.\n")

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",

		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	res, err := FinalizeIngest(cfg, FinalizeContext{RunID: "reflect-r", Now: now}, nil)
	if err != nil {
		t.Fatalf("FinalizeIngest: %v", err)
	}
	if !res.CheckpointWritten {
		t.Fatalf("expected checkpoint to be written, got %+v", res)
	}
	if !res.LogEntryWritten {
		t.Fatalf("expected log entry to be written, got %+v", res)
	}
	// history-summary.md is a real ingest output — it should appear in
	// the change set so the changelog reflects that the agent edited it.
	var sawSummary bool
	for _, c := range res.Changes {
		if c.Path == historySummaryName {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("expected %s in change set, got %+v", historySummaryName, res.Changes)
	}

	if _, err := os.Stat(checkpointPath(wikiDir)); err != nil {
		t.Errorf("checkpoint.json not on disk: %v", err)
	}
	logBody, err := os.ReadFile(logPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBody), historySummaryName) {
		t.Errorf("log.md missing %s line:\n%s", historySummaryName, logBody)
	}
}

func TestFinalizeIngestPicksUpDeletes(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "twin")
	// Commit the doc set, then retire the rolling summary in the working
	// tree. history-summary.md is the deletable one — the schema
	// invariants require every managed doc to stay on disk.
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(wikiDir, historySummaryName), "# History\n\nold horizons\n")
	gittest.Run(t, root, "add", "twin")
	gittest.Run(t, root, "commit", "-m", "seed twin")
	if err := os.Remove(filepath.Join(wikiDir, historySummaryName)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wikiDir, "vision.md"), "# Vision\n\nrewritten\n")

	cfg := Config{
		Name:            "twin",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		ManagedDocs:     twinDocs("vision.md"),
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	res, err := FinalizeIngest(cfg, FinalizeContext{RunID: "r", Now: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("expected 2 changes (modify + remove), got %+v", res.Changes)
	}
	logBody, err := os.ReadFile(logPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBody), "modified: vision.md") {
		t.Errorf("log.md missing modify line:\n%s", logBody)
	}
	if !strings.Contains(string(logBody), "removed: "+historySummaryName) {
		t.Errorf("log.md missing remove line:\n%s", logBody)
	}
}
