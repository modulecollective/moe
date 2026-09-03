package sync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// pushMainFixture is a local repo with main tracking a bare origin —
// the smallest shape on which PushMain is observable.
func pushMainFixture(t *testing.T) (root, origin string) {
	t.Helper()
	root = t.TempDir()
	gittest.InitAt(t, root)
	gittest.Run(t, root, "checkout", "-b", "main")
	gittest.Commit(t, root, "seed")
	origin = gittest.InitBare(t)
	gittest.Run(t, root, "remote", "add", "origin", origin)
	gittest.Run(t, root, "push", "-u", "origin", "main")
	return root, origin
}

// TestPushMainLandsLocalCommitsOnOrigin: the drain's whole job.
func TestPushMainLandsLocalCommitsOnOrigin(t *testing.T) {
	root, origin := pushMainFixture(t)
	gittest.Commit(t, root, "journal: record something")

	if err := PushMain(root); err != nil {
		t.Fatalf("PushMain: %v", err)
	}
	if local, remote := gittest.HeadSHA(t, root), gittest.HeadSHA(t, origin); local != remote {
		t.Fatalf("origin main = %s, want local HEAD %s", remote, local)
	}
}

// TestPushMainReturnsErrorWithGitStderr: unlike the auto-push it
// replaced, PushMain returns the failure — the pusher needs it to back
// off — and folds git's own words in so the serve log says why.
func TestPushMainReturnsErrorWithGitStderr(t *testing.T) {
	root, _ := pushMainFixture(t)
	gittest.Run(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	gittest.Commit(t, root, "journal: record something")

	err := PushMain(root)
	if err == nil {
		t.Fatal("PushMain: want error for unreachable origin, got nil")
	}
	if !strings.Contains(err.Error(), "git push") {
		t.Fatalf("error should name the operation: %v", err)
	}
	if !strings.Contains(err.Error(), "gone.git") {
		t.Fatalf("error should carry git's stderr naming the bad remote: %v", err)
	}
}

func TestParseGitmodulesIncludesBranch(t *testing.T) {
	dir := t.TempDir()
	content := `[submodule "foo"]
	path = projects/foo/src
	url = https://example.com/foo.git
	branch = trunk
[submodule "bar"]
	path = projects/bar/src
	url = https://example.com/bar.git
`
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ParseGitmodules(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	if got[0].Branch != "trunk" {
		t.Fatalf("foo branch: want trunk, got %q", got[0].Branch)
	}
	if got[1].Branch != "" {
		t.Fatalf("bar branch: want empty (so resolver falls back to main), got %q", got[1].Branch)
	}
}

func TestParseGitmodulesMissingFileIsNil(t *testing.T) {
	got, err := ParseGitmodules(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing .gitmodules should return (nil, nil), got err=%v", err)
	}
	if got != nil {
		t.Fatalf("missing .gitmodules should return nil entries, got %v", got)
	}
}

func TestProjectIDForSubmodulePath(t *testing.T) {
	cases := map[string]string{
		"projects/moe/src":     "moe",
		"projects/foo-bar/src": "foo-bar",
		"projects/moe":         "", // not the canonical shape
		"vendor/thing":         "",
		"":                     "",
	}
	for in, want := range cases {
		if got := ProjectIDForSubmodulePath(in); got != want {
			t.Errorf("ProjectIDForSubmodulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// advanceFixture builds a bureaucracy root with a submodule worktree at
// a non-projects/<id>/src path (so AdvanceSubmodule skips
// EnsureMaterialized), seeds the submodule at c0 on main, pushes it to a
// bare origin, and records the gitlink in root at c0. Each caller then
// drives the submodule into the detached state under test.
func advanceFixture(t *testing.T) (root, subAbs, c0 string, e GitmoduleEntry) {
	t.Helper()
	root = t.TempDir()
	gittest.InitAt(t, root)
	gittest.Run(t, root, "checkout", "-b", "main")
	gittest.Commit(t, root, "bureaucracy seed")

	origin := gittest.InitBare(t)
	subAbs = filepath.Join(root, "sub")
	if err := os.Mkdir(subAbs, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	gittest.InitAt(t, subAbs)
	gittest.Run(t, subAbs, "checkout", "-b", "main")
	c0 = gittest.WriteAndCommit(t, subAbs, "a.txt", "0", "sub c0")
	gittest.Run(t, subAbs, "remote", "add", "origin", origin)
	gittest.Run(t, subAbs, "push", "-u", "origin", "main")

	// Record the gitlink at c0 so GitlinkSHA resolves and bump
	// detection has a baseline to compare the re-attached HEAD against.
	gittest.Run(t, root, "add", "sub")
	gittest.Run(t, root, "commit", "-m", "add sub gitlink")

	e = GitmoduleEntry{Name: "sub", Path: "sub", URL: origin, Branch: "main"}
	return root, subAbs, c0, e
}

// TestAdvanceSubmoduleRefusesDetachedOrphan: a hotfix committed on a
// detached HEAD is reachable from neither origin/main nor local main, so
// checking out main would strand it. AdvanceSubmodule must refuse before
// the checkout and leave HEAD exactly where the operator left it.
func TestAdvanceSubmoduleRefusesDetachedOrphan(t *testing.T) {
	root, subAbs, _, e := advanceFixture(t)

	gittest.Run(t, subAbs, "checkout", "--detach", "HEAD")
	hotfix := gittest.WriteAndCommit(t, subAbs, "hot.txt", "fix", "hotfix while detached")

	var stdout bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout)
	if err == nil {
		t.Fatalf("expected refusal, got bump=%+v", bump)
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Fatalf("error should name the detached state, got: %v", err)
	}
	if got := gittest.HeadSHA(t, subAbs); got != hotfix {
		t.Fatalf("guard must not move HEAD: want %s, got %s", hotfix, got)
	}
}

// TestAdvanceSubmoduleReattachesFromGitlink: the normal state after
// `git submodule update` is a detached HEAD at the recorded gitlink,
// which is an ancestor of origin/main. That re-attaches cleanly and
// bumps when origin has moved on.
func TestAdvanceSubmoduleReattachesFromGitlink(t *testing.T) {
	root, subAbs, c0, e := advanceFixture(t)

	c1 := gittest.WriteAndCommit(t, subAbs, "a.txt", "1", "sub c1")
	gittest.Run(t, subAbs, "push", "origin", "main")
	gittest.Run(t, subAbs, "checkout", "--detach", c0)

	var stdout bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout)
	if err != nil {
		t.Fatalf("re-attach from gitlink should succeed: %v\nstdout=%s", err, stdout.String())
	}
	if bump == nil || bump.ToSHA != c1 {
		t.Fatalf("want bump to %s, got %+v", c1, bump)
	}
	if ref := gittest.Output(t, subAbs, "symbolic-ref", "HEAD"); ref != "refs/heads/main" {
		t.Fatalf("HEAD should re-attach to main, got %q", ref)
	}
}

// TestAdvanceSubmoduleAllowsStaleLocalBranch: HEAD detached at
// origin/main's tip while the local branch lags. HEAD is on origin's
// history but not the local branch, so only the origin arm of the guard
// clears it — the checkout is safe and the ff advances local main.
func TestAdvanceSubmoduleAllowsStaleLocalBranch(t *testing.T) {
	root, subAbs, c0, e := advanceFixture(t)

	c1 := gittest.WriteAndCommit(t, subAbs, "a.txt", "1", "sub c1")
	gittest.Run(t, subAbs, "push", "origin", "main")
	gittest.Run(t, subAbs, "checkout", "--detach", c1)
	gittest.Run(t, subAbs, "branch", "-f", "main", c0) // local branch now stale

	var stdout bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout)
	if err != nil {
		t.Fatalf("stale local branch should not refuse: %v\nstdout=%s", err, stdout.String())
	}
	if bump == nil || bump.ToSHA != c1 {
		t.Fatalf("want bump to %s, got %+v", c1, bump)
	}
}
