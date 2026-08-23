package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestCatRegisteredOnEveryWorkflow: every workflow whose runs land a
// run-scoped canvas grows a `cat` subcommand on its group. Skipping a
// workflow drops the shared shape — this test is the tripwire.
func TestCatRegisteredOnEveryWorkflow(t *testing.T) {
	for _, wf := range []string{"idea", "sdlc", "twin", "chat", "pulse"} {
		g, err := LookupGroup(wf)
		if err != nil {
			t.Fatalf("workflow %q not registered as a group: %v", wf, err)
		}
		if g.Lookup("cat") == nil {
			t.Fatalf("workflow %q has no `cat` subcommand registered", wf)
		}
	}
}

// TestSdlcCatPrintsDesignCanvas: a multi-stage workflow with the
// stage named on the command line. The checkout copy of the canvas
// streams to stdout verbatim — no headers, no pager, no decoration.
func TestSdlcCatPrintsDesignCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "design", "# Design body\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/fix-it", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# Design body\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %q", errb.String())
	}
}

// TestCatUnknownProject: missing project surfaces with the design's
// error shape and exit 1, before any run lookup.
func TestCatUnknownProject(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "ghost/fix-it", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on missing project, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such project: ghost") {
		t.Fatalf("expected missing-project error, got: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got: %q", out.String())
	}
}

// TestCatUnknownRun: project exists but no run; exit 1, named slug
// appears in the error.
func TestCatUnknownRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/ghost", "design"}, &bytes.Buffer{}, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on missing run, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such run: tele/ghost") {
		t.Fatalf("expected missing-run error, got: %q", errb.String())
	}
}

// TestCatWrongWorkflow: pointing `moe <wf> cat` at a run that belongs
// to another workflow refuses loudly and points at the right verb.
func TestCatWrongWorkflow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"twin", "cat", "tele/fix-it", "vision"}, &bytes.Buffer{}, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on wrong-workflow, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "fix-it is a sdlc run, use 'moe sdlc cat'") {
		t.Fatalf("expected wrong-workflow error pointing at sdlc, got: %q", errb.String())
	}
}

// seedRetiredRun stands up a run whose workflow is no longer in the
// registry — the kb/hooks/quick/chores runs left on disk after their
// verbs were retired — carrying the named documents both in run.json
// and as directories under documents/, the shape a worked run has.
// Fails loudly if the named workflow *is* registered, so a future
// registration can't quietly turn these tests into something else.
func seedRetiredRun(t *testing.T, root, projectID, runID, workflow string, docs ...string) {
	t.Helper()
	seedRetiredRunUnrecorded(t, root, projectID, runID, workflow)
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		md.Documents[d] = &run.Document{}
		if err := os.MkdirAll(filepath.Join(root, run.DocDir(projectID, runID, d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// seedRetiredRunUnrecorded stands up a retired-workflow run with an
// empty run.json Documents map and nothing under documents/ — what
// `run.New` leaves behind before any stage turn calls EnsureDocument.
// Callers add the doc dirs themselves (writeContent / writeThread) to
// build the metadata↔disk mismatch that hid a real canvas.
func seedRetiredRunUnrecorded(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	if _, err := LookupWorkflow(workflow); err == nil {
		t.Fatalf("seedRetiredRunUnrecorded: %q is registered; pick a genuinely retired workflow", workflow)
	}
	trailerstest.SeedRun(t, root, projectID, runID, workflow, run.StatusMerged)
}

// TestCatRetiredWorkflowServesCanvas: a run whose workflow was retired
// has no verb of its own left to redirect to, so the verb the operator
// typed prints its canvas rather than naming a command the binary
// rejects.
func TestCatRetiredWorkflowServesCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRun(t, root, "tele", "amp-code", "kb", "research", "summarize")
	writeContent(t, root, "tele", "amp-code", "research", "# Research body\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/amp-code", "research"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# Research body\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
	// stdout stays pipe-clean and stderr silent: the operator asked for
	// content, not a note about the workflow's retirement.
	if errb.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %q", errb.String())
	}
}

// TestCatRetiredWorkflowUnknownStage: a wrong stage guess against a
// retired run lists the run's own documents, not the typed verb's
// ladder. `design` is a real sdlc stage and still errors here — the
// run's metadata, not sdlc's, is what the retired arm validates.
func TestCatRetiredWorkflowUnknownStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRun(t, root, "tele", "amp-code", "kb", "research", "summarize")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/amp-code", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on unknown stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such stage: design (have: [research summarize])") {
		t.Fatalf("expected the run's own stages listed, got: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got: %q", out.String())
	}
}

// TestCatRetiredWorkflowEmptyMetadataServesCanvas: run.json records no
// documents at all — the shape of moe/readme-update-chore-covers-docs-
// 2026-06-13 and of every run seeded but never worked — while the
// canvas sits on disk. The stage list comes from documents/, so the
// canvas prints instead of dying with `no such stage: code (have: [])`.
func TestCatRetiredWorkflowEmptyMetadataServesCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRunUnrecorded(t, root, "tele", "amp-code", "chores")
	writeContent(t, root, "tele", "amp-code", "code", "# Code body\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/amp-code", "code"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# Code body\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("expected empty stderr, got: %q", errb.String())
	}
}

// TestCatRetiredWorkflowEmptyMetadataUnknownStage: the same fixture
// with a stage it doesn't carry lists what's on disk, so the operator
// gets a usable next guess rather than an empty set.
func TestCatRetiredWorkflowEmptyMetadataUnknownStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRunUnrecorded(t, root, "tele", "amp-code", "chores")
	writeContent(t, root, "tele", "amp-code", "code", "# Code body\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/amp-code", "design"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on unknown stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such stage: design (have: [code])") {
		t.Fatalf("expected the on-disk documents listed, got: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got: %q", out.String())
	}
}

// TestCatRetiredWorkflowNoDocumentsDir: a retired run with no
// documents/ dir at all carries nothing, and `have: []` says so
// honestly — the nil arm of runDocIDs.
func TestCatRetiredWorkflowNoDocumentsDir(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	seedRetiredRunUnrecorded(t, root, "tele", "amp-code", "chores")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/amp-code", "code"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on unknown stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such stage: code (have: [])") {
		t.Fatalf("expected an empty stage list, got: %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got: %q", out.String())
	}
}

// TestCatUnknownStage: validating <stage> against the workflow's
// registered ladder produces a stable error that names what's
// available.
func TestCatUnknownStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/fix-it", "bogus"}, &bytes.Buffer{}, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on unknown stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no such stage: bogus") {
		t.Fatalf("expected unknown-stage error, got: %q", errb.String())
	}
	for _, want := range []string{"design", "code", "test", "review", "push"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("expected error to list %q, got: %q", want, errb.String())
		}
	}
}

// TestCatNoCanvasYet: run+stage validate but no content.md exists;
// the error names the absolute path and exits 1.
func TestCatNoCanvasYet(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/fix-it", "design"}, &bytes.Buffer{}, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 when canvas missing, got %d; stderr=%q", code, errb.String())
	}
	wantPath := filepath.Join(root, run.ContentPath("tele", "fix-it", "design"))
	if !strings.Contains(errb.String(), "no canvas yet at "+wantPath) {
		t.Fatalf("expected loud no-canvas error naming %s, got: %q", wantPath, errb.String())
	}
}

// TestCatSingleStageDefaultsStage: single-stage workflows accept the
// two-arg form and route to the only registered stage.
func TestCatSingleStageDefaultsStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "chat-1", "chat", run.StatusInProgress)
	writeContent(t, root, "tele", "chat-1", "chat", "# chat canvas\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"chat", "cat", "tele/chat-1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# chat canvas\n" {
		t.Fatalf("unexpected canvas dump: %q", out.String())
	}
}

// TestCatMultiStageRequiresStage: a multi-stage workflow with no
// stage argument is a usage error, not a silent default to the first
// stage in the ladder.
func TestCatMultiStageRequiresStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/fix-it"}, &bytes.Buffer{}, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on missing stage, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "usage: moe sdlc cat") {
		t.Fatalf("expected usage line, got: %q", errb.String())
	}
}

// TestCatSessionWorktreeWins: when a stage session is open the
// worktree copy of the canvas wins over the checkout, so in-flight
// uncommitted agent edits show up.
func TestCatSessionWorktreeWins(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "fix-it", "design", "# stale checkout\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	// Drop a different body in the worktree's canvas — the live
	// session's edits live there, not in the checkout.
	worktreeCanvas := filepath.Join(sess.WorktreePath, run.ContentPath("tele", "fix-it", "design"))
	if err := os.MkdirAll(filepath.Dir(worktreeCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktreeCanvas, []byte("# fresh worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/fix-it", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# fresh worktree\n" {
		t.Fatalf("expected worktree body to win, got: %q", out.String())
	}
}

// TestCatLatestSentinelResolvesMostRecent: `@latest` picks the run in
// (project, workflow) with the freshest journal activity, regardless
// of status.
func TestCatLatestSentinelResolvesMostRecent(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	trailerstest.SeedRun(t, root, "tele", "older", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "newer", "sdlc", run.StatusInProgress)
	writeContent(t, root, "tele", "older", "design", "# older body\n")
	writeContent(t, root, "tele", "newer", "design", "# newer body\n")
	// Stamp activity so `newer` wins the LastActivity sort. Two
	// commits in temporal order — the later one wins.
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "older", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "newer", "sdlc", "design", t0.Add(time.Hour))
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/@latest", "design"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if out.String() != "# newer body\n" {
		t.Fatalf("expected @latest to resolve to newer, got: %q", out.String())
	}
}

// TestCatLatestSentinelEmptyPool: `@latest` against a project with no
// runs in the requested workflow exits 1 with the design's error.
func TestCatLatestSentinelEmptyPool(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var errb bytes.Buffer
	code := Run([]string{"sdlc", "cat", "tele/@latest", "design"}, &bytes.Buffer{}, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on empty @latest pool, got %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "no runs for tele in workflow sdlc") {
		t.Fatalf("expected empty-pool error, got: %q", errb.String())
	}
}
