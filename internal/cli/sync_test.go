package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
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
	return gittest.HeadSHA(f.t, work)
}

func (f *syncFixture) bureaucracyHead() string {
	f.t.Helper()
	return gittest.HeadSHA(f.t, f.root)
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
	out := gittest.Output(t, f.root, "show", "--name-only", "--format=", "HEAD")
	touched := strings.Fields(out)
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
	f.origin = initBureaucracyOriginAt(f.t, f.root)
}

// initBureaucracyOriginAt is the fixture-free form, for tests that build
// their bureaucracy some other way and still need an upstream — the
// pulse's reconcile step, whose whole question is whether the journal
// push fired. Returns the bare repo's path.
func initBureaucracyOriginAt(t *testing.T, root string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "bureaucracy.git")
	gittest.Run(t, "", "init", "--bare", "-b", "main", bare)
	gittest.Run(t, root, "remote", "add", "origin", bare)
	gittest.Run(t, root, "push", "-u", "origin", "main")
	return bare
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
	gittest.Run(f.t, "", "clone", "-b", "main", f.origin, work)
	writeFile(f.t, filepath.Join(work, path), content)
	gittest.Run(f.t, work, "add", path)
	gittest.Run(f.t, work, "commit", "-m", msg)
	gittest.Run(f.t, work, "push", "origin", "main")
	return gittest.HeadSHA(f.t, work)
}

// originHead returns the SHA at refs/heads/main in the bare remote.
func (f *syncFixture) originHead() string {
	f.t.Helper()
	return gittest.Output(f.t, f.origin, "rev-parse", "main")
}

func TestDoSyncRebasesOverDivergedRemote(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Local: one turn-shaped commit with a MoE-Run trailer, to confirm
	// the trailer survives the rebase replay.
	writeFile(t, filepath.Join(f.root, "local.txt"), "local\n")
	gittest.Run(t, f.root, "add", "local.txt")
	gittest.Run(t, f.root, "commit", "-m", "local: add local.txt\n\nMoE-Run: r-local\n")
	localSubject := lastCommitMessage(t, f.root)

	// Remote: a parallel commit, no path overlap.
	remoteSHA := f.advanceBureaucracyOrigin("remote.txt", "remote\n", "remote: add remote.txt")

	var stdout, stderr bytes.Buffer
	if err := doSync(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("doSync: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	// After rebase, the local commit sits on top of the remote tip.
	head := f.bureaucracyHead()
	parent := gittest.Output(t, f.root, "rev-parse", "HEAD^")
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

// TestDoSyncDedupesDuplicateBumpQuietly is the regression guard for
// bureaucracy-repo-sync-issue. When sync's rebasing pull drops a
// duplicate "sync: bump project pointers" commit — machine A and machine
// B independently recorded the same gitlink move, so the two bumps are
// patch-identical — git prints a "skipped previously applied commit"
// warning followed by two --reapply-cherry-picks hints. The hints are
// actively misleading here: reapplying would force an empty/conflicting
// duplicate bump back in. The -c advice.skippedCherryPicks=false on the
// pull suppresses the advice while leaving the bare warning as honest
// signal.
//
// The assertion is on the ABSENCE of the hint, not the presence of the
// warning. The warning is git's and we keep it deliberately; if a future
// git folds it under the same advice key and the output goes fully
// silent, that's still acceptable and still passes.
func TestDoSyncDedupesDuplicateBumpQuietly(t *testing.T) {
	f := newSyncFixture(t)
	f.addProjectSubmodule("proj", "main")
	fromSHA := f.gitlink("projects/proj/src")
	f.initBureaucracyOrigin()

	// A real new submodule commit the bump moves the gitlink to.
	// advanceOrigin pushes it to the submodule's origin, so the
	// post-pull BumpProjectPointers fast-forwards the submodule to it and
	// finds the gitlink already there — a clean no-op, leaving the
	// rebase's converged state as the only thing under test.
	toSHA := f.advanceOrigin("proj", "main", "dedup\n")

	// Two patch-identical bump commits — same gitlink move
	// fromSHA->toSHA — one on local main, one on origin/main. Identical
	// diff, distinct author/committer dates so they're different commits;
	// the rebase recognises local's as already applied upstream (as C2)
	// and skips it. cacheinfo writes the gitlink without needing the
	// submodule object present, which is all a pointer bump records.
	body := "sync: bump project pointers\n\nproj: " + git.ShortSHA(fromSHA) + ".." + git.ShortSHA(toSHA) + "\n"
	bumpCommit := func(dir, date string) {
		gittest.Run(t, dir, "update-index", "--cacheinfo", "160000,"+toSHA+",projects/proj/src")
		env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		gittest.RunWithEnv(t, dir, env, "commit", "-m", body)
	}

	// origin's bump lands first, on a throwaway clone of the bureaucracy
	// remote; then local's, with an earlier date so the two SHAs differ.
	originWork := t.TempDir()
	gittest.Run(t, "", "clone", "-b", "main", f.origin, originWork)
	bumpCommit(originWork, "2026-06-02T11:00:00")
	gittest.Run(t, originWork, "push", "origin", "main")

	bumpCommit(f.root, "2026-06-01T10:00:00")

	var stdout, stderr bytes.Buffer
	if err := doSync(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("doSync: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	// The gitlink converged to the shared target — nothing lost.
	if got := f.gitlink("projects/proj/src"); got != toSHA {
		t.Fatalf("gitlink didn't converge: want %s, got %s", toSHA, got)
	}

	// The misleading cherry-pick advice is gone. Check both the hint text
	// and the config key the hint names, so the test fails if someone
	// drops the -c flag and git's default advice comes back.
	out := stdout.String() + stderr.String()
	if strings.Contains(out, "reapply-cherry-picks") {
		t.Fatalf("cherry-pick hint leaked into sync output:\n%s", out)
	}
	if strings.Contains(out, "advice.skippedCherryPicks") {
		t.Fatalf("advice config hint leaked into sync output:\n%s", out)
	}
}

func TestDoSyncRebaseConflictHaltsWithRecovery(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Seed a shared file on both sides so the rebase has a hunk to
	// conflict on.
	shared := filepath.Join(f.root, "shared.txt")
	writeFile(t, shared, "base\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "base: add shared.txt")
	gittest.Run(t, f.root, "push", "origin", "main")

	// Local: rewrite shared.txt.
	writeFile(t, shared, "local\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "local: rewrite shared.txt")

	// Remote: rewrite the same line independently.
	f.advanceBureaucracyOrigin("shared.txt", "remote\n", "remote: rewrite shared.txt")

	var stdout, stderr bytes.Buffer
	err := doSync(f.root, &stdout, &stderr)
	if err == nil {
		t.Fatalf("doSync: expected error\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !sync.RebaseInProgress(f.root) {
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
	gittest.Run(t, f.root, "add", "local.txt")
	gittest.Run(t, f.root, "commit", "-m", "local: add local.txt")
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

// TestAutoPushAheadOnlyPushesAndIsCheap is the happy-path session-close
// shape: a turn commit lives on local main, AutoPush gets it to origin
// without doing pointer bumps or PR reconciliation (those are sync's
// job). Asserts the local HEAD reaches origin and that no extra
// bureaucracy commit was created (sync would have made one if it ran).
func TestAutoPushAheadOnlyPushesAndIsCheap(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	writeFile(t, filepath.Join(f.root, "turn.txt"), "turn\n")
	gittest.Run(t, f.root, "add", "turn.txt")
	gittest.Run(t, f.root, "commit", "-m", "work: turn commit")
	local := f.bureaucracyHead()

	var stdout, stderr bytes.Buffer
	if err := sync.AutoPush(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPush: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if got := f.originHead(); got != local {
		t.Fatalf("origin didn't receive push: want %s, got %s", local, got)
	}
	if f.bureaucracyHead() != local {
		t.Fatalf("AutoPush mutated bureaucracy HEAD: %s -> %s (it should only push, not bump)", local, f.bureaucracyHead())
	}
}

// TestAutoPushNoUpstreamIsSilentNoop is the brand-new-branch case: no
// @{u} configured, AutoPush returns nil without trying to push.
func TestAutoPushNoUpstreamIsSilentNoop(t *testing.T) {
	f := newSyncFixture(t)
	// Deliberately no initBureaucracyOrigin — main has no upstream.

	var stdout, stderr bytes.Buffer
	if err := sync.AutoPush(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPush: %v\nstderr=%s", err, stderr.String())
	}
	if stderr.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("expected silent no-op, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestAutoPushWarnsAndContinuesOnNetworkFailure: origin is unreachable
// (path that doesn't exist). AutoPush must not fail the turn — it warns
// to stderr and returns nil. Local HEAD must be untouched.
func TestAutoPushWarnsAndContinuesOnNetworkFailure(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()
	// Repoint origin at a path that doesn't exist so push fails locally
	// (no network needed to reproduce).
	bogus := filepath.Join(t.TempDir(), "does-not-exist.git")
	gittest.Run(t, f.root, "remote", "set-url", "origin", bogus)

	writeFile(t, filepath.Join(f.root, "turn.txt"), "turn\n")
	gittest.Run(t, f.root, "add", "turn.txt")
	gittest.Run(t, f.root, "commit", "-m", "work: turn commit")
	headBefore := f.bureaucracyHead()

	var stdout, stderr bytes.Buffer
	if err := sync.AutoPush(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPush returned %v; should warn and continue on network failure", err)
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatal("AutoPush mutated HEAD on a failure path")
	}
	if !strings.Contains(stderr.String(), "[auto-sync skipped]") {
		t.Fatalf("expected warn line on stderr, got %q", stderr.String())
	}
}

// TestAutoPullPullsRebasedRemoteHead: remote advanced independently;
// AutoPull rebases local onto origin/main without any local divergence.
func TestAutoPullPullsRebasedRemoteHead(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	remoteSHA := f.advanceBureaucracyOrigin("remote.txt", "remote\n", "remote: add remote.txt")

	var stdout, stderr bytes.Buffer
	if err := sync.AutoPull(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPull: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if got := f.bureaucracyHead(); got != remoteSHA {
		t.Fatalf("local HEAD didn't advance to remote: want %s, got %s", remoteSHA, got)
	}
}

// TestAutoPullNoUpstreamIsSilentNoop: brand-new branch with no @{u}.
func TestAutoPullNoUpstreamIsSilentNoop(t *testing.T) {
	f := newSyncFixture(t)

	headBefore := f.bureaucracyHead()
	var stdout, stderr bytes.Buffer
	if err := sync.AutoPull(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPull: %v\nstderr=%s", err, stderr.String())
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatal("AutoPull moved HEAD without an upstream configured")
	}
	if stderr.Len() != 0 || stdout.Len() != 0 {
		t.Fatalf("expected silent no-op, got stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestAutoPullRefusesWhenRebaseInProgress: a pre-existing rebase halts
// the auto-pull with the recovery prose, same contract as doSync.
func TestAutoPullRefusesWhenRebaseInProgress(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	if err := os.MkdirAll(filepath.Join(f.root, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	headBefore := f.bureaucracyHead()

	var stdout, stderr bytes.Buffer
	err := sync.AutoPull(f.root, &stdout, &stderr)
	if err == nil {
		t.Fatal("AutoPull: expected refusal on pre-existing rebase")
	}
	if !strings.Contains(err.Error(), "rebase is in progress") {
		t.Fatalf("error shape wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "rebase --continue") {
		t.Fatalf("error missing recovery hint: %v", err)
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatal("HEAD moved despite refusal")
	}
}

// TestAutoPullHaltsOnRebaseConflict: divergent commits on the same path
// cause a real rebase conflict; AutoPull halts with the recovery prose,
// leaving the worktree mid-rebase for the operator to resolve.
func TestAutoPullHaltsOnRebaseConflict(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	shared := filepath.Join(f.root, "shared.txt")
	writeFile(t, shared, "base\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "base: add shared.txt")
	gittest.Run(t, f.root, "push", "origin", "main")

	writeFile(t, shared, "local\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "local: rewrite shared.txt")

	f.advanceBureaucracyOrigin("shared.txt", "remote\n", "remote: rewrite shared.txt")

	var stdout, stderr bytes.Buffer
	err := sync.AutoPull(f.root, &stdout, &stderr)
	if err == nil {
		t.Fatalf("AutoPull: expected halt on conflict\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !sync.RebaseInProgress(f.root) {
		t.Fatal("expected worktree left mid-rebase for operator")
	}
	if !strings.Contains(err.Error(), "rebase --continue") {
		t.Fatalf("recovery hint missing: %v", err)
	}
}

// TestOpenWikiSessionRunsAutoPullBeforeSessionOpen pins the
// session-open wiring: openWikiSession must auto-pull from origin
// before it lays the session worktree down, so the agent's first turn
// starts from current state. Setup advances origin beyond what's local;
// after openWikiSession, local main must have caught up.
func TestOpenWikiSessionRunsAutoPullBeforeSessionOpen(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()
	remoteSHA := f.advanceBureaucracyOrigin("remote.txt", "remote\n", "remote: add remote.txt")

	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "auto-pull-test",
		DocID:       "design",
		LockPurpose: "stage",
	}
	var stdout, stderr bytes.Buffer
	sess, closeSess, err := openWikiSession(f.root, in, &stdout, &stderr)
	if err != nil {
		t.Fatalf("openWikiSession: %v\nstderr=%s", err, stderr.String())
	}
	// Tear the session worktree down. The session never produced a
	// commit, so closeSess will refuse via CanvasUnchangedError —
	// auto-push wouldn't have a turn to ride either way.
	t.Cleanup(func() {
		if err := closeSess(true); err != nil {
			t.Logf("closeSess: %v", err)
		}
		_ = sess
	})

	mainSHA, err := git.RevParse(f.root, "main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if mainSHA != remoteSHA {
		t.Fatalf("local main didn't pick up remote advance: want %s, got %s (auto-pull didn't fire?)", remoteSHA, mainSHA)
	}
}

// TestCloseSessSuppressesAutoPushWhenTurnFailed is the silent-failure-
// at-push regression: when the caller signals okToPush=false (agent run
// errored, or pre-finalize gate fired), closeSess must still tear the
// session worktree down and fast-forward local main, but the in-closure
// sync.AutoPush must be suppressed. Without this gate, a failed push
// synthesis turn auto-pushed the bureaucracy per-turn commit to origin
// while the moe-side branch never reached its remote — bureaucracy
// claimed the ship landed.
func TestCloseSessSuppressesAutoPushWhenTurnFailed(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()
	originBefore := f.originHead()

	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "no-autopush-on-fail",
		DocID:       "code",
		LockPurpose: "stage",
	}
	var stdout, stderr bytes.Buffer
	sess, closeSess, err := openWikiSession(f.root, in, &stdout, &stderr)
	if err != nil {
		t.Fatalf("openWikiSession: %v\nstderr=%s", err, stderr.String())
	}

	// Land a canvas commit on the session branch so session.Close has a
	// non-trivial fast-forward to perform — without it
	// CanvasUnchangedError fires and we'd never reach the sync.AutoPush
	// gate that's the actual subject under test.
	canvasRel := run.ContentPath("moe", "no-autopush-on-fail", "code")
	canvasAbs := filepath.Join(sess.WorktreePath, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasAbs, []byte("# canvas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, sess.WorktreePath, "add", canvasRel)
	gittest.Run(t, sess.WorktreePath, "commit", "-m", "work: turn")

	if err := closeSess(false); err != nil {
		t.Fatalf("closeSess(false): %v\nstderr=%s", err, stderr.String())
	}

	// Local main fast-forwarded to the session branch tip…
	localMain, err := git.RevParse(f.root, "main")
	if err != nil {
		t.Fatal(err)
	}
	// …but origin must not have followed. The push gate is the whole
	// fix; if the assertion below fails, the bug is back.
	if got := f.originHead(); got != originBefore {
		t.Errorf("origin advanced despite okToPush=false: want %s, got %s (local main = %s)",
			originBefore, got, localMain)
	}
}

// TestCloseSessRunsAutoPushWhenTurnSucceeded is the positive control:
// okToPush=true keeps today's behavior — sync.AutoPush fires inside
// closeSess and origin tracks local main. Without this counterpart,
// the failing-turn test could pass against a closeSess that never
// pushed under any condition.
func TestCloseSessRunsAutoPushWhenTurnSucceeded(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "autopush-on-success",
		DocID:       "code",
		LockPurpose: "stage",
	}
	var stdout, stderr bytes.Buffer
	sess, closeSess, err := openWikiSession(f.root, in, &stdout, &stderr)
	if err != nil {
		t.Fatalf("openWikiSession: %v", err)
	}

	canvasRel := run.ContentPath("moe", "autopush-on-success", "code")
	canvasAbs := filepath.Join(sess.WorktreePath, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvasAbs, []byte("# canvas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, sess.WorktreePath, "add", canvasRel)
	gittest.Run(t, sess.WorktreePath, "commit", "-m", "work: turn")

	if err := closeSess(true); err != nil {
		t.Fatalf("closeSess(true): %v\nstderr=%s", err, stderr.String())
	}

	localMain, err := git.RevParse(f.root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.originHead(); got != localMain {
		t.Errorf("origin didn't track local main on okToPush=true: want %s, got %s", localMain, got)
	}
}

// TestOpenWikiSessionRefusesOnRebaseConflict pins the halt-loud
// contract: if auto-pull hits a rebase conflict, the session never
// opens. No worktree, no branch — the operator resolves the conflict
// before any turn starts.
func TestOpenWikiSessionRefusesOnRebaseConflict(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()

	// Set up divergent commits on the same path so the pull rebase
	// conflicts.
	shared := filepath.Join(f.root, "shared.txt")
	writeFile(t, shared, "base\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "base: shared")
	gittest.Run(t, f.root, "push", "origin", "main")

	writeFile(t, shared, "local\n")
	gittest.Run(t, f.root, "add", "shared.txt")
	gittest.Run(t, f.root, "commit", "-m", "local: shared")
	f.advanceBureaucracyOrigin("shared.txt", "remote\n", "remote: shared")

	in := wikiSessionInputs{
		Project:     "moe",
		RunSlug:     "auto-pull-conflict",
		DocID:       "design",
		LockPurpose: "stage",
	}
	var stdout, stderr bytes.Buffer
	_, _, err := openWikiSession(f.root, in, &stdout, &stderr)
	if err == nil {
		t.Fatal("openWikiSession: expected refusal on rebase conflict")
	}
	if !strings.Contains(err.Error(), "rebase --continue") {
		t.Fatalf("error missing recovery prose: %v", err)
	}
	// No session branch should exist — Open never ran.
	branch := "session/moe/auto-pull-conflict/design"
	wt := gittest.Output(t, f.root, "worktree", "list")
	if strings.Contains(wt, branch) {
		t.Fatalf("session worktree was created despite auto-pull halt:\n%s", wt)
	}
}

// TestAutoPullWarnsAndContinuesOnNetworkFailure: origin unreachable
// (path doesn't exist). AutoPull must warn and return nil so the turn
// continues offline.
func TestAutoPullWarnsAndContinuesOnNetworkFailure(t *testing.T) {
	f := newSyncFixture(t)
	f.initBureaucracyOrigin()
	bogus := filepath.Join(t.TempDir(), "does-not-exist.git")
	gittest.Run(t, f.root, "remote", "set-url", "origin", bogus)

	headBefore := f.bureaucracyHead()
	var stdout, stderr bytes.Buffer
	if err := sync.AutoPull(f.root, &stdout, &stderr); err != nil {
		t.Fatalf("AutoPull returned %v; should warn and continue on network failure", err)
	}
	if f.bureaucracyHead() != headBefore {
		t.Fatal("AutoPull mutated HEAD on a failure path")
	}
	if !strings.Contains(stderr.String(), "[auto-sync skipped]") {
		t.Fatalf("expected warn line on stderr, got %q", stderr.String())
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
	return gittest.Output(t, root, "log", "-1", "--format=%B")
}
