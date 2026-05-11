package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
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
	mustGit(t, "", "init", "--bare", "-b", "main", origin)
	seed := t.TempDir()
	mustGit(t, "", "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	mustGit(t, seed, "add", "README.md")
	mustGit(t, seed, "commit", "-m", "seed")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "origin", "main")

	// Register as submodule so project.Load / sandbox.Ensure find it.
	subPath := filepath.Join("projects", projectID, "src")
	mustGit(t, root, "-c", "protocol.file.allow=always",
		"submodule", "add", "-b", "main", origin, subPath)

	// Seed the run first — seedRun writes a minimal project.json, so
	// we overwrite it afterwards with the fields push/project.Load need.
	seedRun(t, root, projectID, runID, "sdlc", run.StatusInProgress)
	writeFile(t, filepath.Join(root, "projects", projectID, "project.json"),
		`{"id":"`+projectID+`","submodule":"`+subPath+`","remote":"`+origin+`","default_branch":"main"}`+"\n")
	mustGit(t, root, "add", ".gitmodules", subPath, filepath.Join("projects", projectID, "project.json"))
	mustGit(t, root, "commit", "-m", "Register project "+projectID)
	writeContent(t, root, projectID, runID, "code", "# code doc\n")
	mustGit(t, root, "add", filepath.Join("projects", projectID, "runs", runID, "documents", "code", "content.md"))
	mustGit(t, root, "commit", "-m", "work: update code\n\nMoE-Run: "+runID+"\nMoE-Document: code\n")

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
	mustGit(t, clonePath, "add", "feature.txt")
	mustGit(t, clonePath, "commit", "-m", "add feature")
	tip, err := exec.Command("git", "-C", clonePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	tipSHA := strings.TrimSpace(string(tip))

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
	err := exec.Command("git", "-C", f.origin, "rev-parse", "--verify", "--quiet", ref).Run()
	return err == nil
}

// originHead returns the SHA at origin's main.
func (f *pushFixture) originHead() string {
	f.t.Helper()
	out, err := exec.Command("git", "-C", f.origin, "rev-parse", "main").Output()
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
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
// flips to StatusMerged, the sandbox is gone, and the remote branch
// is deleted.
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
	mustGit(t, "", "clone", "-b", "main", f.origin, work)
	writeFile(t, filepath.Join(work, "other.txt"), "other\n")
	mustGit(t, work, "add", "other.txt")
	mustGit(t, work, "commit", "-m", "divergent")
	mustGit(t, work, "push", "origin", "main")
	movedBaseSHA := strings.TrimSpace(mustGitOutput(t, work, "rev-parse", "HEAD"))

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
	headOut := strings.TrimSpace(mustGitOutput(t, "", "-C", f.origin, "rev-parse", "main"))
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
	mustGit(t, "", "clone", "-b", "main", f.origin, work)
	writeFile(t, filepath.Join(work, "feature.txt"), "from-default\n")
	mustGit(t, work, "add", "feature.txt")
	mustGit(t, work, "commit", "-m", "default-side feature")
	mustGit(t, work, "push", "origin", "main")
	movedHead := strings.TrimSpace(mustGitOutput(t, work, "rev-parse", "HEAD"))

	// Stub the chain-back: capture the conflict context, do not actually
	// launch Claude (the test process has no terminal/agent).
	var captured *push.RebaseConflictError
	var capturedRun *run.Metadata
	prev := openCodeSessionForRebaseConflict
	openCodeSessionForRebaseConflict = func(md *run.Metadata, c *push.RebaseConflictError, _, _ io.Writer) int {
		capturedRun = md
		captured = c
		return 1
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

// TestPushFailsCleanlyWhenNoEditorAtMerge: the merge path now requires
// $EDITOR (it pops on followups.md before harvest). With neither
// $EDITOR nor $VISUAL set, push fails with the same error message
// close emits, the run stays in_progress, and origin/main is unchanged.
func TestPushFailsCleanlyWhenNoEditorAtMerge(t *testing.T) {
	f := newPushFixture(t)
	noEditor(t)

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
	mustGit(t, f.root, "add", filepath.Join("projects", f.projectID, "project.json"))
	mustGit(t, f.root, "commit", "-m", "use GitHub-shaped remote for --pr test")

	fakeGh(t, nil)

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
	mustGit(t, f.root, "add", filepath.Join("projects", f.projectID, "runs", f.runID, "run.json"))
	mustGit(t, f.root, "commit", "-m", "sync: close\n\nMoE-Run: "+f.runID+"\nMoE-Closed: https://example.com/pr/1\n")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code != 0 {
		t.Fatalf("rerun: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "already closed") {
		t.Fatalf("expected 'already closed' notice, got: %s", stdout)
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

// TestPromptPushNextStagePrintsCodeCanvas: when the code canvas
// exists on disk, its bytes appear above the [N/m/p] prompt verbatim
// (no header, no decoration). follow no longer surfaces the code
// canvas during the code stage, so this is the canvas's one chance
// to land in front of the operator at the merge decision.
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
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "code"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "## What changed\n\nReplaced the canvas pager with hunk.\n"
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
	if code := promptPushNextStage(next, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
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
	if code := promptPushNextStage(next, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
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
	if code := promptPushNextStage(next, root, md, "moe sdlc push tele fix-it", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if strings.HasPrefix(got, "\n") {
		t.Errorf("whitespace canvas should not pad the prompt with blank lines; got:\n%q", got)
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
	code := promptPushNextStage(next, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("promptPushNextStage exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[N/m/p]") {
		t.Fatalf("expected [N/m/p] label in prompt, got: %s", stdout.String())
	}
	return rec
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
	if !strings.Contains(stdout, "running pre-push hook ") {
		t.Errorf("stdout missing 'running pre-push hook' notice:\n%s", stdout)
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
	openCodeSessionForHookFailure = func(md *run.Metadata, fail *hookFailure, _, _ io.Writer) int {
		capturedRun = md
		captured = fail
		return 1
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
		name        string
		invoke      func(md *run.Metadata) int
		stubReturns int
	}{
		{
			name: "hook failure",
			invoke: func(md *run.Metadata) int {
				return openCodeSessionForHookFailure(md, &hookFailure{
					event:  hookEventPrePush,
					script: "10-fail.sh",
					output: "boom\n",
				}, io.Discard, io.Discard)
			},
			stubReturns: 0,
		},
		{
			name: "hook failure (inner non-zero)",
			invoke: func(md *run.Metadata) int {
				return openCodeSessionForHookFailure(md, &hookFailure{
					event:  hookEventPrePush,
					script: "10-fail.sh",
					output: "boom\n",
				}, io.Discard, io.Discard)
			},
			stubReturns: 1,
		},
		{
			name: "rebase conflict",
			invoke: func(md *run.Metadata) int {
				return openCodeSessionForRebaseConflict(md, &push.RebaseConflictError{
					Branch:        "moe/fix-it",
					DefaultBranch: "main",
					Conflicts:     []string{"feature.txt"},
				}, io.Discard, io.Discard)
			},
			stubReturns: 0,
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
			got := tc.invoke(md)
			if got != tc.stubReturns {
				t.Fatalf("chain-back exit code: want propagated %d, got %d", tc.stubReturns, got)
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
	if strings.Contains(stdout, "running pre-push hook ") || strings.Contains(stderr, "running pre-push hook ") {
		t.Fatalf("no scripts to run, but saw 'running pre-push hook' in output:\n%s\n%s", stdout, stderr)
	}
}

// mustGitOutput runs git and returns its stdout, failing the test on error.
func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}
