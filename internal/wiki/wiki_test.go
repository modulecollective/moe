package wiki

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"split",
		"retire",
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
	if err := AssertModeInvariants(Config{Mode: Open}); err != nil {
		t.Fatalf("open-schema invariants: unexpected error %v", err)
	}
}

func TestAssertModeInvariantsClosedIsPhase2(t *testing.T) {
	err := AssertModeInvariants(Config{Mode: Closed})
	if err == nil || !strings.Contains(err.Error(), "phase 2") {
		t.Fatalf("closed-schema should refuse with phase-2 message, got %v", err)
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
// `git status --porcelain` path. Same shape as cli's
// newTestBureaucracy: scoped git config, throwaway tempdir, one
// initial commit. The wiki dir lives at <root>/wiki/ and the project
// repo (when used) is a sibling tempdir.

func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\temail=t@example.com\n\tname=T\n[init]\n\tdefaultBranch=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"commit", "--allow-empty", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
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

func gitInRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
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
	if _, err := os.Stat(LogPath(wikiDir)); err == nil {
		t.Fatal("log.md should not exist after no-op finalize")
	}
	if _, err := os.Stat(CheckpointPath(wikiDir)); err == nil {
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
	logBody, err := os.ReadFile(LogPath(wikiDir))
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

func TestFinalizeIngestPicksUpDeletes(t *testing.T) {
	root := newGitRepo(t)
	wikiDir := filepath.Join(root, "kb")
	// Commit a starter doc, then delete it in the working tree.
	writeFile(t, filepath.Join(wikiDir, "old.md"), "to retire\n")
	gitInRepo(t, root, "add", "kb/old.md")
	gitInRepo(t, root, "commit", "-m", "seed kb")
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
	logBody, err := os.ReadFile(LogPath(wikiDir))
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
