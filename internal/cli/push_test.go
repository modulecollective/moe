package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// pushFixture wires together the moving pieces runPush expects:
// a registered project pointing at a bare origin, a seeded run in
// StatusInProgress, a sandbox clone on moe/<run> with one commit
// ahead of the default branch, and a code/content.md ready to ship.
type pushFixture struct {
	t         *testing.T
	root      string
	origin    string
	projectID string
	runID     string
	branch    string
	clonePath string
	tipSHA    string
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	// Push's merge path now pops $EDITOR on followups.md before
	// harvest (matching close — both are operator-initiated termination
	// decisions). Stub it to a no-op binary so the harvest pre-flight
	// succeeds without dropping the test into vi. Tests that explicitly
	// exercise the no-editor failure can override with noEditor(t).
	stubEditor(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	projectID := "tele"
	runID := "fix-it"

	// Bare "remote" with one seed commit on main — the push/merge
	// flow FF-pushes on top of this.
	origin := filepath.Join(t.TempDir(), projectID+".git")
	gittest.Run(t, "", "init", "--bare", "-b", "main", origin)
	seed := t.TempDir()
	gittest.Run(t, "", "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	gittest.Run(t, seed, "add", "README.md")
	gittest.Run(t, seed, "commit", "-m", "seed")
	gittest.Run(t, seed, "remote", "add", "origin", origin)
	gittest.Run(t, seed, "push", "origin", "main")

	// Register as submodule so project.Load / sandbox.Ensure find it.
	subPath := filepath.Join("projects", projectID, "src")
	gittest.Run(t, root, "-c", "protocol.file.allow=always",
		"submodule", "add", "-b", "main", origin, subPath)

	// Seed the run first — seedRun writes a minimal project.json, so
	// we overwrite it afterwards with the fields push/project.Load need.
	trailerstest.SeedRun(t, root, projectID, runID, "sdlc", run.StatusInProgress)
	writeFile(t, filepath.Join(root, "projects", projectID, "project.json"),
		`{"id":"`+projectID+`","submodule":"`+subPath+`","remote":"`+origin+`","default_branch":"main"}`+"\n")
	gittest.Run(t, root, "add", ".gitmodules", subPath, filepath.Join("projects", projectID, "project.json"))
	gittest.Run(t, root, "commit", "-m", "Register project "+projectID)
	writeContent(t, root, projectID, runID, "code", "# code doc\n")
	gittest.Run(t, root, "add", filepath.Join("projects", projectID, "runs", runID, "documents", "code", "content.md"))
	gittest.Run(t, root, "commit", "-m", "work: update code\n\nMoE-Run: "+runID+"\nMoE-Document: code\n")

	// Bring the sandbox up — mirrors what `moe sdlc code` does.
	clonePath, err := sandbox.Ensure(root, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	branch := branchPrefix + runID
	if err := sandbox.CheckoutBranch(clonePath, branch); err != nil {
		t.Fatal(err)
	}
	// Put a single commit on moe/<run> so checkBranchHasCommits
	// passes and FF-merge has something to ship.
	writeFile(t, filepath.Join(clonePath, "feature.txt"), "hello\n")
	gittest.Run(t, clonePath, "add", "feature.txt")
	gittest.Run(t, clonePath, "commit", "-m", "add feature")
	tipSHA := gittest.HeadSHA(t, clonePath)

	stubSynthesisWritesCanvas(t, root, projectID, runID, "# Push\n\n## PR body\n\nSynthesized body for review.\n\n## Ship readiness\n\nGreen.\n\n## Conflicts surfaced\n\n")

	return &pushFixture{
		t:         t,
		root:      root,
		origin:    origin,
		projectID: projectID,
		runID:     runID,
		branch:    branch,
		clonePath: clonePath,
		tipSHA:    tipSHA,
	}
}

// originHasRef reports whether the bare origin has a given ref.
func (f *pushFixture) originHasRef(ref string) bool {
	f.t.Helper()
	cmd := exec.Command("git", "-C", f.origin, "rev-parse", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

// originHead returns the SHA at origin's main.
func (f *pushFixture) originHead() string {
	f.t.Helper()
	return gittest.Output(f.t, f.origin, "rev-parse", "main")
}

// reloadRun returns the run's on-disk metadata — tests check it as a
// proxy for "push committed the status flip."
func (f *pushFixture) reloadRun() *run.Metadata {
	f.t.Helper()
	md, err := run.Load(f.root, f.projectID, f.runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return md
}

// runInRoot invokes the CLI against f.root as MOE_HOME.
func (f *pushFixture) runInRoot(args ...string) (string, string, int) {
	f.t.Helper()
	f.t.Setenv("MOE_HOME", f.root)
	f.t.Setenv("NO_COLOR", "1")
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// TestPushMergeFFAdvancesOriginAndMarksMerged is the happy-path
// default-merge test: origin/main moves to moe/<run>'s tip, the run
// flips to StatusMerged, the sandbox is gone, the remote branch is
// deleted, and the merge path leaves a deterministic push note.
func TestPushMergeFFAdvancesOriginAndMarksMerged(t *testing.T) {
	f := newPushFixture(t)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	if got := f.originHead(); got != f.tipSHA {
		t.Fatalf("origin/main: want %s, got %s", f.tipSHA, got)
	}
	if f.originHasRef("refs/heads/" + f.branch) {
		t.Fatalf("expected %s deleted on origin", f.branch)
	}
	if sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("expected sandbox removed")
	}
	md := f.reloadRun()
	if md.Status != run.StatusMerged {
		t.Fatalf("status: want %s, got %s", run.StatusMerged, md.Status)
	}
	// MoE-Merged trailer carries the tip SHA.
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-Merged: "+f.tipSHA) {
		t.Fatalf("MoE-Merged trailer missing tip SHA:\n%s", body)
	}
	canvas, err := os.ReadFile(filepath.Join(f.root, run.ContentPath(f.projectID, f.runID, "push")))
	if err != nil {
		t.Fatalf("read push canvas: %v", err)
	}
	gotCanvas := string(canvas)
	for _, want := range []string{
		"Shipped by fast-forward merge. No push synthesis was run for this path.",
		"Code-stage record: `projects/tele/runs/fix-it/documents/code/content.md`.",
		"No test-stage canvas was present at `projects/tele/runs/fix-it/documents/test/content.md`.",
	} {
		if !strings.Contains(gotCanvas, want) {
			t.Fatalf("merge push canvas missing %q:\n%s", want, canvas)
		}
	}
	if strings.Contains(gotCanvas, "Synthesized body") {
		t.Fatalf("merge path should not synthesize push canvas, got:\n%s", canvas)
	}
}

// TestPushRebasesAndMergesWhenDefaultMovedCleanly: when origin/main
// has advanced past the merge-base but the divergent commits don't
// conflict with moe/<run>, the pre-push hook rebases the run branch
// onto origin/main and the ff-merge proceeds. The original
// "default moved" path used to bail with a manual-rebase nudge — the
// hook is the bail's replacement.
func TestPushRebasesAndMergesWhenDefaultMovedCleanly(t *testing.T) {
	f := newPushFixture(t)

	// Advance origin/main independently so the run branch is behind it.
	// The divergent file (`other.txt`) doesn't overlap with `feature.txt`
	// so the rebase is clean.
	work := t.TempDir()
	gittest.Run(t, "", "clone", "-b", "main", f.origin, work)
	writeFile(t, filepath.Join(work, "other.txt"), "other\n")
	gittest.Run(t, work, "add", "other.txt")
	gittest.Run(t, work, "commit", "-m", "divergent")
	gittest.Run(t, work, "push", "origin", "main")
	movedBaseSHA := gittest.HeadSHA(t, work)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "rebasing "+f.branch+" onto origin/main") {
		t.Fatalf("expected rebase status line in stdout, got:\n%s", stdout)
	}

	// origin/main now contains both the divergent commit and the run's
	// `feature.txt` commit, in that order. We don't pin the exact tip
	// SHA — the rebase rewrote the run commit — but we do assert the
	// merge happened (run flipped to merged, sandbox gone, branch deleted)
	// and the divergent commit is still in origin's history.
	headOut := gittest.Output(t, f.origin, "rev-parse", "main")
	if headOut == movedBaseSHA {
		t.Fatalf("origin/main should have advanced past the divergent commit after the rebased ff-push, still at %s", movedBaseSHA)
	}
	if anc := exec.Command("git", "-C", f.origin, "merge-base", "--is-ancestor", movedBaseSHA, "main"); anc.Run() != nil {
		t.Fatalf("origin/main should still contain the divergent commit %s", movedBaseSHA)
	}
	if f.originHasRef("refs/heads/" + f.branch) {
		t.Fatalf("expected %s deleted on origin", f.branch)
	}
	if sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("expected sandbox removed")
	}
	if md := f.reloadRun(); md.Status != run.StatusMerged {
		t.Fatalf("status: want %s, got %s", run.StatusMerged, md.Status)
	}
}

// TestPushRebaseConflictOpensCodeSession: when the pre-push rebase
// hits real conflicts (origin's divergent commit touches the same
// file), push aborts the rebase, hands control to the chain-back
// (a fresh code session), and exits non-zero. Origin is untouched
// and the run stays in_progress so the operator's re-run after
// the agent commits the resolution is the merge-trigger.
func TestPushRebaseConflictOpensCodeSession(t *testing.T) {
	f := newPushFixture(t)

	// Advance origin/main with a commit that touches the SAME file
	// the run branch added — guarantees a rebase conflict.
	work := t.TempDir()
	gittest.Run(t, "", "clone", "-b", "main", f.origin, work)
	writeFile(t, filepath.Join(work, "feature.txt"), "from-default\n")
	gittest.Run(t, work, "add", "feature.txt")
	gittest.Run(t, work, "commit", "-m", "default-side feature")
	gittest.Run(t, work, "push", "origin", "main")
	movedHead := gittest.HeadSHA(t, work)

	// Stub the chain-back: capture the conflict context, do not actually
	// launch Claude (the test process has no terminal/agent).
	var captured *push.RebaseConflictError
	var capturedRun *run.Metadata
	prev := openCodeSessionForRebaseConflict
	openCodeSessionForRebaseConflict = func(md *run.Metadata, c *push.RebaseConflictError, _, _ io.Writer) (int, error) {
		capturedRun = md
		captured = c
		return 1, &PushDeferredError{Recovery: "rebase-conflict", Project: md.Project, Run: md.ID}
	}
	t.Cleanup(func() { openCodeSessionForRebaseConflict = prev })

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero exit on rebase conflict; stdout=%s stderr=%s", stdout, stderr)
	}
	if captured == nil {
		t.Fatal("expected chain-back to be invoked with conflict context")
	}
	if capturedRun == nil || capturedRun.ID != f.runID {
		t.Fatalf("chain-back run.Metadata: want id=%s, got %#v", f.runID, capturedRun)
	}
	if captured.Branch != f.branch || captured.DefaultBranch != "main" {
		t.Fatalf("chain-back conflict context: branch=%q default=%q", captured.Branch, captured.DefaultBranch)
	}
	if len(captured.Conflicts) == 0 {
		t.Fatalf("chain-back conflict list should name at least one path; got empty")
	}
	foundFeature := false
	for _, p := range captured.Conflicts {
		if p == "feature.txt" {
			foundFeature = true
			break
		}
	}
	if !foundFeature {
		t.Fatalf("expected feature.txt in conflict list, got %v", captured.Conflicts)
	}

	// Origin untouched — operator must re-run push after resolution.
	if got := f.originHead(); got != movedHead {
		t.Fatalf("origin/main must not advance on rebase conflict: want %s, got %s", movedHead, got)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox must remain on rebase conflict so the agent can resolve")
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status should remain in_progress on rebase conflict, got %s", md.Status)
	}

	// Sandbox should be back to a clean working tree (rebase was --abort'd).
	entries, err := git.Status(f.clonePath)
	if err != nil {
		t.Fatalf("git status sandbox: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("sandbox should be clean after rebase --abort, got %d entries", len(entries))
	}

	// Kickoff prompt should name the conflicting file, the target branch,
	// and the workflow verb the post-turn chain prompt will offer.
	kickoff := buildRebaseConflictKickoff("sdlc", captured)
	if !strings.Contains(kickoff, "feature.txt") {
		t.Fatalf("kickoff prompt missing feature.txt: %s", kickoff)
	}
	if !strings.Contains(kickoff, "origin/main") {
		t.Fatalf("kickoff prompt missing origin/main: %s", kickoff)
	}
	if !strings.Contains(kickoff, "moe sdlc push") {
		t.Fatalf("kickoff prompt missing `moe sdlc push`: %s", kickoff)
	}
	if !strings.Contains(kickoff, "chain prompt") {
		t.Fatalf("kickoff prompt should point the agent at the post-turn chain prompt rather than asking the operator to re-run: %s", kickoff)
	}
}

// TestRunPushReturnsDeferredOnRebaseRecovery pins the typed-error
// contract added in cascade-message-was-a-lie: when push hands off to
// a recovery session and that session exits cleanly (exit 0), the
// caller still gets a *PushDeferredError back through runPushTyped so
// the cascade can render "deferred to recovery (rebase conflict) —
// stopped" instead of mistaking the clean recovery for a successful
// ship. The standalone CLI (pushCmd.Run) discards the error to
// preserve the Command.Run int contract — so exit 0 still flows
// through to the shell, but the cascade reads the typed channel.
//
// Subtest "rebase-conflict" covers openCodeSessionForRebaseConflict;
// "hook-failure" covers openCodeSessionForHookFailure. Both pin
// the same shape: clean inner exit + typed error.
func TestRunPushReturnsDeferredOnRebaseRecovery(t *testing.T) {
	t.Run("rebase-conflict", func(t *testing.T) {
		f := newPushFixture(t)

		// Advance origin/main with a commit that conflicts with the run
		// branch's feature.txt commit, so the pre-push rebase hits a
		// real conflict.
		work := t.TempDir()
		gittest.Run(t, "", "clone", "-b", "main", f.origin, work)
		writeFile(t, filepath.Join(work, "feature.txt"), "from-default\n")
		gittest.Run(t, work, "add", "feature.txt")
		gittest.Run(t, work, "commit", "-m", "default-side feature")
		gittest.Run(t, work, "push", "origin", "main")

		// Stub the recovery helper to return (0, *PushDeferredError) —
		// the "agent resolved and exited cleanly" case.
		prev := openCodeSessionForRebaseConflict
		openCodeSessionForRebaseConflict = func(md *run.Metadata, _ *push.RebaseConflictError, _, _ io.Writer) (int, error) {
			return 0, &PushDeferredError{
				Recovery: "rebase-conflict",
				Project:  md.Project,
				Run:      md.ID,
			}
		}
		t.Cleanup(func() { openCodeSessionForRebaseConflict = prev })

		// Call runPushTyped directly to assert both returns. MOE_HOME
		// is what bureaucracy.Find consults, so the test can run from
		// any cwd.
		t.Setenv("MOE_HOME", f.root)
		t.Setenv("NO_COLOR", "1")
		var stdout, stderr bytes.Buffer
		code, err := runPushTyped("sdlc", []string{f.projectID, f.runID}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runPushTyped exit: want 0 (recovery exited cleanly), got %d; stderr=%s", code, stderr.String())
		}
		var deferred *PushDeferredError
		if !errors.As(err, &deferred) {
			t.Fatalf("runPushTyped error: want *PushDeferredError, got %T (%v)", err, err)
		}
		if deferred.Recovery != "rebase-conflict" {
			t.Fatalf("deferred.Recovery: want %q, got %q", "rebase-conflict", deferred.Recovery)
		}
		if deferred.Project != f.projectID || deferred.Run != f.runID {
			t.Fatalf("deferred identity: want (%s,%s), got (%s,%s)",
				f.projectID, f.runID, deferred.Project, deferred.Run)
		}

		// The standalone CLI contract: pushCmd.Run wraps runPushTyped
		// and discards the error. Exit 0 flows through; the typed
		// signal is invisible at the shell boundary.
		stdoutBuf, stderrBuf, cliCode := f.runInRoot("sdlc", "push", f.projectID, f.runID)
		if cliCode != 0 {
			t.Fatalf("pushCmd.Run (clean recovery) exit: want 0, got %d; stdout=%s stderr=%s",
				cliCode, stdoutBuf, stderrBuf)
		}
	})

	t.Run("hook-failure", func(t *testing.T) {
		f := newPushFixture(t)

		writeHookScript(t, f.root, f.projectID, "pre-push", "10-fail.sh", `#!/bin/sh
echo "intentional hook output"
exit 7
`)

		prev := openCodeSessionForHookFailure
		openCodeSessionForHookFailure = func(md *run.Metadata, _ *hookFailure, _, _ io.Writer) (int, error) {
			return 0, &PushDeferredError{
				Recovery: "hook-failure",
				Project:  md.Project,
				Run:      md.ID,
			}
		}
		t.Cleanup(func() { openCodeSessionForHookFailure = prev })

		t.Setenv("MOE_HOME", f.root)
		t.Setenv("NO_COLOR", "1")
		var stdout, stderr bytes.Buffer
		code, err := runPushTyped("sdlc", []string{f.projectID, f.runID}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runPushTyped exit: want 0 (recovery exited cleanly), got %d; stderr=%s", code, stderr.String())
		}
		var deferred *PushDeferredError
		if !errors.As(err, &deferred) {
			t.Fatalf("runPushTyped error: want *PushDeferredError, got %T (%v)", err, err)
		}
		if deferred.Recovery != "hook-failure" {
			t.Fatalf("deferred.Recovery: want %q, got %q", "hook-failure", deferred.Recovery)
		}
		if deferred.Project != f.projectID || deferred.Run != f.runID {
			t.Fatalf("deferred identity: want (%s,%s), got (%s,%s)",
				f.projectID, f.runID, deferred.Project, deferred.Run)
		}

		stdoutBuf, stderrBuf, cliCode := f.runInRoot("sdlc", "push", f.projectID, f.runID)
		if cliCode != 0 {
			t.Fatalf("pushCmd.Run (clean recovery) exit: want 0, got %d; stdout=%s stderr=%s",
				cliCode, stdoutBuf, stderrBuf)
		}
	})
}

// TestPushNoRebaseNeededFastPath: when origin/main hasn't moved past
// the run branch's merge-base, the pre-push hook short-circuits with
// no rebase and the merge proceeds as before. Asserts no rebase was
// attempted (no "rebasing ..." line in stdout) so the fast path stays
// fast.
func TestPushNoRebaseNeededFastPath(t *testing.T) {
	f := newPushFixture(t)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "rebasing ") {
		t.Fatalf("expected no rebase when origin hasn't moved, got:\n%s", stdout)
	}
	if got := f.originHead(); got != f.tipSHA {
		t.Fatalf("origin/main: want %s, got %s", f.tipSHA, got)
	}
}

// TestPushHarvestsFollowupsWithBodyAtMerge pins the new push-time
// editor gate end-to-end: a sentinel $EDITOR is invoked on the
// followups.md, the captured body lands in the harvested idea's seed
// canvas, the line is rewritten as `[x]` carrying the resolved slug,
// and the merge proceeds. Without the skipEdit=false flip in
// mergePath this test would not see the editor invocation marker.
func TestPushHarvestsFollowupsWithBodyAtMerge(t *testing.T) {
	f := newPushFixture(t)

	// Sentinel editor: writes a marker file when invoked. Confirms the
	// editor pops at push time, not just that harvest ran (which it
	// would have under the old skipEdit=true path too).
	marker := filepath.Join(t.TempDir(), "editor-was-called")
	editorScript := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(editorScript,
		[]byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editorScript)
	t.Setenv("VISUAL", "")

	writeFollowups(t, f.root, f.projectID, f.runID, strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"",
		"  Why: foo's internals leak; foo.go:42 is the load-bearing line.",
		"",
	}, "\n"))

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected $EDITOR to be invoked at push time: %v", err)
	}

	canvas, err := os.ReadFile(filepath.Join(f.root,
		"projects", f.projectID, "runs", "cleanup-foo", "documents", "idea", "content.md"))
	if err != nil {
		t.Fatalf("read harvested idea canvas: %v", err)
	}
	want := "# Clean up foo helper\n" +
		"\n" +
		"Why: foo's internals leak; foo.go:42 is the load-bearing line.\n"
	if string(canvas) != want {
		t.Errorf("idea canvas missing body:\nwant: %q\n got: %q", want, string(canvas))
	}

	got, err := os.ReadFile(filepath.Join(f.root, run.FollowupsPath(f.projectID, f.runID)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "- [x] `cleanup-foo` — Clean up foo helper") {
		t.Errorf("followups.md not rewritten as harvested:\n%s", got)
	}
}

// TestPushFailsCleanlyWhenNoEditorAtMerge: the merge path pops $EDITOR
// on followups.md before harvest whenever there's an unchecked entry to
// review. With one such entry on disk and neither $EDITOR nor $VISUAL
// set, push fails with the editor-missing error, the run stays
// in_progress, and origin/main is unchanged.
func TestPushFailsCleanlyWhenNoEditorAtMerge(t *testing.T) {
	f := newPushFixture(t)
	noEditor(t)

	// Seed an unchecked entry so the harvest pre-flight reaches the
	// editor pop — an empty followups.md now short-circuits before the
	// $EDITOR check, which would let push succeed.
	writeFollowups(t, f.root, f.projectID, f.runID,
		"- [ ] `chase-it` — Chase the thing\n")

	mainBefore := f.originHead()
	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero exit when no editor is configured; stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "no $EDITOR or $VISUAL set") {
		t.Errorf("expected editor-missing error in stderr, got: %s", stderr)
	}
	if got := f.originHead(); got != mainBefore {
		t.Fatalf("origin/main must not advance when push fails the editor gate: want %s, got %s", mainBefore, got)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status should remain in_progress after editor-gate failure, got %s", md.Status)
	}
}

// TestPushPRPathOpensPRAndKeepsSandbox regresses today's --pr flow:
// the branch is pushed, a PR opens via gh, the run flips to
// StatusPushed with a MoE-PR trailer, and the sandbox stays put.
// origin/main must NOT move — --pr is the opt-in "wait for review"
// path.
//
// Uses a `url.<ghHost>.insteadOf = <localBare>` rewrite so project.json
// can name a GitHub-shaped URL (which ghRepoSpec parses) while git push
// still hits the local bare repo the fixture set up.
func TestPushPRPathOpensPRAndKeepsSandbox(t *testing.T) {
	f := newPushFixture(t)
	const fakeRemote = "https://github.com/owner/repo.git"
	addInsteadOfRewrite(t, fakeRemote, f.origin)
	writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"),
		`{"id":"`+f.projectID+`","submodule":"projects/`+f.projectID+`/src","remote":"`+fakeRemote+`","default_branch":"main"}`+"\n")
	gittest.Run(t, f.root, "add", filepath.Join("projects", f.projectID, "project.json"))
	gittest.Run(t, f.root, "commit", "-m", "use GitHub-shaped remote for --pr test")

	fakeGh(t, nil)
	stubSynthesisWritesCanvas(t, f.root, f.projectID, f.runID,
		"# Push\n\n## PR body\n\nSynthesized body for review.\n\n## Ship readiness\n\nGreen.\n")

	mainBefore := f.originHead()

	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	// origin/main untouched — --pr leaves the merge to the human.
	if got := f.originHead(); got != mainBefore {
		t.Fatalf("origin/main must not advance on --pr path: want %s, got %s", mainBefore, got)
	}
	// Remote branch is still there for the reviewer.
	if !f.originHasRef("refs/heads/" + f.branch) {
		t.Fatalf("%s should still exist on origin after --pr", f.branch)
	}
	// Sandbox stays — iteration via `moe sdlc code` remains a one-liner.
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox must be preserved on --pr path")
	}
	md := f.reloadRun()
	if md.Status != run.StatusPushed {
		t.Fatalf("status: want pushed, got %s", md.Status)
	}
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-PR: https://github.com/owner/repo/pull/99") {
		t.Fatalf("MoE-PR trailer missing:\n%s", body)
	}
	if !strings.Contains(stdout, "opened PR: https://github.com/owner/repo/pull/99") {
		t.Fatalf("expected PR URL in stdout, got:\n%s", stdout)
	}
}

// TestPushPRPathSynthesizesBeforeExistingPR pins the re-run behavior:
// even when GitHub already has an open PR and `gh pr create` will not
// run, push still refreshes the push canvas before recording/reusing
// the PR.
func TestPushPRPathSynthesizesBeforeExistingPR(t *testing.T) {
	f := newPushFixture(t)
	const fakeRemote = "https://github.com/owner/repo.git"
	const existingURL = "https://github.com/owner/repo/pull/77"
	addInsteadOfRewrite(t, fakeRemote, f.origin)
	writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"),
		`{"id":"`+f.projectID+`","submodule":"projects/`+f.projectID+`/src","remote":"`+fakeRemote+`","default_branch":"main"}`+"\n")
	gittest.Run(t, f.root, "add", filepath.Join("projects", f.projectID, "project.json"))
	gittest.Run(t, f.root, "commit", "-m", "use GitHub-shaped remote for existing PR test")

	var synthCalls int
	prev := runStageSession
	runStageSession = func(_, _, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		if docID != "push" {
			t.Fatalf("unexpected docID %q", docID)
		}
		synthCalls++
		canvasPath := filepath.Join(f.root, run.ContentPath(f.projectID, f.runID, "push"))
		if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canvasPath, []byte("# Push\n\n## PR body\n\nRefreshed existing PR body.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })
	fakeGhExistingPR(t, existingURL)

	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if synthCalls != 1 {
		t.Fatalf("synthesis calls = %d, want 1", synthCalls)
	}
	if !strings.Contains(stdout, "existing PR: "+existingURL) {
		t.Fatalf("expected existing PR URL in stdout, got:\n%s", stdout)
	}
	body := lastCommitMessage(t, f.root)
	if !strings.Contains(body, "MoE-PR: "+existingURL) {
		t.Fatalf("MoE-PR trailer should record existing PR:\n%s", body)
	}
	canvas, err := os.ReadFile(filepath.Join(f.root, run.ContentPath(f.projectID, f.runID, "push")))
	if err != nil {
		t.Fatalf("read push canvas: %v", err)
	}
	if !strings.Contains(string(canvas), "Refreshed existing PR body.") {
		t.Fatalf("existing PR path should refresh push canvas, got:\n%s", canvas)
	}
}

// TestExtractMarkdownSection pins the slicing helper writePRBodyFile
// uses to pull `## PR body` out of the push canvas. The skeleton has
// a fixed shape, so the helper doesn't need a real markdown parser —
// but the line-by-line semantics ("section ends at next `## ` or
// EOF, contents trimmed") have to stay honest, since they're what
// ends up on the PR.
func TestExtractMarkdownSection(t *testing.T) {
	const canvas = `# Push

## PR body

Body line 1.

Body line 2.

## Ship readiness

Green.

## Conflicts surfaced
`
	if got, want := extractMarkdownSection(canvas, "PR body"), "Body line 1.\n\nBody line 2."; got != want {
		t.Fatalf("PR body section = %q, want %q", got, want)
	}
	if got, want := extractMarkdownSection(canvas, "Ship readiness"), "Green."; got != want {
		t.Fatalf("Ship readiness section = %q, want %q", got, want)
	}
	// Final section: extends to EOF and trims trailing whitespace.
	if got, want := extractMarkdownSection(canvas, "Conflicts surfaced"), ""; got != want {
		t.Fatalf("empty trailing section = %q, want %q", got, want)
	}
	// Missing section returns "".
	if got := extractMarkdownSection(canvas, "Nope"); got != "" {
		t.Fatalf("missing section = %q, want \"\"", got)
	}
}

// TestWritePRBodyFile exercises the helper openPRPath uses after
// synthesis writes the push canvas: read push/content.md, slice out
// `## PR body`, drop the content in a tempfile gh pr create can pass
// to --body-file.
func TestWritePRBodyFile(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "push"))
	if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const canvas = "# Push\n\n## PR body\n\nFinal body for the reviewer.\n\n## Ship readiness\n\nGreen.\n"
	if err := os.WriteFile(canvasPath, []byte(canvas), 0o644); err != nil {
		t.Fatal(err)
	}

	path, cleanup, err := writePRBodyFile(root, md)
	if err != nil {
		t.Fatalf("writePRBodyFile: %v", err)
	}
	defer cleanup()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if want := "Final body for the reviewer."; string(got) != want {
		t.Fatalf("body file = %q, want %q", string(got), want)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove tempfile %s: stat err=%v", path, err)
	}
}

// TestWritePRBodyFileMissingCanvas: synthesis was supposed to write
// the push canvas just above; if it's missing the helper surfaces a
// clear error instead of silently degrading to an empty PR body.
func TestWritePRBodyFileMissingCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	_, _, err := writePRBodyFile(root, md)
	if err == nil {
		t.Fatal("expected error when push canvas is missing")
	}
	if !strings.Contains(err.Error(), "push canvas") {
		t.Fatalf("expected error to mention push canvas, got: %v", err)
	}
}

// TestWritePRBodyFileMissingSection: a push canvas without a `## PR
// body` heading (degenerate synthesis output) is an error, not a
// soft fallback. The PR opens with whatever synthesis produced or it
// doesn't open at all — silently shipping a blank body is the worst
// case.
func TestWritePRBodyFileMissingSection(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "push"))
	if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasPath, []byte("# Push\n\n## Ship readiness\n\nGreen.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := writePRBodyFile(root, md)
	if err == nil {
		t.Fatal("expected error when `## PR body` section is missing")
	}
	if !strings.Contains(err.Error(), "PR body") {
		t.Fatalf("expected error to mention PR body, got: %v", err)
	}
}

func TestWriteMechanicalPushNoteMentionsTestCanvasWhenPresent(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	testPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "test"))
	if err := os.MkdirAll(filepath.Dir(testPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(testPath, []byte("## What was verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel, err := writeMechanicalPushNote(root, md)
	if err != nil {
		t.Fatalf("writeMechanicalPushNote: %v", err)
	}
	if rel != run.ContentPath(md.Project, md.ID, "push") {
		t.Fatalf("rel path = %q, want push content path", rel)
	}
	canvas, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read push note: %v", err)
	}
	got := string(canvas)
	if !strings.Contains(got, "Test-stage record: `projects/tele/runs/fix-it/documents/test/content.md`.") {
		t.Fatalf("push note should name present test canvas, got:\n%s", got)
	}
	if strings.Contains(got, "No test-stage canvas") {
		t.Fatalf("push note should not claim test canvas is absent, got:\n%s", got)
	}
}

// TestRunPushSynthesisSessionLeavesModelToAgentDefault asserts the
// headless synthesis session no longer carries a push-specific cheap
// model override. PR synthesis uses the same agent model policy as
// normal code/test execution.
func TestRunPushSynthesisSessionLeavesModelToAgentDefault(t *testing.T) {
	t.Setenv("MOE_HOME", newTestBureaucracy(t))
	t.Setenv("MOE_AGENT", "claude")

	var captured stageSessionOpts
	prev := runStageSession
	runStageSession = func(_, _, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		if docID != "push" {
			t.Fatalf("unexpected docID %q", docID)
		}
		captured = opts
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })

	if code := runPushSynthesisSession("tele", "fix-it", true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("synthesis session exit=%d, want 0", code)
	}
	if captured.Model != "" {
		t.Fatalf("Model = %q, want empty agent default", captured.Model)
	}
	if !captured.Headless {
		t.Errorf("Headless: want true")
	}
}

// stubSynthesisWritesCanvas swaps runStageSession for a stub that
// writes the given canvas content to push/content.md and returns 0 —
// what `runPushSynthesisSession` does on the happy path, minus the
// actual claude turn. Tests that drive push through runPush use this so
// PR-only synthesis produces the canvas writePRBodyFile expects
// without spinning up a real session worktree.
// Restores the original on cleanup. Canvas content should be a full
// canvas including `## PR body` so writePRBodyFile finds a section to
// extract.
func stubSynthesisWritesCanvas(t *testing.T, root, projectID, runID, canvas string) {
	t.Helper()
	prev := runStageSession
	runStageSession = func(_, _, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		if docID != "push" {
			t.Fatalf("stubSynthesisWritesCanvas: unexpected docID %q (only push expected)", docID)
		}
		canvasPath := filepath.Join(root, run.ContentPath(projectID, runID, "push"))
		if err := os.MkdirAll(filepath.Dir(canvasPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canvasPath, []byte(canvas), 0o644); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })
}

// addInsteadOfRewrite appends a `url.<real>.insteadOf = <fake>` to the
// scoped GIT_CONFIG_GLOBAL newTestBureaucracy set up. Lets tests use
// GitHub-shaped URLs in project.json while git actually pushes to a
// local bare repo.
func addInsteadOfRewrite(t *testing.T, fake, real string) {
	t.Helper()
	cfg := os.Getenv("GIT_CONFIG_GLOBAL")
	if cfg == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set — newTestBureaucracy should have set it")
	}
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "[url \"%s\"]\n\tinsteadOf = %s\n", real, fake)
}

func fakeGhExistingPR(t *testing.T, url string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim gh fake only works on unix-y OSes")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
set -e
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  echo '[{"url":"` + url + `"}]'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo "unexpected gh pr create" >&2
  exit 9
fi
echo "fake gh existing PR: unsupported invocation: $*" >&2
exit 2
`
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
}

// TestPushIdempotentOnMergedRun: a rerun on a merged run is a no-op
// that prints the MoE-Merged SHA and exits 0. Mirrors today's
// "existing PR" pattern.
func TestPushIdempotentOnMergedRun(t *testing.T) {
	f := newPushFixture(t)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("first push: exit=%d stderr=%s", code, stderr)
	}
	_ = stdout

	stdout, stderr, code = f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("rerun: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "already merged") {
		t.Fatalf("expected 'already merged' notice, got: %s", stdout)
	}
	if !strings.Contains(stdout, git.ShortSHA(f.tipSHA)) {
		t.Fatalf("expected merge SHA in rerun output, got: %s", stdout)
	}
}

// TestPushIdempotentOnClosedRun: a rerun on a closed run prints
// "already closed" and exits 0.
func TestPushIdempotentOnClosedRun(t *testing.T) {
	f := newPushFixture(t)

	// Fake a StatusClosed by writing run.json directly and committing.
	md := f.reloadRun()
	md.Status = run.StatusClosed
	if err := run.Save(f.root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, f.root, "add", filepath.Join("projects", f.projectID, "runs", f.runID, "run.json"))
	gittest.Run(t, f.root, "commit", "-m", "sync: close\n\nMoE-Run: "+f.runID+"\nMoE-Closed: https://example.com/pr/1\n")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("rerun: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "already closed") {
		t.Fatalf("expected 'already closed' notice, got: %s", stdout)
	}
}

// TestPushPRSynthesisFailureBlocksPRCreation: PR-only synthesis runs
// after the branch is pushed but before PR creation. A failed synthesis
// must leave origin/main untouched and keep the run in progress.
func TestPushPRSynthesisFailureBlocksPRCreation(t *testing.T) {
	f := newPushFixture(t)

	canary := filepath.Join(t.TempDir(), "hook-ran")
	writeHookScript(t, f.root, f.projectID, "pre-push", "10-canary.sh", `#!/bin/sh
touch "`+canary+`"
`)

	prev := runStageSession
	runStageSession = func(_, _, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		if docID != "push" {
			t.Fatalf("unexpected docID %q", docID)
		}
		return 42
	}
	t.Cleanup(func() { runStageSession = prev })

	mainBefore := f.originHead()
	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID, f.runID)
	if code != 42 {
		t.Fatalf("exit=%d, want synthesis exit 42\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := f.originHead(); got != mainBefore {
		t.Fatalf("origin/main must not advance when PR synthesis fails: want %s, got %s", mainBefore, got)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("pre-push hook should run before PR synthesis; stat err=%v", err)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status should remain in_progress after synthesis failure, got %s", md.Status)
	}
}

// TestPromptPushNextStageAcceptsMergeChoice: feeding "m\n" on stdin
// runs push with no extra args (the merge path).
func TestPromptPushNextStageAcceptsMergeChoice(t *testing.T) {
	got := capturePromptDispatch(t, "m\n")
	if !got.ran {
		t.Fatalf("expected push to be dispatched")
	}
	if len(got.args) != 2 || got.args[0] != "tele" || got.args[1] != "fix-it" {
		t.Fatalf("merge path: expected [project, run], got %v", got.args)
	}
}

// TestPromptPushNextStageAcceptsPRChoice: "p\n" invokes push with
// --pr prepended.
func TestPromptPushNextStageAcceptsPRChoice(t *testing.T) {
	got := capturePromptDispatch(t, "p\n")
	if !got.ran {
		t.Fatalf("expected push to be dispatched")
	}
	if len(got.args) != 3 || got.args[0] != "--pr" {
		t.Fatalf("pr path: expected [--pr, project, run], got %v", got.args)
	}
}

// TestPromptPushNextStageCaseInsensitive: uppercase M and P behave
// identically to lowercase.
func TestPromptPushNextStageCaseInsensitive(t *testing.T) {
	for _, in := range []string{"M\n", "P\n"} {
		got := capturePromptDispatch(t, in)
		if !got.ran {
			t.Fatalf("input %q: expected dispatch", in)
		}
	}
}

// TestPromptPushNextStageBlankDeclines: Enter / blank input / "n"
// all decline — no dispatch.
func TestPromptPushNextStageBlankDeclines(t *testing.T) {
	for _, in := range []string{"\n", "n\n", "N\n", "garbage\n"} {
		got := capturePromptDispatch(t, in)
		if got.ran {
			t.Fatalf("input %q: expected decline, but push ran with args %v", in, got.args)
		}
	}
}

// TestPromptPushNextStagePrintsTestCanvas: when the test canvas
// exists on disk, its bytes appear above the [N/m/p] prompt verbatim
// (no header, no decoration). follow no longer surfaces stage
// canvases once their sessions close, so this is the canvas's one
// chance to land in front of the operator at the merge decision.
// The test canvas is the just-finished narrative — the more direct
// "should we ship?" framing than the code canvas (which holds the PR
// body but is one stage back).
func TestPromptPushNextStagePrintsCodeCanvas(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "test"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "## What was verified\n\n`go test ./...` passes.\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, body) {
		t.Errorf("canvas body not printed verbatim:\n%s", got)
	}
	if i, j := strings.Index(got, body), strings.Index(got, "[N/m/p]"); i < 0 || j < 0 || i >= j {
		t.Errorf("canvas should appear above the prompt label; canvas=%d prompt=%d", i, j)
	}
}

// TestPromptPushNextStageMissingCanvasFallsThrough: a missing canvas
// is silent — no header, no error, just the bare prompt. Robust
// against runs that reach the merge gate without a code stage having
// committed (the run was opened against an old layout, or the agent
// truly produced no canvas).
func TestPromptPushNextStageMissingCanvasFallsThrough(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(strings.TrimLeft(got, "\n"), "next: ") {
		t.Errorf("expected the bare prompt to be the only output; got:\n%s", got)
	}
}

// TestPromptPushNextStageWhitespaceCanvasFallsThrough: a canvas with
// only whitespace is treated the same as missing — the agent didn't
// say anything worth surfacing, so don't decorate the prompt with
// blank lines.
func TestPromptPushNextStageWhitespaceCanvasFallsThrough(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "code"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvas, []byte("\n\n   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if strings.HasPrefix(got, "\n") {
		t.Errorf("whitespace canvas should not pad the prompt with blank lines; got:\n%q", got)
	}
}

// TestPromptPushNextStageFallsBackToCodeCanvas: when the test canvas
// is missing (operator skipped test via `s`, or invoked `moe sdlc
// push` directly without test having landed), the code canvas takes
// its place above [N/m/p]. The operator's last reading material
// before the ship decision should still be the most recent thing
// the agent wrote.
func TestPromptPushNextStageFallsBackToCodeCanvas(t *testing.T) {
	next := &Command{
		Name: "push",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "code"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "## Summary\n\nDoc-only diff; skipped test.\n"
	if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, body) {
		t.Errorf("code canvas body not printed verbatim:\n%s", got)
	}
	if i, j := strings.Index(got, body), strings.Index(got, "[N/m/p]"); i < 0 || j < 0 || i >= j {
		t.Errorf("code canvas should appear above the prompt label; canvas=%d prompt=%d", i, j)
	}
}

// TestPromptPushNextStagePrefersTestCanvasOverCode: when both
// canvases exist, the test canvas wins — it's the more direct
// "should we ship?" framing. Pins the precedence against a future
// refactor that might accidentally swap or merge the two.
func TestPromptPushNextStagePrefersTestCanvasOverCode(t *testing.T) {
	next := &Command{
		Name: "push",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	codeCanvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "code"))
	if err := os.MkdirAll(filepath.Dir(codeCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const codeBody = "## Summary\n\nCode canvas — should not appear when test canvas exists.\n"
	if err := os.WriteFile(codeCanvas, []byte(codeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	testCanvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "test"))
	if err := os.MkdirAll(filepath.Dir(testCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const testBody = "## What was verified\n\nTest canvas — the agent's pre-push framing.\n"
	if err := os.WriteFile(testCanvas, []byte(testBody), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, testBody) {
		t.Errorf("test canvas should be printed when present:\n%s", got)
	}
	if strings.Contains(got, codeBody) {
		t.Errorf("code canvas should not appear when test canvas is present:\n%s", got)
	}
}

// promptDispatchRecord captures whether promptPushNextStage invoked
// next.Run and with what args.
type promptDispatchRecord struct {
	ran  bool
	args []string
}

// capturePromptDispatch pipes stdin through promptPushNextStage with a
// stub next Command so the test asserts the dispatch shape (or lack
// thereof) without hitting the real push flow.
func capturePromptDispatch(t *testing.T, input string) *promptDispatchRecord {
	t.Helper()
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			rec.args = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	w.Close()

	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	// Empty root → no code canvas on disk → the prompt skips the cat
	// and falls through to the bare [N/m/p] line, which is what the
	// dispatch-shape assertions below expect.
	code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("promptPushNextStage exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[N/m/p]") {
		t.Fatalf("expected [N/m/p] label in prompt, got: %s", stdout.String())
	}
	return rec
}

// TestPromptPushNextStageOffersBackWhenJustFinished: a non-nil back at
// the push prompt grows the label to [N/m/p/b] and the legend to
// "N=decline · m=fast-forward merge · p=open PR · b=back to code". Typing
// `b` dispatches back.Run with [project, run]. Pins the rule that the
// new option is appended (so the existing N-as-default stays).
func TestPromptPushNextStageOffersBackWhenJustFinished(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			rec.args = append([]string(nil), args...)
			return 0
		},
	}
	var backRan bool
	var backArgs []string
	back := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			backRan = true
			backArgs = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "b\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, []*Command{back}, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[N/m/p/b]") {
		t.Fatalf("expected [N/m/p/b] label in prompt, got: %q", got)
	}
	if !strings.Contains(got, "N=decline · m=fast-forward merge · p=open PR · b=back to code") {
		t.Fatalf("expected legend with back target in prompt, got: %q", got)
	}
	if rec.ran {
		t.Errorf("`b` must not dispatch push: rec.args=%v", rec.args)
	}
	if !backRan {
		t.Fatalf("expected back to be dispatched, but it was not")
	}
	if got, want := strings.Join(backArgs, " "), "tele fix-it"; got != want {
		t.Fatalf("back args = %q, want %q", got, want)
	}
}

// TestPromptPushNextStageNoBackWhenNil: a nil back collapses the
// label back to [N/m/p] (no /b) and the legend omits the b row.
// Mirrors TestPromptStageNextStageNoBackWhenNil for the push prompt.
func TestPromptPushNextStageNoBackWhenNil(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "b\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[N/m/p]") {
		t.Fatalf("expected [N/m/p] label without /b, got: %q", got)
	}
	if strings.Contains(got, "/b]") {
		t.Fatalf("expected no /b in label, got: %q", got)
	}
	if strings.Contains(got, "b=back") {
		t.Fatalf("expected legend without back entry, got: %q", got)
	}
	if rec.ran {
		t.Errorf("`b` with nil back must not dispatch anything")
	}
}

// TestPromptPushNextStageOffersScuttleWhenRegistered: a non-nil scuttle
// at the push prompt grows the label to [N/x/m/p] (scuttle adjacent to
// the decline default), the legend names "scuttle (close)", and typing
// `x\n` dispatches scuttle.Run([project, run]). Pins the "abandon ship"
// affordance one keystroke from the merge gate, where the design says
// the intent most often forms.
func TestPromptPushNextStageOffersScuttleWhenRegistered(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			rec.args = append([]string(nil), args...)
			return 0
		},
	}
	var scuttleRan bool
	var scuttleArgs []string
	scuttle := &Command{
		Name: "close",
		Run: func(args []string, _, _ io.Writer) int {
			scuttleRan = true
			scuttleArgs = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "x\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, scuttle, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[N/x/m/p]") {
		t.Fatalf("expected [N/x/m/p] label, got: %q", got)
	}
	if !strings.Contains(got, "x=scuttle (close)") {
		t.Fatalf("expected legend entry for scuttle, got: %q", got)
	}
	if rec.ran {
		t.Errorf("`x` must not dispatch push: rec.args=%v", rec.args)
	}
	if !scuttleRan {
		t.Fatalf("expected scuttle to dispatch on `x`")
	}
	if got, want := strings.Join(scuttleArgs, " "), "tele fix-it"; got != want {
		t.Fatalf("scuttle args = %q, want %q", got, want)
	}
}

// TestPromptPushNextStageScuttleWithBack: scuttle and back both
// registered produce [N/x/m/p/b] — scuttle adjacent to N, back at the
// tail — and the legend lists both in that order.
func TestPromptPushNextStageScuttleWithBack(t *testing.T) {
	next := &Command{
		Name: "push",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	back := &Command{
		Name: "code",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	scuttle := &Command{
		Name: "close",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, []*Command{back}, scuttle, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[N/x/m/p/b]") {
		t.Fatalf("expected [N/x/m/p/b] label, got: %q", got)
	}
	if !strings.Contains(got, "N=decline · x=scuttle (close) · m=fast-forward merge · p=open PR · b=back to code") {
		t.Fatalf("expected full legend with scuttle adjacent to decline, got: %q", got)
	}
}

// TestPromptPushNextStageNoScuttleWhenNil: a nil scuttle keeps the
// label at [N/m/p] and the legend free of any `x=` entry. `x\n` on
// stdin must not dispatch anything — it falls into the decline arm
// like any other unrecognised input.
func TestPromptPushNextStageNoScuttleWhenNil(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			rec.args = append([]string(nil), args...)
			return 0
		},
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "x\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[N/m/p]") {
		t.Fatalf("expected [N/m/p] label without /x, got: %q", got)
	}
	if strings.Contains(got, "/x/") || strings.Contains(got, "/x]") {
		t.Fatalf("expected no /x in label, got: %q", got)
	}
	if strings.Contains(got, "x=scuttle") {
		t.Fatalf("expected legend without scuttle entry, got: %q", got)
	}
	if rec.ran {
		t.Errorf("`x` with nil scuttle must not dispatch push: args=%v", rec.args)
	}
}

// TestPushSynthesisDispatchesHeadlessStage verifies the headless
// push-synthesis session is wired with the right stageSessionOpts
// shape: headless, sandboxed, post-turn chain prompt suppressed, canvas
// skeleton seeded, and no push-specific model override. Stubs runStageSession at the
// seam runPushSynthesisSession goes through.
func TestPushSynthesisDispatchesHeadlessStage(t *testing.T) {
	t.Setenv("MOE_HOME", newTestBureaucracy(t))
	t.Setenv("MOE_AGENT", "claude")

	var capturedDoc string
	var capturedOpts stageSessionOpts
	prev := runStageSession
	runStageSession = func(_, _, docID string, opts stageSessionOpts, _, _ io.Writer) int {
		capturedDoc = docID
		capturedOpts = opts
		return 0
	}
	t.Cleanup(func() { runStageSession = prev })

	var stdout, stderr bytes.Buffer
	if code := runPushSynthesisSession("tele", "fix-it", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if capturedDoc != "push" {
		t.Fatalf("docID: want push, got %q", capturedDoc)
	}
	if !capturedOpts.Headless {
		t.Errorf("Headless: want true (synthesis is headless-only)")
	}
	if !capturedOpts.NeedsSandbox {
		t.Errorf("NeedsSandbox: want true (synthesis runs in the run sandbox)")
	}
	if !capturedOpts.SkipNextStage {
		t.Errorf("SkipNextStage: want true (synthesis sits inside a larger flow)")
	}
	if capturedOpts.CanvasSkeleton == "" {
		t.Errorf("CanvasSkeleton: want non-empty (push canvas seeded with structural headings)")
	}
	if capturedOpts.Model != "" {
		t.Errorf("Model: want empty agent default, got %q", capturedOpts.Model)
	}
}

// TestPromptPushNextStageIgnoresPushCanvas pins the rule that the
// ship gate's preamble doesn't fall back to push/content.md.
// Synthesis runs inside the chosen push command, not at chain-prompt
// time, so by the time the operator reads this preamble the push canvas
// (if any) may be left over from a prior push attempt — stale relative
// to whatever the operator's about to do. Test → code is the live story.
func TestPromptPushNextStageIgnoresPushCanvas(t *testing.T) {
	next := &Command{
		Name: "push",
		Run:  func(_ []string, _, _ io.Writer) int { return 0 },
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}

	root := t.TempDir()
	testCanvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "test"))
	if err := os.MkdirAll(filepath.Dir(testCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const testBody = "## What was verified\n\nLive test canvas — this is what should appear.\n"
	if err := os.WriteFile(testCanvas, []byte(testBody), 0o644); err != nil {
		t.Fatal(err)
	}
	pushCanvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "push"))
	if err := os.MkdirAll(filepath.Dir(pushCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const pushBody = "## PR body\n\nStale push canvas — should NOT appear above the gate.\n"
	if err := os.WriteFile(pushCanvas, []byte(pushBody), 0o644); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, testBody) {
		t.Errorf("test canvas body should appear above the gate:\n%s", got)
	}
	if strings.Contains(got, pushBody) {
		t.Errorf("push canvas body must not appear at the ship gate:\n%s", got)
	}
}

// writeHookScript drops an executable script into
// projects/<p>/hooks/<event>.d/. Returns the absolute path.
func writeHookScript(t *testing.T, root, projectID, event, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "hooks", event+".d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPushProjectHookSeesSandboxAndEnv drops a pre-push.d script
// that snapshots its CWD-evidence and key MOE_* env vars, then runs
// push. The canary file confirms the script ran with CWD = sandbox
// clone (it observes feature.txt, the run-branch's only added file)
// and the dispatcher exported the documented env contract.
func TestPushProjectHookSeesSandboxAndEnv(t *testing.T) {
	f := newPushFixture(t)

	canary := filepath.Join(t.TempDir(), "canary.txt")
	body := fmt.Sprintf(`#!/bin/sh
{
  echo "feature_seen=$([ -f feature.txt ] && echo 1 || echo 0)"
  echo "MOE_PROJECT=$MOE_PROJECT"
  echo "MOE_RUN=$MOE_RUN"
  echo "MOE_DOCUMENT=$MOE_DOCUMENT"
  echo "MOE_WORKFLOW=$MOE_WORKFLOW"
  echo "MOE_SANDBOX=$MOE_SANDBOX"
  echo "MOE_BUREAUCRACY=$MOE_BUREAUCRACY"
  echo "MOE_TARGET_BRANCH=$MOE_TARGET_BRANCH"
} > %q
`, canary)
	writeHookScript(t, f.root, f.projectID, "pre-push", "10-canary.sh", body)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	got, err := os.ReadFile(canary)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	canaryStr := string(got)
	if !strings.Contains(canaryStr, "feature_seen=1") {
		t.Errorf("hook CWD: feature.txt not observable from $PWD, canary:\n%s", canaryStr)
	}
	for _, want := range []string{
		"MOE_PROJECT=" + f.projectID,
		"MOE_RUN=" + f.runID,
		"MOE_DOCUMENT=push",
		"MOE_WORKFLOW=sdlc",
		"MOE_SANDBOX=" + f.clonePath,
		"MOE_BUREAUCRACY=" + f.root,
		"MOE_TARGET_BRANCH=main",
	} {
		if !strings.Contains(canaryStr, want) {
			t.Errorf("hook env: want %q in canary:\n%s", want, canaryStr)
		}
	}
	if !strings.Contains(stdout, "pre-push hooks:") {
		t.Errorf("stdout missing pre-push section header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "→ 10-canary.sh") {
		t.Errorf("stdout missing per-script start marker:\n%s", stdout)
	}
}

// TestPushRunsBuiltinsBeforeProjectHooks pins the post-rebase ordering:
// built-ins fire first, then project scripts. The motivating bug — a
// concurrent edit to reorderFlags's signature passing local `go vet`
// against the pre-rebase tree, then breaking CI on the post-rebase one
// — only stays fixed if scripts vet what's about to be pushed.
//
// Both kinds of hook append a tag to the same file; the tag order is
// the assertion.
func TestPushRunsBuiltinsBeforeProjectHooks(t *testing.T) {
	f := newPushFixture(t)

	orderFile := filepath.Join(t.TempDir(), "order.txt")

	prev := builtinHooks[hookEventPrePush]
	tracker := builtinHook{
		Name: "test-tracker",
		Run: func(_ hookEnv, _, _ io.Writer) error {
			fh, err := os.OpenFile(orderFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return err
			}
			defer fh.Close()
			_, err = fh.WriteString("builtin\n")
			return err
		},
	}
	// Prepend so the tracker fires before the real rebase built-in. We
	// only need to record one builtin tag — the assertion is "any
	// builtin before any script."
	builtinHooks[hookEventPrePush] = append([]builtinHook{tracker}, prev...)
	t.Cleanup(func() { builtinHooks[hookEventPrePush] = prev })

	writeHookScript(t, f.root, f.projectID, "pre-push", "10-tracker.sh", fmt.Sprintf(`#!/bin/sh
echo "script" >> %q
`, orderFile))

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}

	contents, err := os.ReadFile(orderFile)
	if err != nil {
		t.Fatalf("read order file: %v", err)
	}
	got := strings.TrimSpace(string(contents))
	want := "builtin\nscript"
	if got != want {
		t.Fatalf("hook order: want %q, got %q", want, got)
	}
}

// TestPushHookFailureChainsBackToCodeSession asserts a non-zero
// project hook halts push, surfaces the captured output via the
// generic chain-back, leaves origin / sandbox / run state untouched,
// and exits non-zero.
func TestPushHookFailureChainsBackToCodeSession(t *testing.T) {
	f := newPushFixture(t)

	writeHookScript(t, f.root, f.projectID, "pre-push", "10-fail.sh", `#!/bin/sh
echo "intentional hook output"
exit 7
`)

	var captured *hookFailure
	var capturedRun *run.Metadata
	prev := openCodeSessionForHookFailure
	openCodeSessionForHookFailure = func(md *run.Metadata, fail *hookFailure, _, _ io.Writer) (int, error) {
		capturedRun = md
		captured = fail
		return 1, &PushDeferredError{Recovery: "hook-failure", Project: md.Project, Run: md.ID}
	}
	t.Cleanup(func() { openCodeSessionForHookFailure = prev })

	mainBefore := f.originHead()
	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero exit on hook failure; stdout=%s stderr=%s", stdout, stderr)
	}
	if captured == nil {
		t.Fatal("expected chain-back to be invoked with hook failure context")
	}
	if capturedRun == nil || capturedRun.ID != f.runID {
		t.Fatalf("chain-back run.Metadata: want id=%s, got %#v", f.runID, capturedRun)
	}
	if captured.event != hookEventPrePush {
		t.Fatalf("event: want %q, got %q", hookEventPrePush, captured.event)
	}
	if !strings.HasSuffix(captured.script, "10-fail.sh") {
		t.Fatalf("script: want suffix 10-fail.sh, got %q", captured.script)
	}
	if !strings.Contains(captured.output, "intentional hook output") {
		t.Fatalf("captured output: want hook stdout, got %q", captured.output)
	}

	if got := f.originHead(); got != mainBefore {
		t.Fatalf("origin/main must not advance on hook failure: want %s, got %s", mainBefore, got)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox must remain on hook failure so the agent can fix")
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status should remain in_progress on hook failure, got %s", md.Status)
	}

	kickoff := buildHookFailureKickoff("sdlc", captured)
	if !strings.Contains(kickoff, "10-fail.sh") {
		t.Fatalf("kickoff missing script name: %s", kickoff)
	}
	if !strings.Contains(kickoff, "intentional hook output") {
		t.Fatalf("kickoff missing captured output: %s", kickoff)
	}
	if !strings.Contains(kickoff, "moe sdlc push") {
		t.Fatalf("kickoff missing `moe sdlc push`: %s", kickoff)
	}
	if !strings.Contains(kickoff, "chain prompt") {
		t.Fatalf("kickoff should point the agent at the post-turn chain prompt rather than asking the operator to re-run: %s", kickoff)
	}
}

// TestChainBackPropagatesStageExitAndChainsForward verifies the
// design contract for both chain-backs: drop the inner runStageSession's
// exit code straight through, and don't suppress the post-turn chain
// prompt — so a clean fix-and-commit lets the workflow's `next: moe
// <wf> push` prompt fire, the same way `moe <wf> code` already chains.
//
// Stubs runStageSession so the chain-back closures can be exercised
// without spinning a real session worktree, and asserts the opts the
// closure passes through (no SkipNextStage, docID="code", sandbox on).
func TestChainBackPropagatesStageExitAndChainsForward(t *testing.T) {
	cases := []struct {
		name         string
		invoke       func(md *run.Metadata) (int, error)
		stubReturns  int
		wantRecovery string
	}{
		{
			name: "hook failure",
			invoke: func(md *run.Metadata) (int, error) {
				return openCodeSessionForHookFailure(md, &hookFailure{
					event:  hookEventPrePush,
					script: "10-fail.sh",
					output: "boom\n",
				}, io.Discard, io.Discard)
			},
			stubReturns:  0,
			wantRecovery: "hook-failure",
		},
		{
			name: "hook failure (inner non-zero)",
			invoke: func(md *run.Metadata) (int, error) {
				return openCodeSessionForHookFailure(md, &hookFailure{
					event:  hookEventPrePush,
					script: "10-fail.sh",
					output: "boom\n",
				}, io.Discard, io.Discard)
			},
			stubReturns:  1,
			wantRecovery: "hook-failure",
		},
		{
			name: "rebase conflict",
			invoke: func(md *run.Metadata) (int, error) {
				return openCodeSessionForRebaseConflict(md, &push.RebaseConflictError{
					Branch:        "moe/fix-it",
					DefaultBranch: "main",
					Conflicts:     []string{"feature.txt"},
				}, io.Discard, io.Discard)
			},
			stubReturns:  0,
			wantRecovery: "rebase-conflict",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var capturedDoc string
			var capturedOpts stageSessionOpts
			prev := runStageSession
			runStageSession = func(_, _, docID string, opts stageSessionOpts, _, _ io.Writer) int {
				capturedDoc = docID
				capturedOpts = opts
				return tc.stubReturns
			}
			t.Cleanup(func() { runStageSession = prev })

			md := &run.Metadata{
				ID:       "fix-it",
				Project:  "tele",
				Workflow: "sdlc",
			}
			got, err := tc.invoke(md)
			if got != tc.stubReturns {
				t.Fatalf("chain-back exit code: want propagated %d, got %d", tc.stubReturns, got)
			}
			// The typed-error contract: every chain-back surfaces a
			// *PushDeferredError tagged with the recovery flavour and
			// the run identity, regardless of whether the inner
			// session exited cleanly or not. The cascade reads this
			// to render `push deferred to recovery (...)` instead of
			// claiming a ship that never happened.
			var deferred *PushDeferredError
			if !errors.As(err, &deferred) {
				t.Fatalf("chain-back error: want *PushDeferredError, got %T (%v)", err, err)
			}
			if deferred.Recovery != tc.wantRecovery {
				t.Fatalf("recovery tag: want %q, got %q", tc.wantRecovery, deferred.Recovery)
			}
			if deferred.Project != md.Project || deferred.Run != md.ID {
				t.Fatalf("deferred identity: want (%s,%s), got (%s,%s)",
					md.Project, md.ID, deferred.Project, deferred.Run)
			}
			if capturedDoc != "code" {
				t.Fatalf("docID: want %q, got %q", "code", capturedDoc)
			}
			if !capturedOpts.NeedsSandbox {
				t.Fatalf("NeedsSandbox: want true (chain-back is a code stage)")
			}
			if capturedOpts.SkipNextStage {
				t.Fatalf("SkipNextStage must be false so the post-turn prompt offers push next")
			}
			if capturedOpts.InitialPrompt == "" {
				t.Fatalf("InitialPrompt: want non-empty kickoff, got blank")
			}
		})
	}
}

// TestPushNoHooksDirectoryIsNoOp asserts that the absence of a hooks
// dir is a clean no-op: push succeeds with no "running ..." notice
// and no missing-dir errors logged.
func TestPushNoHooksDirectoryIsNoOp(t *testing.T) {
	f := newPushFixture(t)

	hooksDir := filepath.Join(f.root, "projects", f.projectID, "hooks")
	if _, err := os.Stat(hooksDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture should not have a hooks dir; stat err=%v", err)
	}

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "pre-push hooks:") || strings.Contains(stderr, "pre-push hooks:") {
		t.Fatalf("no scripts to run, but saw pre-push section header in output:\n%s\n%s", stdout, stderr)
	}
}
