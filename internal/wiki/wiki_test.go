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

func TestIngestPromptSectionOpenSchema(t *testing.T) {
	cfg := Config{
		Name:              "kb",
		ContentDir:        "/some/path/projects/p/kb",
		Mode:              Open,
		IngestPrompt:      "Work the new sources into the wiki.",
		AllowedPrimitives: []string{"split", "merge", "rename", "retire"},
	}
	got := IngestPromptSection(cfg)
	for _, want := range []string{
		"Work the new sources into the wiki.",
		"## Wiki: kb (open-schema)",
		"/some/path/projects/p/kb",
		"index.md",
		"log.md",
		"checkpoint.json",
		"open-schema",
		// Per-primitive rubric — each primitive gets a labelled bullet
		// with criteria, evidence, and a "not for X" guard.
		"**split**",
		"**merge**",
		"**rename**",
		"**retire**",
		// [wiki-op] tag convention is surfaced with the literal shapes
		// the parser recognises.
		"[wiki-op] split",
		"[wiki-op] merge",
		"[wiki-op] rename",
		"[wiki-op] retire",
		// Stash file path the agent appends to; absolute, derived from
		// ContentDir.
		"/some/path/projects/p/kb/.wiki-ops",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "closed-schema") {
		t.Errorf("open-schema prompt leaked closed-schema text:\n%s", got)
	}
}

func TestIngestPromptSectionClosedSchema(t *testing.T) {
	cfg := Config{Name: "twin", Mode: Closed}
	got := IngestPromptSection(cfg)
	if !strings.Contains(got, "(closed-schema)") {
		t.Errorf("closed-schema prompt missing label:\n%s", got)
	}
	if !strings.Contains(got, "doc set is fixed") {
		t.Errorf("closed-schema prompt missing rules:\n%s", got)
	}
	if strings.Contains(got, "split a topic doc") {
		t.Errorf("closed-schema prompt leaked open-schema text:\n%s", got)
	}
}

func TestAssertModeInvariantsOpenIsNoOp(t *testing.T) {
	if err := assertModeInvariantsPreFinalize(Config{Mode: Open}); err != nil {
		t.Fatalf("open-schema invariants: unexpected error %v", err)
	}
}

func TestAssertModeInvariantsClosedRequiresManagedDocs(t *testing.T) {
	err := assertModeInvariantsPreFinalize(Config{Mode: Closed})
	if err == nil || !strings.Contains(err.Error(), "ManagedDocs") {
		t.Fatalf("closed-schema with no ManagedDocs should refuse with a managed-docs message, got %v", err)
	}
}

func TestAssertModeInvariantsClosedRefusesMissingDoc(t *testing.T) {
	dir := t.TempDir()
	// Only one of two docs present — invariants flag the missing one.
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	err := assertModeInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), "architecture.md") {
		t.Fatalf("expected missing-doc error naming architecture.md, got %v", err)
	}
}

func TestAssertModeInvariantsClosedBootstrapTolerantOfMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	if err := assertModeInvariantsBootstrap(cfg); err != nil {
		t.Fatalf("bootstrap should tolerate missing docs, got %v", err)
	}
}

func TestAssertModeInvariantsClosedRefusesUnexpectedDoc(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	writeFile(t, filepath.Join(dir, "stray.md"), "# Stray\n")
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	err := assertModeInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), "stray.md") {
		t.Fatalf("expected unexpected-doc error naming stray.md, got %v", err)
	}
}

func TestAssertModeInvariantsClosedRefusesTopicsDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vision.md"), "# Vision\n")
	if err := os.MkdirAll(filepath.Join(dir, topicsSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
		},
	}
	err := assertModeInvariantsPreFinalize(cfg)
	if err == nil || !strings.Contains(err.Error(), topicsSubdir) {
		t.Fatalf("expected topics-dir refusal, got %v", err)
	}
}

func TestAssertModeInvariantsClosedAllowsHistorySummary(t *testing.T) {
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
		Mode:       Closed,
		ContentDir: dir,
		ManagedDocs: []ManagedDoc{
			{Filename: "vision.md", Title: "Vision"},
			{Filename: "architecture.md", Title: "Architecture"},
		},
	}
	if err := assertModeInvariantsPreFinalize(cfg); err != nil {
		t.Fatalf("invariants should accept history-summary.md, got %v", err)
	}
}

func TestEnsureManagedDocsCreatesStubs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "twin")
	cfg := Config{
		Mode:       Closed,
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

func TestEnsureManagedDocsOpenSchemaIsNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kb")
	stubbed, err := EnsureManagedDocs(Config{Mode: Open, ContentDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stubbed {
		t.Fatal("open-schema should not stub anything")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("open-schema EnsureManagedDocs should not create the dir, got err=%v", err)
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

func TestFinalizeIngestNoChangesIsNoOp(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "kb")
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
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
	wikiDir := filepath.Join(root, "kb")
	// Two articles authored this turn — both new files. The agent
	// hasn't staged anything yet; finalize is supposed to see them
	// via `git status` regardless.
	writeFile(t, filepath.Join(wikiDir, "dns-basics.md"), "# DNS basics\n\nresolvers...\n")
	writeFile(t, filepath.Join(wikiDir, "index.md"), "# kb\n\n- DNS\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	var stderr bytes.Buffer
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID:    "wiki-engine",
		RunTitle: "Wiki engine — applied to kb",
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
		"_Wiki engine — applied to kb_",
		"added: dns-basics.md, index.md",
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
	wikiDir := filepath.Join(root, "kb")
	// Only engine-managed files dirty in the working tree. That's
	// not a real scenario in production (the engine writes them
	// during finalize) but it lets us assert excludeManaged works.
	writeFile(t, filepath.Join(wikiDir, "log.md"), "# Changelog\n")
	writeFile(t, filepath.Join(wikiDir, "checkpoint.json"), "{}\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
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
	wikiDir := filepath.Join(root, "kb")
	writeFile(t, filepath.Join(wikiDir, "topic.md"), "body\n")

	// A separate clean git repo standing in for the project's
	// submodule checkout. capturedSHA reads HEAD here.
	projectRepo := newGitRepo(t)

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		ProjectRepoPath: projectRepo,
		Project:         "p",
		Mode:            Open,
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
	wikiDir := filepath.Join(root, "kb")
	writeFile(t, filepath.Join(wikiDir, "topic.md"), "body\n")

	projectRepo := newGitRepo(t)
	// Dirty the project repo's tree so capturedSHA records null.
	writeFile(t, filepath.Join(projectRepo, "stray.txt"), "uncommitted\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		ProjectRepoPath: projectRepo,
		Project:         "p",
		Mode:            Open,
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

func TestParseOpsRecognisesAllPrimitives(t *testing.T) {
	body := `[wiki-op] split networking.md → dns-basics.md, tcp-handshake.md
[wiki-op] merge dns-caching.md into dns-basics.md
[wiki-op] rename old-stuff.md → archived-projects.md
[wiki-op] retire scratch-notes.md
`
	ops := parseOps(body)
	if len(ops) != 4 {
		t.Fatalf("expected 4 ops, got %d: %+v", len(ops), ops)
	}
	if ops[0].Kind != opSplit ||
		len(ops[0].Sources) != 1 || ops[0].Sources[0] != "networking.md" ||
		len(ops[0].Targets) != 2 ||
		ops[0].Targets[0] != "dns-basics.md" || ops[0].Targets[1] != "tcp-handshake.md" {
		t.Errorf("split op malformed: %+v", ops[0])
	}
	if ops[1].Kind != opMerge ||
		len(ops[1].Sources) != 1 || ops[1].Sources[0] != "dns-caching.md" ||
		len(ops[1].Targets) != 1 || ops[1].Targets[0] != "dns-basics.md" {
		t.Errorf("merge op malformed: %+v", ops[1])
	}
	if ops[2].Kind != opRename ||
		ops[2].Sources[0] != "old-stuff.md" || ops[2].Targets[0] != "archived-projects.md" {
		t.Errorf("rename op malformed: %+v", ops[2])
	}
	if ops[3].Kind != opRetire ||
		ops[3].Sources[0] != "scratch-notes.md" || len(ops[3].Targets) != 0 {
		t.Errorf("retire op malformed: %+v", ops[3])
	}
}

func TestParseOpsAcceptsAsciiArrow(t *testing.T) {
	// Operators on keyboards without → should not be locked out.
	ops := parseOps("[wiki-op] rename old.md -> new.md\n")
	if len(ops) != 1 || ops[0].Kind != opRename ||
		ops[0].Sources[0] != "old.md" || ops[0].Targets[0] != "new.md" {
		t.Fatalf("ascii arrow rename not parsed: %+v", ops)
	}
}

func TestParseOpsSkipsMalformedAndCommentary(t *testing.T) {
	body := `# random commentary line, not a tag
[wiki-op] split   <-- missing arrow + targets
[wiki-op] not-a-real-primitive thing
[wiki-op] retire
[wiki-op]
[wiki-op] retire kept.md
plain text without a tag prefix
`
	ops := parseOps(body)
	if len(ops) != 1 || ops[0].Kind != opRetire || ops[0].Sources[0] != "kept.md" {
		t.Fatalf("expected only the well-formed retire to survive, got %+v", ops)
	}
}

func TestEnsureOpsStashCreatesAndTruncates(t *testing.T) {
	dir := t.TempDir()
	wikiDir := filepath.Join(dir, "kb")

	// Fresh content dir doesn't exist yet — EnsureOpsStash must mkdir it.
	if err := EnsureOpsStash(wikiDir); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	stash := opsStashPath(wikiDir)
	body, err := os.ReadFile(stash)
	if err != nil {
		t.Fatalf("read after first seed: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("expected empty stash on fresh seed, got %q", body)
	}

	// Pre-populate with content from a prior session and re-seed; the
	// agent should land on an empty file again.
	if err := os.WriteFile(stash, []byte("[wiki-op] retire stale.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOpsStash(wikiDir); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	body, err = os.ReadFile(stash)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("expected empty stash after re-seed, got %q", body)
	}
}

func TestFinalizeIngestRendersOperationsGroup(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "kb")
	// Realistic shape: the agent split a doc and added a brand-new one.
	// log.md should carry both an operations group (from .wiki-ops) and
	// the deterministic content-edit list (from git status).
	writeFile(t, filepath.Join(wikiDir, "dns-basics.md"), "# DNS\n")
	writeFile(t, filepath.Join(wikiDir, "tcp-handshake.md"), "# TCP\n")
	writeFile(t, filepath.Join(wikiDir, "tls-handshake.md"), "# TLS\n")
	writeFile(t, filepath.Join(wikiDir, ".wiki-ops"),
		"[wiki-op] split networking.md → dns-basics.md, tcp-handshake.md\n"+
			"[wiki-op] retire scratch-notes.md\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID:    "wiki-engine",
		RunTitle: "Wiki engine — applied to kb",
		Now:      now,
	}, nil)
	if err != nil {
		t.Fatalf("FinalizeIngest: %v", err)
	}
	if !res.LogEntryWritten {
		t.Fatalf("expected log entry, got %+v", res)
	}
	// .wiki-ops must not show up as a content change.
	for _, c := range res.Changes {
		if c.Path == ".wiki-ops" {
			t.Fatalf("stash file leaked into change set: %+v", res.Changes)
		}
	}

	logBody, err := os.ReadFile(logPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	logS := string(logBody)
	// Operations group bullets, exactly as the rubric dictates.
	for _, want := range []string{
		"- split: networking.md → dns-basics.md, tcp-handshake.md",
		"- retire: scratch-notes.md",
		"- added: dns-basics.md, tcp-handshake.md, tls-handshake.md",
	} {
		if !strings.Contains(logS, want) {
			t.Errorf("log.md missing %q:\n%s", want, logS)
		}
	}
	// Operations group sits above the content-edit group.
	if iSplit := strings.Index(logS, "- split:"); iSplit < 0 {
		t.Fatalf("log.md missing operations group:\n%s", logS)
	} else if iAdded := strings.Index(logS, "- added:"); iAdded < 0 || iSplit > iAdded {
		t.Fatalf("operations should appear above content edits:\n%s", logS)
	}

	// Stash file must be truncated to zero bytes by finalize so the
	// next session starts fresh; the truncation rides into the
	// per-turn commit alongside log.md.
	stashBody, err := os.ReadFile(opsStashPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(stashBody) != 0 {
		t.Errorf("stash should be truncated by finalize, got %q", stashBody)
	}
}

func TestFinalizeIngestStashOnlyIsNoOp(t *testing.T) {
	// A session where the agent appended `[wiki-op]` lines but then
	// reverted all topic-doc edits should not produce a log entry —
	// the stash being non-empty isn't a wiki edit on its own. The
	// stash gets truncated as a side effect, which is fine.
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "kb")
	writeFile(t, filepath.Join(wikiDir, ".wiki-ops"), "[wiki-op] retire stale.md\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
	}
	res, err := FinalizeIngest(cfg, FinalizeContext{
		RunID: "test",
		Now:   time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.LogEntryWritten || res.CheckpointWritten {
		t.Fatalf("expected no-op when only stash differs, got %+v", res)
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
		Mode:            Closed,
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
	wikiDir := filepath.Join(root, "kb")
	// Commit a starter doc, then delete it in the working tree.
	writeFile(t, filepath.Join(wikiDir, "old.md"), "to retire\n")
	gittest.Run(t, root, "add", "kb/old.md")
	gittest.Run(t, root, "commit", "-m", "seed kb")
	if err := os.Remove(filepath.Join(wikiDir, "old.md")); err != nil {
		t.Fatal(err)
	}
	// And add a new one.
	writeFile(t, filepath.Join(wikiDir, "new.md"), "incoming\n")

	cfg := Config{
		Name:            "kb",
		ContentDir:      wikiDir,
		BureaucracyPath: root,
		Project:         "p",
		Mode:            Open,
	}
	now := time.Date(2026, 4, 27, 15, 30, 0, 0, time.UTC)
	res, err := FinalizeIngest(cfg, FinalizeContext{RunID: "r", Now: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("expected 2 changes (add + remove), got %+v", res.Changes)
	}
	logBody, err := os.ReadFile(logPath(wikiDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBody), "added: new.md") {
		t.Errorf("log.md missing add line:\n%s", logBody)
	}
	if !strings.Contains(string(logBody), "removed: old.md") {
		t.Errorf("log.md missing remove line:\n%s", logBody)
	}
}
