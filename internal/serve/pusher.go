package serve

import (
	"context"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/sync"
)

// The resident pusher: the one place the bureaucracy's local main
// reaches origin.
//
// Every journal verb — a UI tag, `moe sdlc new`, a chain edit, a stage
// session close — used to run `git push` inside the repo lock before it
// returned. From the cloud-box that leg costs whole seconds, and it was
// costing them twice: once on the verb's own path, and again on every
// other verb queued behind the lock it was held across. Now verbs commit
// to local main and return; this goroutine drains what accumulated
// within a couple of seconds.
//
// Two properties are load-bearing:
//
//   - **Not gated on Dynamic.** The heartbeat's "armed or nothing" rule
//     is about *starting agent work* — an unarmed serve must decide
//     nothing on its own. Draining commits an operator already made to
//     the remote they already configured decides nothing, so the pusher
//     runs unarmed too. It is the first serve side-effect that does.
//   - **Outside the repo lock.** `git push` writes no worktree state and
//     no local branch ref — only refs/remotes/origin/main, which nothing
//     under the lock writes except a pull. Taking the lock would
//     reintroduce exactly the stall this removes. The one concurrent
//     case is an auto-pull's rebase: the pusher either pushes a tip
//     origin already has (no-op) or one origin has moved past
//     (non-fast-forward rejection, logged, retried after the rebase
//     lands). Neither corrupts anything, which is why the ahead-count is
//     re-read every tick rather than latched.

// pushInterval is the drain cadence. Baked, not configurable: a quiet
// tick is one `rev-list --count` against local refs, ~2 ms, so the
// choice between one second and five changes nothing an operator would
// notice. Variable rather than const so tests can shorten it.
var pushInterval = 2 * time.Second

// pushBackoffCap bounds the exponential cool-off after failures. Five
// minutes is short enough that a network that comes back is drained
// while the operator is still at the keyboard, long enough that an
// overnight outage mints a handful of log lines rather than a wall.
var pushBackoffCap = 5 * time.Minute

// runPusher is the drain loop. Started by ListenAndServe for the
// listener's lifetime; returns when ctx is cancelled. An in-flight push
// is not joined on the way out — the drain is stateless, so whatever it
// didn't finish is still ahead of origin at the next start.
//
// Failure cools itself off. The interval doubles per consecutive
// failure up to pushBackoffCap and resets on the first success, and only
// the transitions are logged: one line when a run of failures starts,
// one when it ends. A steady state of no commits is silent.
func (s *Server) runPusher(ctx context.Context) {
	interval := pushInterval
	fails := 0
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := s.drainToOrigin()
		switch {
		case err != nil:
			if fails == 0 {
				s.logf("pusher: %v — retrying, backing off", err)
			}
			fails++
			interval = min(interval*2, pushBackoffCap)
		case fails > 0:
			s.logf("pusher: origin reachable again, pushed %d commit(s)", n)
			fails, interval = 0, pushInterval
		case n > 0:
			s.logf("pusher: pushed %d commit(s)", n)
		}
		t.Reset(interval)
	}
}

// drainToOrigin pushes local main to its upstream when it is ahead,
// returning the number of commits that were ahead when the push ran (0
// when there was nothing to do, or when the tree is in a state where
// pushing would be wrong).
//
// The skips are all "ask again next tick", never errors: no upstream is
// the normal local-only setup, and a paused rebase means the worktree is
// mid-reconcile and HEAD is not a tip anyone should publish. An
// unreadable ahead-count is the one genuine failure — it means git could
// not answer about refs it should always be able to resolve.
func (s *Server) drainToOrigin() (int, error) {
	root := s.opts.Root
	if sync.RebaseInProgress(root) {
		return 0, nil
	}
	upstream, _ := git.Upstream(root)
	if upstream == "" {
		return 0, nil
	}
	n, err := git.AheadOf(root, upstream, "HEAD")
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := sync.PushMain(root); err != nil {
		return 0, err
	}
	return n, nil
}
