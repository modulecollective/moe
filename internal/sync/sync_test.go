package sync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
)

// journalPushFixture is a local repo with main tracking a bare origin —
// the smallest shape on which WithJournalPush's push leg is observable.
func journalPushFixture(t *testing.T) (root, origin string) {
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

// TestWithJournalPushRacesCommitToOrigin: fn's commit reaches origin
// before the verb returns — the whole point of the shared write-edge.
func TestWithJournalPushRacesCommitToOrigin(t *testing.T) {
	root, origin := journalPushFixture(t)

	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: record something")
		return nil
	})
	if err != nil {
		t.Fatalf("WithJournalPush: %v\nstderr=%s", err, stderr.String())
	}
	if local, remote := gittest.HeadSHA(t, root), gittest.HeadSHA(t, origin); local != remote {
		t.Fatalf("origin main = %s, want local HEAD %s", remote, local)
	}
}

// TestWithJournalPushSkipsPushOnFnError: a failing fn surfaces its
// error unchanged and origin never sees a push.
func TestWithJournalPushSkipsPushOnFnError(t *testing.T) {
	root, origin := journalPushFixture(t)
	originBefore := gittest.HeadSHA(t, origin)

	sentinel := errors.New("verb failed")
	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: half-done")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if got := gittest.HeadSHA(t, origin); got != originBefore {
		t.Fatalf("origin advanced on fn failure: %s -> %s", originBefore, got)
	}
}

// TestWithJournalPushUnreachableOriginWarnsNotFails: the push leg is
// best-effort — a dead origin costs one stderr line, never the verb.
func TestWithJournalPushUnreachableOriginWarnsNotFails(t *testing.T) {
	root, _ := journalPushFixture(t)
	gittest.Run(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: record something")
		return nil
	})
	if err != nil {
		t.Fatalf("WithJournalPush should warn, not fail: %v", err)
	}
	if !strings.Contains(stderr.String(), "[auto-sync skipped]") {
		t.Fatalf("missing warn line, stderr=%q", stderr.String())
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

	var stdout, stderr bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout, &stderr)
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

	var stdout, stderr bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout, &stderr)
	if err != nil {
		t.Fatalf("re-attach from gitlink should succeed: %v\nstderr=%s", err, stderr.String())
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

	var stdout, stderr bytes.Buffer
	bump, err := AdvanceSubmodule(root, e, &stdout, &stderr)
	if err != nil {
		t.Fatalf("stale local branch should not refuse: %v\nstderr=%s", err, stderr.String())
	}
	if bump == nil || bump.ToSHA != c1 {
		t.Fatalf("want bump to %s, got %+v", c1, bump)
	}
}
