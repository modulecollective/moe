package serve

import (
	"context"
	"time"

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

// pushTimeout bounds one push. Without it a black-holed transport hangs
// forever — git exposes no knob that covers connect, proxy CONNECT, DNS
// and the credential helper, and curl's own connect timeout is 300s — so
// the drain stalls with no error to log and no error to back off on.
//
// A minute is ~10x the slowest push observed from the cloud-box (1-5s
// end to end, worst tick-to-landed gap 7s) because the asymmetry runs
// one way: too short is a permanent wedge, where a slow-but-alive push
// is killed and retried and killed again forever, while too long costs
// only detection latency on a link that was already dead. Variable
// rather than const so tests can shorten it.
var pushTimeout = 60 * time.Second

// runPusher is the drain loop. Started by ListenAndServe for the
// listener's lifetime; returns when ctx is cancelled. An in-flight push
// is killed on the way out rather than joined: it runs under ctx, so a
// process-group child can't outlive serve as an orphan. Nothing is lost
// — the remote's ref update is atomic and the drain is stateless, so
// whatever didn't land is still ahead of origin at the next start.
//
// Failure cools itself off. The interval doubles per consecutive
// failure up to pushBackoffCap and resets on the first push that lands,
// and only the transitions are logged: one line when a run of failures
// starts, one when it ends. A steady state of no commits is silent.
//
// Recovery is a push that landed, not merely a tick that didn't error.
// A drain that skipped returns (0, nil) too, so treating any quiet tick
// as recovery lets a rebase paused during an outage — the pull fails
// for the same reason the push does — mint a "reachable again" line for
// a remote nobody reached, reset the cool-off, and let the next tick
// re-log the failure. While failing, a skip stays silent and holds the
// cool-off where it is.
func (s *Server) runPusher(ctx context.Context) {
	// Read the tunables once. The loop is not joined at shutdown, so it
	// can outlive the ListenAndServe that started it by a tick — and a
	// test that shortens these restores them in a cleanup that would
	// otherwise race a live loop's re-read.
	base, backoffCap, timeout := pushInterval, pushBackoffCap, pushTimeout
	interval := base
	fails := 0
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := s.drainToOrigin(ctx, timeout)
		// Shutdown cancels the push mid-flight, so the error it returns
		// is our own doing. Logging it would blame the remote for a
		// stop the operator asked for.
		if ctx.Err() != nil {
			return
		}
		// One clock read for the whole arm: the log line and the recorded
		// instant describe the same event and shouldn't drift.
		now := time.Now()
		switch {
		case err != nil:
			if fails == 0 {
				s.logf("pusher: %v — retrying, backing off", err)
			}
			fails++
			interval = min(interval*2, backoffCap)
			s.activity.recordPushFail(now, err, now.Add(interval))
			s.saveActivity()
		case fails > 0 && n > 0:
			s.logf("pusher: origin reachable again, pushed %d commit(s)", n)
			fails, interval = 0, base
			s.activity.recordPush(now, n)
			s.saveActivity()
		case n > 0:
			s.logf("pusher: pushed %d commit(s)", n)
			s.activity.recordPush(now, n)
			s.saveActivity()
		}
		// A skip records nothing, exactly as it logs nothing: it is not
		// news that there was nothing to push.
		t.Reset(interval)
	}
}

// drainToOrigin pushes local main to its upstream when it is ahead,
// returning the number of commits that were ahead when the push ran (0
// when there was nothing to do, or when the tree is in a state where
// pushing would be wrong).
//
// What counts as ahead — and which states are a silent skip rather than
// a failure — is sync.Unpushed's, not this loop's: the dashes report the
// same number from the same predicate, so the drain and the surfaces
// that describe it can't disagree. Its skips are all "ask again next
// tick", never errors.
func (s *Server) drainToOrigin(ctx context.Context, timeout time.Duration) (int, error) {
	root := s.opts.Root
	n, err := sync.Unpushed(root)
	if err != nil || n == 0 {
		return 0, err
	}
	pushCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := sync.PushMain(pushCtx, root); err != nil {
		return 0, err
	}
	return n, nil
}
