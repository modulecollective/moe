package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/sync"
)

// pusherFixture is a bureaucracy root whose main tracks a bare origin —
// the shape the drain reads: an upstream to be ahead of.
func pusherFixture(t *testing.T) (root, origin string) {
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

// shortenPushInterval speeds the drain loop up for a test and restores
// the baked values after.
func shortenPushInterval(t *testing.T, interval, backoffCap time.Duration) {
	t.Helper()
	prevInterval, prevCap := pushInterval, pushBackoffCap
	pushInterval, pushBackoffCap = interval, backoffCap
	t.Cleanup(func() { pushInterval, pushBackoffCap = prevInterval, prevCap })
}

// TestUnarmedServeStillPushes is the pusher's arming property, and the
// one place it deliberately parts company with the heartbeat: "armed or
// nothing" governs starting agent work, and draining commits the
// operator already made to the remote they already configured starts
// nothing. An unarmed serve is the common case on a box the operator
// isn't automating, and its journal still has to reach origin.
func TestUnarmedServeStillPushes(t *testing.T) {
	root, origin := pusherFixture(t)
	gittest.Commit(t, root, "journal: record something")
	local := gittest.HeadSHA(t, root)

	shortenPushInterval(t, 5*time.Millisecond, 50*time.Millisecond)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		settle(func() bool { return gittest.HeadSHA(t, origin) == local })
		cancel()
	}()
	if err := s.ListenAndServe(ctx); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if got := gittest.HeadSHA(t, origin); got != local {
		t.Fatalf("origin main = %s, want the local commit %s", got, local)
	}
}

// TestPusherLogsOnceThenRecovers: a dead origin costs one log line and
// an exponential back-off, not a line per tick, and the loop comes back
// on its own once origin does — with the drained count in the recovery
// line so the operator can see what was stranded.
func TestPusherLogsOnceThenRecovers(t *testing.T) {
	root, origin := pusherFixture(t)
	gittest.Commit(t, root, "journal: record something")
	local := gittest.HeadSHA(t, root)

	// Move the bare repo aside rather than repointing the remote: the
	// remote URL stays valid, so putting it back is the whole recovery.
	away := origin + ".away"
	if err := os.Rename(origin, away); err != nil {
		t.Fatal(err)
	}

	shortenPushInterval(t, 5*time.Millisecond, 40*time.Millisecond)
	log := &syncBuf{}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, Logger: log})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		settle(func() bool { return strings.Contains(log.String(), "pusher:") })
		if err := os.Rename(away, origin); err != nil {
			return
		}
		settle(func() bool { return strings.Contains(log.String(), "reachable again") })
		cancel()
	}()
	if err := s.ListenAndServe(ctx); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}

	out := log.String()
	if n := strings.Count(out, "retrying, backing off"); n != 1 {
		t.Errorf("failure logged %d times, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(out, "origin reachable again, pushed 1 commit(s)") {
		t.Errorf("missing recovery line with the drained count:\n%s", out)
	}
	if got := gittest.HeadSHA(t, origin); got != local {
		t.Fatalf("origin main = %s, want the stranded commit %s once recovered", got, local)
	}
}

// TestDrainToOriginReportsAheadCount pins the "count, not a flag"
// property the loop rests on: the drain re-reads how far ahead main is
// on every tick, so commits that land while a push is in flight are
// picked up by the next one rather than waiting for a fresh trigger.
func TestDrainToOriginReportsAheadCount(t *testing.T) {
	root, origin := pusherFixture(t)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	if n, err := s.drainToOrigin(); err != nil || n != 0 {
		t.Fatalf("drainToOrigin on a caught-up main = (%d, %v), want (0, nil)", n, err)
	}

	gittest.Commit(t, root, "journal: one")
	gittest.Commit(t, root, "journal: two")
	n, err := s.drainToOrigin()
	if err != nil {
		t.Fatalf("drainToOrigin: %v", err)
	}
	if n != 2 {
		t.Errorf("drained count = %d, want 2 (both commits, not one push event)", n)
	}
	if got, want := gittest.HeadSHA(t, origin), gittest.HeadSHA(t, root); got != want {
		t.Fatalf("origin main = %s, want %s", got, want)
	}

	// A commit landing after that push is the next tick's work, counted
	// on its own.
	gittest.Commit(t, root, "journal: three")
	if n, err := s.drainToOrigin(); err != nil || n != 1 {
		t.Fatalf("drainToOrigin after a later commit = (%d, %v), want (1, nil)", n, err)
	}
}

// TestDrainToOriginSkipsDuringRebase: nothing gets published while the
// worktree is mid-reconcile.
//
// Two phases, because a rebase has two shapes and only the second needs
// the guard. Phase one is the real thing — a `git pull --rebase` that
// conflicts, exactly what a session-open auto-pull does on divergent
// history. Git detaches HEAD to replay, so `@{u}` doesn't resolve and
// the drain would skip on the upstream check alone; the phase is here
// because it is the case that actually happens, not because it isolates
// the guard. Phase two is the window that does: the rebase state dir
// present with HEAD attached and main ahead of origin, where the
// upstream check passes and RebaseInProgress is the only thing standing
// between a half-finished rebase and origin.
func TestDrainToOriginSkipsDuringRebase(t *testing.T) {
	root, origin := pusherFixture(t)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	// Diverge: push a change to origin, rewind local, then commit a
	// conflicting one. The pull that follows cannot fast-forward.
	gittest.WriteAndCommit(t, root, "conflict.txt", "from origin\n", "remote: change")
	gittest.Run(t, root, "push")
	originHead := gittest.HeadSHA(t, origin)
	gittest.Run(t, root, "reset", "--hard", "HEAD~1")
	gittest.WriteAndCommit(t, root, "conflict.txt", "from local\n", "local: change")

	// Probe, not gittest.Run: this pull is expected to fail with
	// conflicts, and the paused rebase it leaves behind is the point.
	if git.Probe(root, "pull", "--rebase") {
		t.Fatal("pull --rebase succeeded; the fixture did not diverge")
	}
	if !sync.RebaseInProgress(root) {
		t.Fatal("no rebase paused after the conflicting pull")
	}
	if n, err := s.drainToOrigin(); err != nil || n != 0 {
		t.Fatalf("drainToOrigin mid-rebase = (%d, %v), want (0, nil)", n, err)
	}
	if got := gittest.HeadSHA(t, origin); got != originHead {
		t.Fatalf("origin main = %s, want %s untouched during a rebase", got, originHead)
	}

	// Phase two. Back on an attached HEAD, ahead of origin — the drain
	// would push — with the rebase state dir still on disk.
	gittest.Run(t, root, "rebase", "--abort")
	gittest.Run(t, root, "reset", "--hard", "origin/main")
	gittest.Commit(t, root, "journal: a commit the drain would otherwise take")
	if n, err := s.drainToOrigin(); err != nil || n != 1 {
		t.Fatalf("control: drainToOrigin = (%d, %v), want (1, nil) with no rebase in progress", n, err)
	}
	// Cleanly ahead by one, on top of what origin just took — so without
	// the guard this is a push that would land, not one origin refuses.
	gittest.Commit(t, root, "journal: the commit the guard must hold back")
	stateDir := filepath.Join(root, ".git", "rebase-merge")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	before := gittest.HeadSHA(t, origin)
	if n, err := s.drainToOrigin(); err != nil || n != 0 {
		t.Fatalf("drainToOrigin with a rebase state dir = (%d, %v), want (0, nil)", n, err)
	}
	if got := gittest.HeadSHA(t, origin); got != before {
		t.Fatalf("origin main advanced to %s (was %s) with a rebase in progress", got, before)
	}
}

// TestDrainToOriginSkipsWithoutUpstream: a branch with no @{u} is the
// normal local-only setup, not a failure — the drain asks again next
// tick and says nothing.
func TestDrainToOriginSkipsWithoutUpstream(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Run(t, root, "checkout", "-b", "main")
	gittest.Commit(t, root, "seed")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	if n, err := s.drainToOrigin(); err != nil || n != 0 {
		t.Fatalf("drainToOrigin with no upstream = (%d, %v), want (0, nil)", n, err)
	}
}
