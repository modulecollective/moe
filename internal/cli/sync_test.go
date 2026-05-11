package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/sync"
)

// syncFixture is a bureaucracy with one or more submodules mounted as
// fake projects. Each submodule has a bare "origin" repo on local disk
// so the test can advance it to simulate upstream activity.
type syncFixture struct {
	t           *testing.T
	root        string            // bureaucracy root
	originBySub map[string]string // submodule path -> bare remote dir
	origin      string            // bureaucracy bare remote (set by initBureaucracyOrigin)
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
	gittest.Run(f.t, "", "init", "--bare", "-b", branch, origin)

	// Seed the origin with one commit on <branch>.
	seed := f.t.TempDir()
	gittest.Run(f.t, "", "init", "-b", branch, seed)
	writeFile(f.t, filepath.Join(seed, "README.md"), "seed\n")
	gittest.Run(f.t, seed, "add", "README.md")
	gittest.Run(f.t, seed, "commit", "-m", "seed")
	gittest.Run(f.t, seed, "remote", "add", "origin", origin)
	gittest.Run(f.t, seed, "push", "origin", branch)

	subPath := filepath.Join("projects", id, "src")
	// -c protocol.file.allow=always because `git submodule add` uses a
	// file:// URL in these tests and modern git rejects that by default.
	gittest.Run(f.t, f.root, "-c", "protocol.file.allow=always", "submodule", "add", "-b", branch, origin, subPath)

	projectJSON := filepath.Join(f.root, "projects", id, "project.json")
	writeFile(f.t, projectJSON, `{"id":"`+id+`","default_branch":"`+branch+`"}`+"\n")
	gittest.Run(f.t, f.root, "add", projectJSON, ".gitmodules", subPath)
	gittest.Run(f.t, f.root, "commit", "-m", "Register project "+id)

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
	gittest.Run(f.t, "", "clone", "-b", branch, origin, work)
	writeFile(f.t, filepath.Join(work, "change.txt"), content)
	gittest.Run(f.t, work, "add", "change.txt")
	gittest.Run(f.t, work, "commit", "-m", "advance")
	gittest.Run(f.t, work, "push", "origin", branch)
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
	sha, err := sync.GitlinkSHA(f.root, subPath)
	if err != nil {
		f.t.Fatalf("GitlinkSHA: %v", err)
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
	err := sync.BumpProjectPointers(f.root, &stdout, &stderr)
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
	if !strings.Contains(msg, "proj: "+git.ShortSHA(beforeLink)+".."+git.ShortSHA(newHead)) {
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
	gittest.Run(t, subAbs, "checkout", "main")
	writeFile(t, filepath.Join(subAbs, "local.txt"), "local\n")
	gittest.Run(t, subAbs, "add", "local.txt")
	gittest.Run(t, subAbs, "commit", "-m", "local-only")
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
	gittest.Run(t, "", "init", "--bare", "-b", "main", origin)
	subAbs := filepath.Join(f.root, "projects/proj/src")
	gittest.Run(t, subAbs, "remote", "set-url", "origin", origin)
	// Also blow away the locally cached origin/main ref so fetch must
	// establish it fresh from the (empty) remote.
	gittest.Run(t, subAbs, "update-ref", "-d", "refs/remotes/origin/main")

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
	head, err := sync.HeadSHA(subAbs)
	if err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, subAbs, "checkout", "--detach", head)

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
	gittest.Run(t, f.root, "add", "scratch.txt")

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
	entries, err := git.Status(f.root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.XY == "A " && e.Path == "scratch.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scratch.txt should still be staged, status=%v", entries)
	}
}

// initBureaucracyOrigin sets up a bare remote for the bureaucracy and
// pushes main to it with -u so HasUpstream returns true and subsequent
// `git pull` / `git push` calls have somewhere to go. The bare repo
// lives under t.TempDir() so cleanup is automatic.
func (f *syncFixture) initBureaucracyOrigin() {
	f.t.Helper()
	bare := filepath.Join(f.t.TempDir(), "bureaucracy.git")
	mustGit(f.t, "", "init", "--bare", "-b", "main", bare)
	mustGit(f.t, f.root, "remote", "add", "origin", bare)
	mustGit(f.t, f.root, "push", "-u", "origin", "main")
	f.origin = bare
}

// advanceBureaucracyOrigin commits to the bare bureaucracy remote
// independently of f.root, simulating a sync from another machine.
// Returns the SHA of the new commit on origin/main.
func (f *syncFixture) advanceBureaucracyOrigin(path, content, msg string) string {
	f.t.Helper()
	if f.origin == "" {
		f.t.Fatal("initBureaucracyOrigin not called")
	}
	work := f.t.TempDir()
	mustGit(f.t, "", "clone", "-b", "main", f.origin, work)
	writeFile(f.t, filepath.Join(work, path), content)
	mustGit(f.t, work, "add", path)
	mustGit(f.t, work, "commit", "-m", msg)
	mustGit(f.t, work, "push", "origin", "main")
	out, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		f.t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// originHead returns the SHA at refs/heads/main in the bare remote.
func (f *syncFixture) originHead() string {
	f.t.Helper()
	out, err := exec.Command("git", "-C", f.origin, "rev-parse", "main").Output()
	if err != nil {
		f.t.Fatalf("rev-parse main on origin: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestDoSyncRebasesOverDivergedRemote(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Local: one turn-shaped commit with a MoE-Run trailer, to confirm
	// the trailer survives the rebase replay.
	writeFile(t, filepath.Join(f.root, "local.txt"), "local\n")
	mustGit(t, f.root, "add", "local.txt")
	mustGit(t, f.root, "commit", "-m", "local: add local.txt\n\nMoE-Run: r-local\n")
	localSubject := lastCommitMessage(t, f.root)

	// Remote: a parallel commit, no path overlap.
	remoteSHA := f.advanceBureaucracyOrigin("remote.txt", "remote\n", "remote: add remote.txt")

	var stdout, stderr bytes.Buffer
	if err := doSync(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("doSync: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	// After rebase, the local commit sits on top of the remote tip.
	head := f.bureaucracyHead()
	parentOut, err := exec.Command("git", "-C", f.root, "rev-parse", "HEAD^").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD^: %v", err)
	}
	parent := strings.TrimSpace(string(parentOut))
	if parent != remoteSHA {
		t.Fatalf("rebased HEAD doesn't sit on remote tip: want parent %s, got %s", remoteSHA, parent)
	}

	// Both files materialised.
	for _, p := range []string{"local.txt", "remote.txt"} {
		if _, err := os.Stat(filepath.Join(f.root, p)); err != nil {
			t.Fatalf("missing %s after rebase: %v", p, err)
		}
	}

	// Trailer ridden along on the replayed commit.
	replayed := lastCommitMessage(t, f.root)
	if !strings.Contains(replayed, "MoE-Run: r-local") {
		t.Fatalf("trailer not preserved on replay:\nbefore=%q\nafter=%q", localSubject, replayed)
	}

	// Sync also pushed: origin advanced to the new local HEAD.
	if got := f.originHead(); got != head {
		t.Fatalf("origin didn't receive rebased push: want %s, got %s", head, got)
	}
}

func TestDoSyncRebaseConflictHaltsWithRecovery(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Seed a shared file on both sides so the rebase has a hunk to
	// conflict on.
	shared := filepath.Join(f.root, "shared.txt")
	writeFile(t, shared, "base\n")
	mustGit(t, f.root, "add", "shared.txt")
	mustGit(t, f.root, "commit", "-m", "base: add shared.txt")
	mustGit(t, f.root, "push", "origin", "main")

	// Local: rewrite shared.txt.
	writeFile(t, shared, "local\n")
	mustGit(t, f.root, "add", "shared.txt")
	mustGit(t, f.root, "commit", "-m", "local: rewrite shared.txt")

	// Remote: rewrite the same line independently.
	f.advanceBureaucracyOrigin("shared.txt", "remote\n", "remote: rewrite shared.txt")

	var stdout, stderr bytes.Buffer
	err := doSync(f.root, &stdout, &stderr)
	if err == nil {
		t.Fatalf("doSync: expected error\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !rebaseInProgress(f.root) {
		t.Fatal("expected worktree to be left in rebase-in-progress state")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rebase --continue") {
		t.Fatalf("recovery hint missing from error: %v", err)
	}
	if !strings.Contains(msg, "shared.txt") {
		t.Fatalf("error doesn't name the conflicting path: %v", err)
	}
}

func TestDoSyncRefusesWhenRebaseInProgress(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Plant a rebase-merge directory directly. doSync should refuse
	// without touching the network.
	if err := os.MkdirAll(filepath.Join(f.root, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	headBefore := f.bureaucracyHead()
	originBefore := f.originHead()

	var stdout, stderr bytes.Buffer
	err := doSync(f.root, &stdout, &stderr)
	if err == nil {
		t.Fatal("doSync: expected refusal")
	}
	if !strings.Contains(err.Error(), "rebase is in progress") {
		t.Fatalf("error wrong shape: %v", err)
	}
	if !strings.Contains(err.Error(), "rebase --continue") {
		t.Fatalf("error missing recovery hint: %v", err)
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatal("HEAD moved despite refusal")
	}
	if f.originHead() != originBefore {
		t.Fatal("origin advanced despite refusal")
	}
}

func TestDoSyncNoopWhenUpToDate(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()
	headBefore := f.bureaucracyHead()

	var stdout, stderr bytes.Buffer
	if err := doSync(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("doSync: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatalf("HEAD moved on no-op sync: %s -> %s", headBefore, f.bureaucracyHead())
	}
}

func TestDoSyncAheadOnlyPushes(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	writeFile(t, filepath.Join(f.root, "local.txt"), "local\n")
	mustGit(t, f.root, "add", "local.txt")
	mustGit(t, f.root, "commit", "-m", "local: add local.txt")
	local := f.bureaucracyHead()

	var stdout, stderr bytes.Buffer
	if err := doSync(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("doSync: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if f.bureaucracyHead() != local {
		t.Fatal("HEAD should be unchanged on ahead-only sync")
	}
	if got := f.originHead(); got != local {
		t.Fatalf("origin didn't receive push: want %s, got %s", local, got)
	}
}

// ---- small test utilities ----

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
