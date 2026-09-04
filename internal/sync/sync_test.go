package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	if err := PushMain(t.Context(), root); err != nil {
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

	err := PushMain(t.Context(), root)
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

// TestPushMainReportsTheTimeoutItBlew: the pusher's whole recovery path
// hangs off this error. It has to say the push timed out (the operator
// reads it in the serve log), and it has to unwrap to
// context.DeadlineExceeded so a caller can tell a dead transport from a
// rejected ref.
//
// An already-expired context is the deterministic way in: exec.Cmd.Start
// returns ctx.Err() before forking, so there's no live push to race.
func TestPushMainReportsTheTimeoutItBlew(t *testing.T) {
	root, _ := pushMainFixture(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := PushMain(ctx, root)
	if err == nil {
		t.Fatal("PushMain: want an error for an expired context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to unwrap to context.DeadlineExceeded", err)
	}
	if got := err.Error(); !strings.HasPrefix(got, "git push: timed out after ") {
		t.Errorf("error = %q, want the \"git push: timed out after ...\" shape", got)
	}
	// A killed push prints nothing; the empty "()" suffix the success
	// path would append reads like git spoke and we dropped it.
	if strings.Contains(err.Error(), "()") {
		t.Errorf("error = %q, want no empty output suffix when git printed nothing", err)
	}
}

// TestUnpushedCountsAndSkips is the one predicate three callers share —
// serve's pusher acts on it, both dashes report it — so its four
// answers are pinned here rather than inferred from any one of them.
//
// The two zeros are the point: "no upstream" and "mid-rebase" are
// states where nothing is owed and publishing would be wrong, and both
// must read as a quiet nothing rather than a failure, or the dashes
// would cry "unpushed" at a local-only box and the drain would back off
// against a remote that never refused anything.
func TestUnpushedCountsAndSkips(t *testing.T) {
	t.Run("no upstream is a quiet zero", func(t *testing.T) {
		root := t.TempDir()
		gittest.InitAt(t, root)
		gittest.Run(t, root, "checkout", "-b", "main")
		gittest.Commit(t, root, "seed")
		if n, err := Unpushed(root); n != 0 || err != nil {
			t.Fatalf("Unpushed with no upstream = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("caught up is zero", func(t *testing.T) {
		root, _ := pushMainFixture(t)
		if n, err := Unpushed(root); n != 0 || err != nil {
			t.Fatalf("Unpushed on a caught-up main = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("counts the commits origin hasn't got", func(t *testing.T) {
		root, _ := pushMainFixture(t)
		gittest.Commit(t, root, "journal: one")
		gittest.Commit(t, root, "journal: two")
		n, err := Unpushed(root)
		if err != nil {
			t.Fatalf("Unpushed: %v", err)
		}
		if n != 2 {
			t.Errorf("Unpushed = %d, want 2", n)
		}
	})

	t.Run("a paused rebase is a quiet zero", func(t *testing.T) {
		root, _ := pushMainFixture(t)
		// Cleanly ahead, so without the rebase guard this would count 1 —
		// the row above proves it does.
		gittest.Commit(t, root, "journal: the commit the guard holds back")
		if err := os.MkdirAll(filepath.Join(root, ".git", "rebase-merge"), 0o755); err != nil {
			t.Fatal(err)
		}
		if n, err := Unpushed(root); n != 0 || err != nil {
			t.Fatalf("Unpushed with a rebase in progress = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("an unreadable count is the one real error", func(t *testing.T) {
		root, _ := pushMainFixture(t)
		// A remote-tracking ref that names an object the repo doesn't
		// have. @{u} still resolves — the name is intact — so the
		// no-upstream skip doesn't fire, and rev-list is left unable to
		// answer a question it should always be able to answer. Deleting
		// the ref outright wouldn't do: that reads as no upstream at all,
		// which is a legitimate zero.
		ref := filepath.Join(root, ".git", "refs", "remotes", "origin", "main")
		if err := os.WriteFile(ref, []byte(strings.Repeat("dead0000", 5)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Unpushed(root); err == nil {
			t.Fatal("Unpushed with an unresolvable upstream: got nil error, want one")
		}
	})
}
