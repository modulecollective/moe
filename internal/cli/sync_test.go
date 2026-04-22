package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syncFixture is a bureaucracy with one or more submodules mounted as
// fake projects. Each submodule has a bare "origin" repo on local disk
// so the test can advance it to simulate upstream activity.
type syncFixture struct {
	t           *testing.T
	root        string            // bureaucracy root
	originBySub map[string]string // submodule path -> bare remote dir
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	root := newTestBureaucracy(t)
	return &syncFixture{t: t, root: root, originBySub: map[string]string{}}
}

// addProjectSubmodule creates a bare "remote" repo with one seed commit,
// registers it as a submodule at projects/<id>/src, and writes a minimal
// projects/<id>/project.json so resolveTrackingBranch finds a
// default_branch. Returns the absolute path to the bare origin so the
// test can advance it independently.
func (f *syncFixture) addProjectSubmodule(id, branch string) string {
	f.t.Helper()
	origin := filepath.Join(f.t.TempDir(), id+".git")
	mustGit(f.t, "", "init", "--bare", "-b", branch, origin)

	// Seed the origin with one commit on <branch>.
	seed := f.t.TempDir()
	mustGit(f.t, "", "init", "-b", branch, seed)
	writeFile(f.t, filepath.Join(seed, "README.md"), "seed\n")
	mustGit(f.t, seed, "add", "README.md")
	mustGit(f.t, seed, "commit", "-m", "seed")
	mustGit(f.t, seed, "remote", "add", "origin", origin)
	mustGit(f.t, seed, "push", "origin", branch)

	subPath := filepath.Join("projects", id, "src")
	// -c protocol.file.allow=always because `git submodule add` uses a
	// file:// URL in these tests and modern git rejects that by default.
	mustGit(f.t, f.root, "-c", "protocol.file.allow=always", "submodule", "add", "-b", branch, origin, subPath)

	projectJSON := filepath.Join(f.root, "projects", id, "project.json")
	writeFile(f.t, projectJSON, `{"id":"`+id+`","default_branch":"`+branch+`"}`+"\n")
	mustGit(f.t, f.root, "add", projectJSON, ".gitmodules", subPath)
	mustGit(f.t, f.root, "commit", "-m", "Register project "+id)

	f.originBySub[subPath] = origin
	return origin
}

// advanceOrigin pushes a new commit to the submodule's bare origin on
// the given branch, without touching the bureaucracy's checkout. That
// simulates someone merging a PR on GitHub.
func (f *syncFixture) advanceOrigin(id, branch, content string) string {
	f.t.Helper()
	origin := f.originBySub[filepath.Join("projects", id, "src")]
	if origin == "" {
		f.t.Fatalf("no origin registered for project %s", id)
	}
	work := f.t.TempDir()
	mustGit(f.t, "", "clone", "-b", branch, origin, work)
	writeFile(f.t, filepath.Join(work, "change.txt"), content)
	mustGit(f.t, work, "add", "change.txt")
	mustGit(f.t, work, "commit", "-m", "advance")
	mustGit(f.t, work, "push", "origin", branch)
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		f.t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (f *syncFixture) bureaucracyHead() string {
	f.t.Helper()
	out, err := exec.Command("git", "-C", f.root, "rev-parse", "HEAD").Output()
	if err != nil {
		f.t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (f *syncFixture) gitlink(subPath string) string {
	f.t.Helper()
	sha, err := gitlinkSHA(f.root, subPath)
	if err != nil {
		f.t.Fatalf("gitlinkSHA: %v", err)
	}
	return sha
}

// runBump invokes bumpProjectPointers directly against the fixture and
// returns its stdout/stderr capture and error. We test this helper
// rather than runSync end-to-end so we don't need an upstream for the
// bureaucracy.
func (f *syncFixture) runBump() (string, string, error) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	err := bumpProjectPointers(f.root, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestBumpProjectPointersHappyPath(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	before := f.bureaucracyHead()
	beforeLink := f.gitlink("projects/proj/src")

	newHead := f.advanceOrigin("proj", "main", "hello\n")

	out, errOut, err := f.runBump()
	if err != nil {
		t.Fatalf("bump: %v\nstdout=%s\nstderr=%s", err, out, errOut)
	}

	after := f.bureaucracyHead()
	if after == before {
		t.Fatalf("expected a pointer-bump commit on bureaucracy; HEAD unchanged at %s", after)
	}
	afterLink := f.gitlink("projects/proj/src")
	if afterLink != newHead {
		t.Fatalf("gitlink didn't advance: want %s, got %s (was %s)", newHead, afterLink, beforeLink)
	}

	// Commit message should list the bumped project by id with short SHAs.
	msg := lastCommitMessage(t, f.root)
	if !strings.Contains(msg, "sync: bump project pointers") {
		t.Fatalf("commit subject wrong:\n%s", msg)
	}
	if !strings.Contains(msg, "proj: "+shortSHA(beforeLink)+".."+shortSHA(newHead)) {
		t.Fatalf("commit body missing proj bump:\n%s", msg)
	}
}

func TestBumpProjectPointersNoopWhenCaughtUp(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	before := f.bureaucracyHead()
	beforeLink := f.gitlink("projects/proj/src")

	if _, _, err := f.runBump(); err != nil {
		t.Fatalf("bump: %v", err)
	}

	if after := f.bureaucracyHead(); after != before {
		t.Fatalf("no-op sync made a commit: %s -> %s", before, after)
	}
	if afterLink := f.gitlink("projects/proj/src"); afterLink != beforeLink {
		t.Fatalf("gitlink changed unexpectedly: %s -> %s", beforeLink, afterLink)
	}
}

func TestBumpProjectPointersAbortsOnDirtySubmodule(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	f.advanceOrigin("proj", "main", "hello\n")

	// Dirty the checkout. An untracked file is enough for `git status
	// --porcelain` to return non-empty.
	writeFile(t, filepath.Join(f.root, "projects/proj/src", "scratch.txt"), "junk\n")

	before := f.bureaucracyHead()
	out, errOut, err := f.runBump()
	if err == nil {
		t.Fatalf("expected abort; stdout=%s stderr=%s", out, errOut)
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("abort message wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "Recovery:") {
		t.Fatalf("abort message missing recovery steps: %v", err)
	}
	if after := f.bureaucracyHead(); after != before {
		t.Fatalf("bureaucracy committed despite abort: %s -> %s", before, after)
	}
}

func TestBumpProjectPointersAbortsOnDivergedSubmodule(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")

	subAbs := filepath.Join(f.root, "projects/proj/src")
	// Start a local commit that's NOT on origin, then advance origin
	// separately so the two have diverged with no fast-forward path.
	mustGit(t, subAbs, "checkout", "main")
	writeFile(t, filepath.Join(subAbs, "local.txt"), "local\n")
	mustGit(t, subAbs, "add", "local.txt")
	mustGit(t, subAbs, "commit", "-m", "local-only")
	f.advanceOrigin("proj", "main", "remote\n")

	before := f.bureaucracyHead()
	_, _, err := f.runBump()
	if err == nil {
		t.Fatal("expected abort on divergence")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("abort message wrong: %v", err)
	}
	if after := f.bureaucracyHead(); after != before {
		t.Fatalf("bureaucracy committed despite abort: %s -> %s", before, after)
	}
}

func TestBumpProjectPointersAbortsOnMissingOriginBranch(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")

	// Remove origin/main so rev-parse fails. Fetching would re-populate
	// it; we simulate a remote that doesn't have our branch by replacing
	// the remote URL with an empty bare repo.
	origin := filepath.Join(t.TempDir(), "empty.git")
	mustGit(t, "", "init", "--bare", "-b", "main", origin)
	subAbs := filepath.Join(f.root, "projects/proj/src")
	mustGit(t, subAbs, "remote", "set-url", "origin", origin)
	// Also blow away the locally cached origin/main ref so fetch must
	// establish it fresh from the (empty) remote.
	mustGit(t, subAbs, "update-ref", "-d", "refs/remotes/origin/main")

	_, _, err := f.runBump()
	if err == nil {
		t.Fatal("expected abort when origin has no main")
	}
	if !strings.Contains(err.Error(), "no origin/main") {
		t.Fatalf("abort message wrong: %v", err)
	}
}

func TestBumpProjectPointersAbortsOnFirstDirtyStopsSecond(t *testing.T) {
	f := newSyncFixture(t)
	// Alphabetical order matters: readGitmoduleEntries walks `.gitmodules`
	// in file order, and `git submodule add` appends at the bottom, so
	// "aaa" is visited before "bbb".
	f.addProjectSubmodule("aaa", "main")
	f.addProjectSubmodule("bbb", "main")
	f.advanceOrigin("aaa", "main", "aaa change\n")
	f.advanceOrigin("bbb", "main", "bbb change\n")

	// Make "aaa" dirty; "bbb" is clean and ready to bump.
	writeFile(t, filepath.Join(f.root, "projects/aaa/src", "scratch.txt"), "junk\n")

	before := f.bureaucracyHead()
	bbbBeforeLink := f.gitlink("projects/bbb/src")

	_, _, err := f.runBump()
	if err == nil {
		t.Fatal("expected abort from dirty aaa")
	}
	if !strings.Contains(err.Error(), "projects/aaa/src") {
		t.Fatalf("abort doesn't name aaa: %v", err)
	}

	if after := f.bureaucracyHead(); after != before {
		t.Fatalf("bureaucracy committed despite abort: %s -> %s", before, after)
	}
	// bbb's gitlink should be untouched: we never got to it.
	if bbbAfter := f.gitlink("projects/bbb/src"); bbbAfter != bbbBeforeLink {
		t.Fatalf("bbb gitlink changed despite abort on aaa: %s -> %s", bbbBeforeLink, bbbAfter)
	}
}

func TestBumpProjectPointersMultipleBumpsOneCommit(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("alpha", "main")
	f.addProjectSubmodule("beta", "main")
	alphaNew := f.advanceOrigin("alpha", "main", "alpha\n")
	betaNew := f.advanceOrigin("beta", "main", "beta\n")

	before := f.bureaucracyHead()
	if _, _, err := f.runBump(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	after := f.bureaucracyHead()
	if after == before {
		t.Fatal("expected one commit on bureaucracy")
	}

	// Exactly one new commit, carrying both bumps in the body.
	msg := lastCommitMessage(t, f.root)
	if !strings.Contains(msg, "alpha: ") || !strings.Contains(msg, "beta: ") {
		t.Fatalf("commit message missing one of the projects:\n%s", msg)
	}

	// Confirm both gitlinks moved to the new heads.
	if got := f.gitlink("projects/alpha/src"); got != alphaNew {
		t.Fatalf("alpha gitlink: want %s, got %s", alphaNew, got)
	}
	if got := f.gitlink("projects/beta/src"); got != betaNew {
		t.Fatalf("beta gitlink: want %s, got %s", betaNew, got)
	}
}

func TestBumpProjectPointersRecoversFromDetachedHead(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	newHead := f.advanceOrigin("proj", "main", "change\n")

	// Put the submodule in detached HEAD — git submodule update does
	// this after a pull, and our algorithm needs to handle it rather
	// than leaving a detached HEAD that git merge --ff-only refuses.
	subAbs := filepath.Join(f.root, "projects/proj/src")
	head, err := gitHeadSHA(subAbs)
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, subAbs, "checkout", "--detach", head)

	if _, _, err := f.runBump(); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if got := f.gitlink("projects/proj/src"); got != newHead {
		t.Fatalf("gitlink: want %s, got %s", newHead, got)
	}
}

// If the operator has unrelated staged changes when sync runs, the
// pointer-bump commit must not sweep them in under a "sync: bump
// project pointers" subject. The commit is scoped to the gitlink paths
// and leaves anything else still staged for the operator to handle.
func TestBumpProjectPointersIgnoresUnrelatedStagedChanges(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	f.advanceOrigin("proj", "main", "change\n")

	scratch := filepath.Join(f.root, "scratch.txt")
	writeFile(t, scratch, "operator's in-progress work\n")
	mustGit(t, f.root, "add", "scratch.txt")

	if _, _, err := f.runBump(); err != nil {
		t.Fatalf("bump: %v", err)
	}

	// The sync commit should touch only the submodule gitlink, not scratch.txt.
	out, err := exec.Command("git", "-C", f.root, "show", "--name-only", "--format=", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	touched := strings.Fields(strings.TrimSpace(string(out)))
	if len(touched) != 1 || touched[0] != "projects/proj/src" {
		t.Fatalf("sync commit touched unexpected paths: %v", touched)
	}
	// scratch.txt should still be staged, not committed.
	statusOut, err := exec.Command("git", "-C", f.root, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statusOut), "A  scratch.txt") {
		t.Fatalf("scratch.txt should still be staged, status=%s", statusOut)
	}
}

func TestReadGitmoduleEntriesIncludesBranch(t *testing.T) {
	dir := t.TempDir()
	content := `[submodule "foo"]
	path = projects/foo/src
	url = https://example.com/foo.git
	branch = trunk
[submodule "bar"]
	path = projects/bar/src
	url = https://example.com/bar.git
`
	writeFile(t, filepath.Join(dir, ".gitmodules"), content)

	got, err := readGitmoduleEntries(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	if got[0].branch != "trunk" {
		t.Fatalf("foo branch: want trunk, got %q", got[0].branch)
	}
	if got[1].branch != "" {
		t.Fatalf("bar branch: want empty (so resolver falls back to main), got %q", got[1].branch)
	}
}

func TestProjectIDForSubmodulePath(t *testing.T) {
	cases := map[string]string{
		"projects/moe/src":   "moe",
		"projects/foo-bar/src": "foo-bar",
		"projects/moe":       "",   // not the canonical shape
		"vendor/thing":       "",
		"":                   "",
	}
	for in, want := range cases {
		if got := projectIDForSubmodulePath(in); got != want {
			t.Errorf("projectIDForSubmodulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- small test utilities ----

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lastCommitMessage(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.TrimSpace(string(out))
}
