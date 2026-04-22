package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// TestPushMergeFFRejectedWhenDefaultMoved: if origin/main advanced
// past the merge-base, the FF push fails and the run stays
// in_progress — no partial cleanup.
func TestPushMergeFFRejectedWhenDefaultMoved(t *testing.T) {
	f := newPushFixture(t)

	// Advance origin/main independently so it's no longer a merge-base.
	work := t.TempDir()
	mustGit(t, "", "clone", "-b", "main", f.origin, work)
	writeFile(t, filepath.Join(work, "other.txt"), "other\n")
	mustGit(t, work, "add", "other.txt")
	mustGit(t, work, "commit", "-m", "divergent")
	mustGit(t, work, "push", "origin", "main")
	movedHead := strings.TrimSpace(mustGitOutput(t, work, "rev-parse", "HEAD"))

	_, stderr, code := f.runInRoot("sdlc", "push", f.projectID, f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero on FF rejection; stderr=%s", stderr)
	}

	// Origin/main untouched, branch still around, sandbox still there,
	// run still in_progress. Failure is all-or-nothing.
	if got := f.originHead(); got != movedHead {
		t.Fatalf("origin/main shouldn't have moved: want %s, got %s", movedHead, got)
	}
	if !f.originHasRef("refs/heads/" + f.branch) {
		t.Fatalf("%s shouldn't be deleted on origin after failed merge", f.branch)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Fatalf("sandbox shouldn't be removed after failed merge")
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status should remain in_progress after FF rejection, got %s", md.Status)
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
	if !strings.Contains(stdout, shortSHA(f.tipSHA)) {
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
	code := promptPushNextStage(next, md, "moe sdlc push tele fix-it", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("promptPushNextStage exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[N/m/p]") {
		t.Fatalf("expected [N/m/p] label in prompt, got: %s", stdout.String())
	}
	return rec
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
