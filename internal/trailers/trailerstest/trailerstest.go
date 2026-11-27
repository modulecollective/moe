// Package trailerstest provides test fixture helpers for commits that
// carry the MoE-* trailer block, plus the run/project bureaucracy
// scaffolding those commits annotate. It is the test-only sibling of
// internal/trailers — where internal/trailers renders the canonical
// trailer block for production callers, trailerstest stamps commits
// with that block (and seeds the on-disk run/project state those
// commits point at) for tests, with t.Fatalf as the failure mode and
// zero ceremony at the callsite.
//
// The trailer-block fixture lives next to the trailer seam (not in
// gittest) because every helper here is shaped by the MoE bureaucracy:
// what fields the trailer carries, what files run/project metadata
// produces, which kind each fixture commit stamps. gittest owns the
// lower-level "throwaway git repo" primitives this package builds on.
package trailerstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// CommitTrailer records a `subject` commit in root whose body is the
// given pre-rendered trailers block. When non-zero, when is set as
// both GIT_AUTHOR_DATE and GIT_COMMITTER_DATE so the commit lands on
// the requested date — tests that exercise timeline-shaped behaviour
// (dash banner ordering) need this.
//
// trailers is a raw string for now: ~25 call sites across cli/ pass
// arbitrary trailer combinations by string concatenation. Migrating
// the parameter to trailers.Block is a separate refactor with its own
// scope; see the design doc for this run.
func CommitTrailer(t *testing.T, root, subject, trailers string, when time.Time) {
	t.Helper()
	var env []string
	if !when.IsZero() {
		stamp := when.Format(time.RFC3339)
		env = []string{
			"GIT_AUTHOR_DATE=" + stamp,
			"GIT_COMMITTER_DATE=" + stamp,
		}
	}
	gittest.RunWithEnv(t, root, env, "commit", "--allow-empty", "-m", subject+"\n\n"+trailers+"\n")
}

// JunkSubject is the headline the fixtures below give the moe-written
// commits they stand for. moe reads commits through their trailers
// only, so a meaningless headline is what proves a reader under test
// never parses one: a reader that did would miss every fixture.
const JunkSubject = "x"

// CommitWorkTurnAt records a work-turn commit with the trailers
// commitTurn writes in production (Run/Project/Workflow/Document/Kind),
// dated to when, under JunkSubject. Returns HEAD's SHA so the caller can
// assert it appears in the banner / log output it is exercising.
func CommitWorkTurnAt(t *testing.T, root, projectID, runID, workflow, docID string, when time.Time) string {
	t.Helper()
	commitKind(t, root, projectID, runID, workflow, docID, trailers.KindTurn, when)
	return gittest.HeadSHA(t, root)
}

// CommitAdvanceAt records an advance marker commit with the trailers
// advanceMarker writes in production (Run/Project/Workflow/Document/
// Kind), dated to when, under JunkSubject. Tests use this to drive the
// click-forward timeline the same way CommitWorkTurnAt drives the
// work-turn timeline.
func CommitAdvanceAt(t *testing.T, root, projectID, runID, workflow, docID string, when time.Time) {
	t.Helper()
	commitKind(t, root, projectID, runID, workflow, docID, trailers.KindAdvance, when)
}

func commitKind(t *testing.T, root, projectID, runID, workflow, docID, kind string, when time.Time) {
	t.Helper()
	block := trailers.Block{
		Run:      runID,
		Project:  projectID,
		Workflow: workflow,
		Document: docID,
		Kind:     kind,
	}.String()
	// Block.String() ends in '\n'; CommitTrailer appends its own, so
	// trim the trailing newline to keep the commit body's blank-line
	// separator canonical (subject + "\n\n" + block + "\n").
	CommitTrailer(t, root, JunkSubject, strings.TrimSuffix(block, "\n"), when)
}

// SeedProject writes a minimal project.json so the project-registered
// check in commands like idea/runNew passes. Commits everything
// currently pending (including the bureaucracy.conf marker laid down
// by markBureaucracy) so the tree is clean for commands that refuse
// to run on a dirty working tree.
func SeedProject(t *testing.T, root, projectID string) {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "register project "+projectID)
}

// SeedRun writes a minimal run.json + project.json pair under root so
// scans like moe dash find it, then commits both with MoE-Run /
// MoE-Project trailers so `git log --grep=MoE-Run` resolves the run.
// Returns the in-memory Metadata so callers can assert or mutate it.
func SeedRun(t *testing.T, root, projectID, runID, workflow, status string) *run.Metadata {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "projects", projectID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "projects", projectID, "project.json"),
		[]byte(`{"id":"`+projectID+`"}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{
		ID:        runID,
		Project:   projectID,
		Status:    status,
		Workflow:  workflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	runJSONRel := filepath.Join(run.Dir(projectID, runID), "run.json")
	projectJSONRel := filepath.Join("projects", projectID, "project.json")
	gittest.Run(t, root, "add", runJSONRel, projectJSONRel)
	CommitTrailer(t, root, JunkSubject,
		"MoE-Run: "+runID+"\nMoE-Project: "+projectID+"\nMoE-Kind: "+trailers.KindOpen, time.Time{})
	return md
}
