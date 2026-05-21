package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestSDLCDesignWrongProjectFailsFast: a typo on the project (or run)
// should fail with "run not found" before any per-turn worktree gets
// materialised. Without the pre-flight in runDesign, the failure
// surfaced as a raw filesystem read error from inside the worktree —
// uninformative and harder to recover from.
func TestSDLCDesignWrongProjectFailsFast(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "design", "wrongproj/ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on wrong-project typo, stdout=%q", out.String())
	}
	msg := errb.String()
	if !strings.Contains(msg, "run not found") {
		t.Fatalf("expected run-not-found error, got: %q", msg)
	}
	if !strings.Contains(msg, "wrongproj/ghost") {
		t.Fatalf("expected error to name wrongproj/ghost, got: %q", msg)
	}
}

// TestRequireDesignCanvasRefusesUnchangedKickoffStub: when the design
// canvas at HEAD is byte-identical to the blob at its kickoff commit
// (the `Open run` commit that seeded the stub — or any other "the
// canvas was created and never edited" shape), `sdlc code` must
// refuse with "unchanged from kickoff" so a `!!` cascade can't carry
// downstream stages forward against an unwritten canvas. This is the
// read-side defense in depth behind session.Close's primary gate —
// operators can also commit canvases directly via `git commit`, and
// the read gate has to stand on its own.
func TestRequireDesignCanvasRefusesUnchangedKickoffStub(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// run.New with SeedDocs commits a kickoff stub at the design
	// canvas as part of the `Open run` commit — the same shape the
	// incident this run targets reproduced.
	if _, err := run.New(root, "tele",
		run.Options{
			ID:       "rename-headless-flags",
			Workflow: "sdlc",
			SeedDocs: map[string]string{
				"design": "# Design\n\n(operator: write the design here)\n",
			},
		}); err != nil {
		t.Fatalf("run.New: %v", err)
	}
	runID := findFirstRunID(t, root, "tele")

	// Switch into root so `bureaucracy.Find` resolves correctly
	// (requirePriorCanvas walks up from cwd).
	t.Chdir(root)
	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "tele/" + runID}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on unchanged-kickoff canvas, stdout=%q errb=%q", out.String(), errb.String())
	}
	msg := errb.String()
	if !strings.Contains(msg, "unchanged from kickoff") {
		t.Fatalf("expected unchanged-from-kickoff error, got: %q", msg)
	}
	if !strings.Contains(msg, "moe sdlc design tele/"+runID) {
		t.Fatalf("expected error to point at `moe sdlc design tele/%s`, got: %q", runID, msg)
	}
}

// TestRequireDesignCanvasAcceptsEditedCanvas: when the design canvas
// blob at HEAD differs from the kickoff blob (the agent — or the
// operator via `git commit` — wrote to the canvas), `sdlc code` must
// proceed past the gate. The negative side of the unchanged-from-
// kickoff check; without it, the new gate would have to be opt-in to
// avoid breaking every existing caller.
func TestRequireDesignCanvasAcceptsEditedCanvas(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := run.New(root, "tele",
		run.Options{
			ID:       "rename-headless-flags",
			Workflow: "sdlc",
			SeedDocs: map[string]string{
				"design": "# Design\n\n(operator: write the design here)\n",
			},
		}); err != nil {
		t.Fatalf("run.New: %v", err)
	}
	runID := findFirstRunID(t, root, "tele")

	// Edit the canvas and commit so its blob at HEAD diverges from
	// the kickoff add.
	canvasRel := run.ContentPath("tele", runID, "design")
	gittest.WriteAndCommit(t, root, canvasRel,
		"# Design\n\nThe real design goes here.\n", "work: update design")

	t.Chdir(root)
	if err := requireDesignCanvas("tele", runID); err != nil {
		t.Fatalf("requireDesignCanvas should accept an edited canvas, got: %v", err)
	}
}

// TestSDLCDesignNoOpSessionRefusesAndBlocksCascade is the end-to-end
// regression for the incident this run targets: when the design agent
// exits without writing to the canvas, openSdlcDesign must surface a
// non-zero exit and the design canvas at HEAD must remain the
// untouched kickoff stub. The non-zero exit is what stops the chain
// prompt from firing — so a downstream `!!` cascade can't dispatch
// the code stage against a stale canvas.
func TestSDLCDesignNoOpSessionRefusesAndBlocksCascade(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Fake claude exits 0 without touching anything — the "agent had
	// a conversation but never wrote the canvas" shape from the
	// reconstructed git history in the design canvas.
	fakeClaudeOnPath(t, "#!/bin/sh\nexit 0\n")

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/noop-cascade"},
		&out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	// Capture the design canvas blob before the no-op session runs.
	canvasPath := filepath.Join(root, run.ContentPath("tele", "noop-cascade", "design"))
	beforeBlob, _ := os.ReadFile(canvasPath)

	out.Reset()
	errb.Reset()
	code := openSdlcDesign("tele", "noop-cascade", true, false, "", &out, &errb)
	// Non-zero exit is the cascade-blocking signal: runStageSession
	// short-circuits before promptNextStage when the inner runWiki
	// session returns non-zero, so the chain prompt never fires and
	// `!!` has no follow-on.
	if code == 0 {
		t.Fatalf("expected non-zero exit when agent never touches the canvas; stderr=%q stdout=%q", errb.String(), out.String())
	}
	// Some loud failure must reach the operator — either commitTurn
	// refusing the empty canvas, or session.Close refusing the
	// unchanged canvas. Either gate satisfies the "fail loud" rule
	// the design records; what matters is that the cascade stopped.
	stderrStr := errb.String()
	if !strings.Contains(stderrStr, "agent did not write to its canvas") &&
		!strings.Contains(stderrStr, "unchanged from main") {
		t.Errorf("expected loud refusal naming the canvas-untouched failure mode, got: %q", stderrStr)
	}
	// Canvas on disk hasn't moved.
	afterBlob, _ := os.ReadFile(canvasPath)
	if string(beforeBlob) != string(afterBlob) {
		t.Errorf("canvas mutated by no-op session:\nbefore: %q\nafter:  %q", beforeBlob, afterBlob)
	}
}

// findFirstRunID returns the only run id under projects/<project>/runs/.
// Helper for tests that call run.New without --id and need to discover
// the dated slug it derived.
func findFirstRunID(t *testing.T, root, projectID string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return e.Name()
		}
	}
	t.Fatalf("no run directory found under %s", dir)
	return ""
}

// TestCheckSandboxBoundaryClean: the happy path — no commits, no
// tracked-file changes — returns nil so design's cascade can proceed.
// Untracked scribbles are explicitly tolerated (the design stage lets
// agents poke around freely as long as they don't touch tracked
// state).
func TestCheckSandboxBoundaryClean(t *testing.T) {
	repo := gittest.Init(t)
	entry := gittest.WriteAndCommit(t, repo, "README.md", "seed\n", "seed")

	// Untracked file present — must NOT trip the check.
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	if err := checkSandboxBoundary(repo, entry); err != nil {
		t.Fatalf("expected clean sandbox to pass, got: %v", err)
	}
}

// TestCheckSandboxBoundaryHeadAdvancedFails: an agent that commits to
// the sandbox during design tripping the gate is the primary thing
// this check exists to catch. The error names the entry and current
// SHAs so the operator can reset deliberately.
func TestCheckSandboxBoundaryHeadAdvancedFails(t *testing.T) {
	repo := gittest.Init(t)
	entry := gittest.WriteAndCommit(t, repo, "README.md", "seed\n", "seed")
	gittest.WriteAndCommit(t, repo, "extra.txt", "spike\n", "spike commit during design")

	err := checkSandboxBoundary(repo, entry)
	if err == nil {
		t.Fatalf("expected HEAD-advanced check to fail")
	}
	if !strings.Contains(err.Error(), "HEAD advanced") {
		t.Fatalf("error should name the advance, got: %v", err)
	}
}

// TestCheckSandboxBoundaryDirtyTrackedFails: modifications to a
// tracked file (without a commit) also trip the gate — the agent
// can't "design" by leaving uncommitted edits behind as a hint.
func TestCheckSandboxBoundaryDirtyTrackedFails(t *testing.T) {
	repo := gittest.Init(t)
	entry := gittest.WriteAndCommit(t, repo, "README.md", "seed\n", "seed")

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("design-spike modification\n"), 0o644); err != nil {
		t.Fatalf("rewrite README: %v", err)
	}

	err := checkSandboxBoundary(repo, entry)
	if err == nil {
		t.Fatalf("expected dirty-tracked check to fail")
	}
	if !strings.Contains(err.Error(), "uncommitted tracked-file changes") {
		t.Fatalf("error should name the dirty-tracked path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("error should name the offending path, got: %v", err)
	}
}

// TestCheckSandboxBoundaryDeletedTrackedFails: a deletion of a
// tracked file is also a tracked-file change — same outcome as
// modification.
func TestCheckSandboxBoundaryDeletedTrackedFails(t *testing.T) {
	repo := gittest.Init(t)
	entry := gittest.WriteAndCommit(t, repo, "README.md", "seed\n", "seed")

	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("delete README: %v", err)
	}

	err := checkSandboxBoundary(repo, entry)
	if err == nil {
		t.Fatalf("expected deletion check to fail")
	}
	if !strings.Contains(err.Error(), "uncommitted tracked-file changes") {
		t.Fatalf("error should name the dirty-tracked path, got: %v", err)
	}
}

// TestSDLCDesignSandboxBoundaryRefusesCommit: end-to-end coverage for
// the design exit check. A fake claude that commits inside the
// sandbox during design must (a) keep the bureaucracy-side canvas
// commit (so the operator's design work isn't lost) but (b) exit
// non-zero so the cascade halts — same shape as the no-op session
// gate's cascade-blocking behaviour.
func TestSDLCDesignSandboxBoundaryRefusesCommit(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	// Fake claude writes to the design canvas (so the canvas-untouched
	// gate doesn't fire first) AND drops a tracked-file commit in the
	// sandbox clone (parsed from the prompt). Either failure mode (HEAD
	// advance OR dirty tree) is enough to trip the boundary check; this
	// picks HEAD advance because it's the spike-as-handoff path the
	// design closed off. Cwd is the bureaucracy session worktree under
	// the cwd-inversion shape, so the sandbox path has to come from the
	// prompt — same pattern the existing fake-claude scripts use for
	// the canvas path.
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
sandbox=$(printf '%s' "$prompt" | awk '/exposed as an additional writable/ {getline; getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'design canvas content\n' >> "$canvas"; fi
if [ -n "$sandbox" ]; then
  (cd "$sandbox" && printf 'spike code\n' > spike.txt && git add spike.txt && git commit -m 'spike during design') >/dev/null 2>&1
fi
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/boundary"},
		&out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	code := openSdlcDesign("tele", "boundary", true, false, "", &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit when agent commits to sandbox; stderr=%q stdout=%q", errb.String(), out.String())
	}
	if !strings.Contains(errb.String(), "HEAD advanced") {
		t.Errorf("expected stderr to name the boundary violation, got: %q", errb.String())
	}

	// The bureaucracy-side canvas commit must still have landed —
	// losing the agent's design work on top of the spike would be a
	// double-punishment recovery path.
	canvasPath := filepath.Join(root, run.ContentPath("tele", "boundary", "design"))
	body, err := os.ReadFile(canvasPath)
	if err != nil {
		t.Fatalf("design canvas missing after boundary refusal: %v", err)
	}
	if !strings.Contains(string(body), "design canvas content") {
		t.Errorf("canvas should preserve agent's design work even after boundary refusal: %q", body)
	}
}

// TestSDLCCodeWrongProjectSaysRunNotFound: on `sdlc code` with a typo,
// the operator must see "run not found" and not "design canvas
// missing" — the latter sends them off to run a design stage that's
// also going to fail. The pre-flight beats requireDesignCanvas to the
// punch.
func TestSDLCCodeWrongProjectSaysRunNotFound(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "code", "wrongproj/ghost"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on wrong-project typo, stdout=%q", out.String())
	}
	msg := errb.String()
	if !strings.Contains(msg, "run not found") {
		t.Fatalf("expected run-not-found error, got: %q", msg)
	}
	if strings.Contains(msg, "design canvas missing") {
		t.Fatalf("typo should not surface as design-canvas-missing, got: %q", msg)
	}
}
