package trailerstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestCommitTrailer_SubjectAndBackdate confirms CommitTrailer lands the
// requested subject, a blank-line-separated trailer block, and (when
// when is non-zero) the requested commit date.
func TestCommitTrailer_SubjectAndBackdate(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")

	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	CommitTrailer(t, root, "do thing", "MoE-Run: r\nMoE-Project: p", when)

	subject := gittest.Output(t, root, "log", "-1", "--format=%s")
	if subject != "do thing" {
		t.Fatalf("subject = %q, want %q", subject, "do thing")
	}
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(body, "do thing\n\nMoE-Run: r\nMoE-Project: p") {
		t.Fatalf("commit body missing trailer block:\n%s", body)
	}
	date := gittest.Output(t, root, "log", "-1", "--format=%aI")
	if !strings.HasPrefix(date, "2026-01-02T03:04:05") {
		t.Fatalf("author date = %q, want backdated to 2026-01-02T03:04:05", date)
	}
}

// TestCommitTrailer_ZeroTimeUsesNow confirms a zero when leaves the
// commit's author date untouched (live clock, not 1970-01-01).
func TestCommitTrailer_ZeroTimeUsesNow(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")
	before := time.Now().Add(-2 * time.Second)

	CommitTrailer(t, root, "subj", "MoE-Run: r", time.Time{})

	date := gittest.Output(t, root, "log", "-1", "--format=%aI")
	got, err := time.Parse(time.RFC3339, date)
	if err != nil {
		t.Fatalf("parse %q: %v", date, err)
	}
	if got.Before(before) {
		t.Fatalf("author date %s precedes pre-call time %s", got, before)
	}
}

// TestCommitWorkTurnAt_TrailerBlockAndSHA confirms CommitWorkTurnAt
// renders the canonical Run/Project/Workflow/Document trailer block,
// uses the `work: update <doc>` subject convention, and returns the
// HEAD SHA of the commit it just made.
func TestCommitWorkTurnAt_TrailerBlockAndSHA(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")

	sha := CommitWorkTurnAt(t, root, "proj-x", "run-y", "sdlc", "design", time.Time{})

	if sha != gittest.HeadSHA(t, root) {
		t.Fatalf("returned SHA %q != HEAD %q", sha, gittest.HeadSHA(t, root))
	}
	subject := gittest.Output(t, root, "log", "-1", "--format=%s")
	if subject != "work: update design" {
		t.Fatalf("subject = %q, want %q", subject, "work: update design")
	}
	body := gittest.Output(t, root, "log", "-1", "--format=%B")
	want := "MoE-Run: run-y\nMoE-Project: proj-x\nMoE-Workflow: sdlc\nMoE-Document: design"
	if !strings.HasSuffix(body, want) {
		t.Fatalf("body does not end with canonical block %q:\n%s", want, body)
	}
}

// TestSeedProject_WritesAndCommits confirms SeedProject lands a
// committed project.json under projects/<id>/ and leaves the tree
// clean.
func TestSeedProject_WritesAndCommits(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")

	SeedProject(t, root, "proj-x")

	got, err := os.ReadFile(filepath.Join(root, "projects", "proj-x", "project.json"))
	if err != nil {
		t.Fatalf("read project.json: %v", err)
	}
	if string(got) != `{"id":"proj-x"}` {
		t.Fatalf("project.json = %q", got)
	}
	if gittest.Output(t, root, "status", "--porcelain") != "" {
		t.Fatal("tree should be clean after SeedProject")
	}
}

// TestSeedRun_WritesMetadataAndCommitsWithTrailers confirms SeedRun
// writes run.json + project.json, lands an open-run commit findable by
// `git log --grep=MoE-Run`, and returns the in-memory Metadata so
// callers can assert on it.
func TestSeedRun_WritesMetadataAndCommitsWithTrailers(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")

	md := SeedRun(t, root, "proj-x", "run-y", "sdlc", "open")

	if md.ID != "run-y" || md.Project != "proj-x" || md.Status != "open" {
		t.Fatalf("metadata = %+v, want ID=run-y Project=proj-x Status=open", md)
	}
	runJSON := filepath.Join(root, run.Dir("proj-x", "run-y"), "run.json")
	if _, err := os.Stat(runJSON); err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	projectJSON := filepath.Join(root, "projects", "proj-x", "project.json")
	if _, err := os.Stat(projectJSON); err != nil {
		t.Fatalf("project.json missing: %v", err)
	}
	hits := gittest.Output(t, root, "log", "--grep=MoE-Run: run-y", "--format=%s")
	if !strings.Contains(hits, "Open run proj-x/run-y: T") {
		t.Fatalf("expected open-run commit in trailer-grep output, got:\n%s", hits)
	}
}
