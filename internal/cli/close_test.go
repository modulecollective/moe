package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/wiki"
)

// seedCloseFixture composes the test setup every close test wants: a
// bureaucracy repo with the marker committed, plus a run seeded via
// seedRun (which also lands project.json). Without this, the marker
// stays untracked and close's clean-tree check refuses.
func seedCloseFixture(t *testing.T, projectID, runID, workflow, status string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	gittest.Run(t, root, "add", "bureaucracy.conf")
	gittest.Run(t, root, "commit", "-m", "mark bureaucracy")
	trailerstest.SeedRun(t, root, projectID, runID, workflow, status)
	return root
}

// TestSDLCCloseRemovesSandboxAndCommits is the happy path: an
// in-progress sdlc run with a sandbox on disk ends up closed, with the
// sandbox gone and a properly trailered close commit on HEAD.
func TestSDLCCloseRemovesSandboxAndCommits(t *testing.T) {
	root := seedCloseFixture(t, "tele", "abandon-me", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// sandbox.Remove tears down the run's worktree directory; we fake
	// it with a plain dir so the test doesn't need a live submodule.
	sandboxPath := sandbox.Path(root, "tele", "abandon-me")
	if err := os.MkdirAll(sandboxPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/abandon-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed sdlc tele/abandon-me") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	if _, err := os.Stat(sandboxPath); !os.IsNotExist(err) {
		t.Fatalf("expected sandbox gone, stat err=%v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "abandon-me", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(body), `"status": "closed"`) {
		t.Fatalf("run.json status not flipped:\n%s", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close sdlc run tele/abandon-me") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: abandon-me",
		"MoE-Project: tele",
		"MoE-Workflow: sdlc",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

// TestSDLCCloseWithoutSandboxIsFine covers the case where the operator
// opened a run but never got as far as `moe sdlc code` — no sandbox
// exists yet. Close should still work; sandbox.Remove is idempotent.
func TestSDLCCloseWithoutSandboxIsFine(t *testing.T) {
	root := seedCloseFixture(t, "tele", "never-opened", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/never-opened"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed sdlc tele/never-opened") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}
}

// TestSDLCCloseRefusesPushed: a pushed run's terminal state is reached
// via GitHub + sync, not by local close. The error must point the
// operator at that path so they don't try to force it.
func TestSDLCCloseRefusesPushed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"pushed", run.StatusPushed},
		{"merged", run.StatusMerged},
		{"closed", run.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "done-"+tc.name, "sdlc", tc.status)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			beforeHead := gitLog(t, root, "-1", "--format=%H")

			var out, errb bytes.Buffer
			code := Run([]string{"sdlc", "close", "--no-edit", "tele/done-" + tc.name}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s, stdout=%q", tc.status, out.String())
			}
			if tc.status == run.StatusPushed {
				// Pushed gets a steering message pointing at sync.
				if !strings.Contains(errb.String(), "moe sync") {
					t.Fatalf("pushed refusal should mention `moe sync`: %q", errb.String())
				}
			} else {
				if !strings.Contains(errb.String(), "already") {
					t.Fatalf("terminal refusal should say 'already': %q", errb.String())
				}
			}

			// No new commit and run.json status unchanged.
			afterHead := gitLog(t, root, "-1", "--format=%H")
			if beforeHead != afterHead {
				t.Fatalf("refused close created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
			}
			body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "done-"+tc.name, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"status": "`+tc.status+`"`) {
				t.Fatalf("run.json status mutated under refusal:\n%s", body)
			}
		})
	}
}

// TestSDLCCloseWorkflowMismatch: `sdlc close` must refuse a run that
// lives in a different workflow, even if the slug matches — the slug
// namespace is shared per project, so this is a real footgun.
func TestSDLCCloseWorkflowMismatch(t *testing.T) {
	root := seedCloseFixture(t, "tele", "not-sdlc", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/not-sdlc"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on workflow mismatch, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "kb run") || !strings.Contains(errb.String(), "not sdlc") {
		t.Fatalf("expected workflow-mismatch error, got: %q", errb.String())
	}
}

// TestSDLCCloseMissingRun: running against a slug that was never
// opened should fail fast, not panic or silently commit.
func TestSDLCCloseMissingRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele") // seedProject commits everything pending, including the marker
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on missing run, stdout=%q", out.String())
	}
	// sdlc close routes through resolveSDLCRunSlug for the
	// promoted-idea fallback, which emits the design/code/review/test "run
	// not found" shape rather than close's prior "does not exist"
	// wording. Same exit-code contract, more consistent wording.
	if !strings.Contains(errb.String(), "sdlc close: run not found: tele/ghost") {
		t.Fatalf("expected run-not-found error, got: %q", errb.String())
	}
}

// TestKBCloseBumpsStatusAndCommits: kb close has no sandbox/branch
// cleanup step — just status flip + trailered commit. Assert both.
func TestKBCloseBumpsStatusAndCommits(t *testing.T) {
	root := seedCloseFixture(t, "tele", "dead-end", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "close", "--no-edit", "tele/dead-end"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed kb tele/dead-end") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}

	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "dead-end", "run.json"))
	if err != nil {
		t.Fatalf("run.json missing: %v", err)
	}
	if !strings.Contains(string(body), `"status": "closed"`) {
		t.Fatalf("run.json status not flipped:\n%s", body)
	}

	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "Close kb run tele/dead-end") {
		t.Fatalf("commit subject wrong:\n%s", head)
	}
	for _, want := range []string{
		"MoE-Run: dead-end",
		"MoE-Project: tele",
		"MoE-Workflow: kb",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("commit missing trailer %q:\n%s", want, head)
		}
	}
}

// TestKBCloseLeavesUnrelatedPathsAlone: kb has no sandbox, so even if
// one exists on disk (e.g., left over from an sdlc run in the same
// project), kb close must not touch it.
func TestKBCloseLeavesUnrelatedPathsAlone(t *testing.T) {
	root := seedCloseFixture(t, "tele", "kb-run", "kb", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Sibling sdlc sandbox that must survive the kb close.
	sibling := sandbox.Path(root, "tele", "sdlc-run")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "close", "--no-edit", "tele/kb-run"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling sandbox should be untouched: %v", err)
	}
}

// TestKBCloseRefusesTerminal mirrors the sdlc refusal table for kb.
// pushed shouldn't normally happen for kb (no push stage), but if a
// run ends up in that state somehow we still refuse locally rather
// than guess.
func TestKBCloseRefusesTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"merged", run.StatusMerged},
		{"closed", run.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "kb-done-"+tc.name, "kb", tc.status)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			beforeHead := gitLog(t, root, "-1", "--format=%H")
			var out, errb bytes.Buffer
			code := Run([]string{"kb", "close", "--no-edit", "tele/kb-done-" + tc.name}, &out, &errb)
			if code == 0 {
				t.Fatalf("expected non-zero on %s, stdout=%q", tc.status, out.String())
			}
			if !strings.Contains(errb.String(), "already") {
				t.Fatalf("refusal should say 'already': %q", errb.String())
			}
			afterHead := gitLog(t, root, "-1", "--format=%H")
			if beforeHead != afterHead {
				t.Fatalf("refused close created a commit")
			}
		})
	}
}

// TestSDLCCloseRegisteredInUsage: the dispatcher's usage listing is
// what an operator discovers via `moe sdlc`; a wiring regression
// should show up here even if the command itself still works.
func TestSDLCCloseRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "close") {
		t.Fatalf("sdlc usage missing 'close':\n%s", out.String())
	}
}

func TestKBCloseRegisteredInUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"kb"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "close") {
		t.Fatalf("kb usage missing 'close':\n%s", out.String())
	}
}

// addDocEntryAndCommit registers a document on the run's metadata and
// optionally seeds its canvas, then commits so the close clean-tree
// check passes. body == "" leaves the canvas absent — the
// missing-write case gate 2 has to refuse the same way as zero-byte.
func addDocEntryAndCommit(t *testing.T, root, projectID, runID, docID, body string) {
	t.Helper()
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		t.Fatalf("run.Load: %v", err)
	}
	if md.Documents == nil {
		md.Documents = map[string]*run.Document{}
	}
	md.Documents[docID] = &run.Document{Session: "00000000-0000-4000-8000-000000000000"}
	if err := run.Save(root, md); err != nil {
		t.Fatalf("run.Save: %v", err)
	}
	runJSONRel := filepath.Join(run.Dir(projectID, runID), "run.json")
	addArgs := []string{"add", runJSONRel}
	if body != "" {
		canvasRel := run.ContentPath(projectID, runID, docID)
		canvasAbs := filepath.Join(root, canvasRel)
		if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canvasAbs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		addArgs = append(addArgs, canvasRel)
	}
	gittest.Run(t, root, addArgs...)
	gittest.Run(t, root, "commit", "-m", "register "+docID+" on "+runID)
}

// TestSDLCCloseRefusesEmptyDesignCanvas: a registered design document
// with an absent canvas must refuse close. Gate 2's reason for being
// — the silent empty fast-forward this run was opened against would
// land here on disk, and runClose has to catch it before the trailered
// close commit goes in.
func TestSDLCCloseRefusesEmptyDesignCanvas(t *testing.T) {
	root := seedCloseFixture(t, "tele", "empty-design", "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "empty-design", "design", "")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	beforeHead := gitLog(t, root, "-1", "--format=%H")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/empty-design"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "canvas projects/tele/runs/empty-design/documents/design/content.md is empty") {
		t.Fatalf("missing canvas-empty error: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "moe sdlc design tele/empty-design") {
		t.Fatalf("missing reopen hint: %q", errb.String())
	}
	if afterHead := gitLog(t, root, "-1", "--format=%H"); beforeHead != afterHead {
		t.Fatalf("refused close created a commit:\nbefore=%safter=%s", beforeHead, afterHead)
	}
	body, err := os.ReadFile(filepath.Join(root, "projects", "tele", "runs", "empty-design", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status": "in_progress"`) {
		t.Fatalf("status mutated under refusal:\n%s", body)
	}
}

// TestSDLCCloseRefusesEmptyCodeCanvas: design canvas is fine, code is
// the zero-byte one. The walk has to keep checking past the first OK
// document — a regression that bailed early on the first non-empty
// canvas would let this through.
func TestSDLCCloseRefusesEmptyCodeCanvas(t *testing.T) {
	root := seedCloseFixture(t, "tele", "empty-code", "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "empty-code", "design", "# Design\n")
	addDocEntryAndCommit(t, root, "tele", "empty-code", "code", "")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/empty-code"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "documents/code/content.md is empty") {
		t.Fatalf("error should name the code canvas: %q", errb.String())
	}
}

// TestSDLCCloseAllowsNeverStartedCode: design canvas non-empty and the
// code stage was never opened (no entry in md.Documents). Close
// succeeds — the satisfaction model says an unopened doc has no
// canvas-existence obligation.
func TestSDLCCloseAllowsNeverStartedCode(t *testing.T) {
	root := seedCloseFixture(t, "tele", "design-only", "sdlc", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "design-only", "design", "# Design\n")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "close", "--no-edit", "tele/design-only"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed sdlc tele/design-only") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}
}

// TestKBCloseRefusesEmptyCanvas: the gate is shared across workflows;
// kb gets the same refusal, with the kb-shaped reopen hint.
func TestKBCloseRefusesEmptyCanvas(t *testing.T) {
	root := seedCloseFixture(t, "tele", "kb-empty", "kb", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "kb-empty", "research", "")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"kb", "close", "--no-edit", "tele/kb-empty"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "documents/research/content.md is empty") {
		t.Fatalf("kb refusal should name research canvas: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "moe kb research tele/kb-empty") {
		t.Fatalf("kb refusal should suggest the kb verb: %q", errb.String())
	}
}

// TestIntentCloseSkipsHarvest: intent is a capture workflow, so its
// close skips the followups/lore harvest the same way idea's does.
// Regression canary for enterTerminal's isCaptureWorkflow gate — with
// the older `!= dash.IdeaWorkflow` guard, the hand-authored scratch
// files below get harvested (fanned out into an idea run and
// lore/<slug>.md, and rewritten in place), or the unchecked entries pop
// $EDITOR, which EDITOR=false turns into a hard close failure.
func TestIntentCloseSkipsHarvest(t *testing.T) {
	root := seedCloseFixture(t, "tele", "harvest-exempt", "intent", run.StatusInProgress)

	followupsRel := run.FollowupsPath("tele", "harvest-exempt")
	loreRel := run.FeedbackPath("tele", "harvest-exempt", "lore")
	followupsBody := "- [ ] `some-followup` — A follow-up nobody should harvest\n"
	loreBody := "- [ ] `some-fact` — A portable fact nobody should promote\n"
	for rel, body := range map[string]string{followupsRel: followupsBody, loreRel: loreBody} {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "add", rel)
	}
	// Capture close runs requireCleanTree, so the fixture must be
	// committed before the verb runs.
	gittest.Run(t, root, "commit", "-m", "seed scratch files")

	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	// Captures don't expose --no-edit, so close passes skipEdit=false.
	// A false editor makes any pop a loud failure rather than a hang.
	t.Setenv("EDITOR", "false")
	t.Setenv("VISUAL", "false")

	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "close", "tele/harvest-exempt"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	if got, err := os.ReadFile(filepath.Join(root, followupsRel)); err != nil {
		t.Fatalf("followups.md: %v", err)
	} else if string(got) != followupsBody {
		t.Fatalf("followups.md rewritten by harvest:\n%q", string(got))
	}
	if got, err := os.ReadFile(filepath.Join(root, loreRel)); err != nil {
		t.Fatalf("feedback/lore.md: %v", err)
	} else if string(got) != loreBody {
		t.Fatalf("feedback/lore.md rewritten by harvest:\n%q", string(got))
	}

	// Nothing fanned out: no idea run minted from the follow-up, no
	// promoted lore file at the bureaucracy root.
	entries, err := os.ReadDir(filepath.Join(root, "projects", "tele", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "harvest-exempt" {
			t.Fatalf("harvest minted a run: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, wiki.LoreDirRel)); !os.IsNotExist(err) {
		t.Fatalf("harvest promoted lore, stat err=%v", err)
	}

	// The close commit carries the status flip and nothing else.
	touched := gittest.Output(t, root, "show", "--name-only", "--format=", "HEAD")
	want := filepath.Join(run.Dir("tele", "harvest-exempt"), "run.json")
	if got := strings.Fields(touched); len(got) != 1 || got[0] != want {
		t.Fatalf("close commit touched %v, want only %s", got, want)
	}
}

// TestIdeaCloseStillAllowsEmpty: idea is exempt — its content.md is
// the operator's free-form capture, and an empty idea on close is
// operator intent. Regression canary for the workflow != dash.IdeaWorkflow
// branch.
func TestIdeaCloseStillAllowsEmpty(t *testing.T) {
	root := seedCloseFixture(t, "tele", "idea-empty", "idea", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "idea-empty", "idea", "")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"idea", "close", "tele/idea-empty"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "closed idea tele/idea-empty") {
		t.Fatalf("missing close confirmation: %q", out.String())
	}
}

// TestCloseRacesCommitToOrigin: run close is a representative
// journal-writing verb — with an upstream configured, the close commit
// must reach origin before the verb returns (the WithJournalPush
// write-edge), without the operator running `moe sync`.
func TestCloseRacesCommitToOrigin(t *testing.T) {
	root := seedCloseFixture(t, "tele", "push-me", "idea", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "push-me", "idea", "# idea\n")
	origin := gittest.InitBare(t)
	gittest.Run(t, root, "remote", "add", "origin", origin)
	gittest.Run(t, root, "push", "-u", "origin", "main")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "close", "tele/push-me"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if local, remote := gittest.HeadSHA(t, root), gittest.HeadSHA(t, origin); local != remote {
		t.Fatalf("origin main = %s, want close commit %s", remote, local)
	}
}

// TestCloseUnreachableOriginWarnsAndSucceeds: the push leg is
// best-effort. A dead origin costs one stderr line; the verb still
// exits 0 with the commit on local main.
func TestCloseUnreachableOriginWarnsAndSucceeds(t *testing.T) {
	root := seedCloseFixture(t, "tele", "offline", "idea", run.StatusInProgress)
	addDocEntryAndCommit(t, root, "tele", "offline", "idea", "# idea\n")
	origin := gittest.InitBare(t)
	gittest.Run(t, root, "remote", "add", "origin", origin)
	gittest.Run(t, root, "push", "-u", "origin", "main")
	gittest.Run(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	headBefore := gittest.HeadSHA(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "close", "tele/offline"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "[auto-sync skipped]") {
		t.Fatalf("missing warn line, stderr=%q", errb.String())
	}
	if gittest.HeadSHA(t, root) == headBefore {
		t.Fatal("close commit missing from local main")
	}
}
