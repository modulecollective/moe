package cli

import (
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
)

// pulseInterrupt is the scoped Ctrl-C latch for a pulse tail. Between
// minting the survey run and the agent executor's own watcher
// (agent.StartCommand), SIGINT has Go's default disposition — Ctrl-C
// kills moe outright, orphaning the just-minted pulse run and any lock
// held in that window. The advertised "Ctrl-C to skip" only actually
// works once the agent child is running.
//
// This latch closes that gap: the first Ctrl-C sets the latch (checked
// at the pulse's step boundaries) and the process survives, so the
// normal unwind paths run — locks release via their defers, the
// bootstrap-failure path tears the worktree down — and the pulse can
// dispose of its own run. Its scope is exactly a pulse tail
// (runPulse): firePulse and `moe pulse new`, never the interactive
// `moe pulse pulse`, which stays the operator's own session.
//
// A second Ctrl-C, after the first is latched, gets Go's default
// disposition again — signal.Stop steps the watcher out of the way so
// an impatient operator keeps the escape hatch, mirroring serve's "two
// Ctrl-Cs" instinct and the documented "SIGINT collapses to fast
// process exit" invariant for everything outside the scope.
type pulseInterrupt struct {
	latched atomic.Bool
	stop    func()
	done    chan struct{}
}

// installPulseInterrupt registers a scoped os.Interrupt watcher and
// returns the live latch. Pair with a deferred Close so the watcher is
// torn down (and the process signal disposition restored) when the
// pulse finishes.
func installPulseInterrupt() *pulseInterrupt {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(sigCh)
			close(sigCh)
		})
	}
	return startPulseInterrupt(sigCh, stop)
}

// startPulseInterrupt is the inner form used by installPulseInterrupt
// and by tests: the signal source and its stop function are injected so
// tests can drive the latch without sending a real SIGINT to the test
// binary. stop must be idempotent (installPulseInterrupt guards it with
// a sync.Once) — the watch goroutine calls it on the first signal and
// Close calls it again on a clean finish.
func startPulseInterrupt(sigCh <-chan os.Signal, stop func()) *pulseInterrupt {
	pi := &pulseInterrupt{stop: stop, done: make(chan struct{})}
	go pi.watch(sigCh)
	return pi
}

// watch latches on the first os.Interrupt and then steps out of the way:
// stop() runs signal.Stop so a subsequent Ctrl-C gets the default
// process teardown. It exits when sigCh is closed (which stop does, or
// Close does on a clean finish).
func (pi *pulseInterrupt) watch(sigCh <-chan os.Signal) {
	defer close(pi.done)
	for range sigCh {
		if pi.latched.CompareAndSwap(false, true) {
			pi.stop()
		}
	}
}

// interrupted reports whether a Ctrl-C landed inside the pulse's skip
// window. Nil-safe so the survey and openPulse's prompt builder can
// call it unconditionally — a nil latch (the interactive `moe pulse
// pulse` path, or a test that passes none) reads as "never
// interrupted".
func (pi *pulseInterrupt) interrupted() bool {
	return pi != nil && pi.latched.Load()
}

// mark force-latches the interrupt. The survey uses it on the mid-agent
// path (openPulse == 130): the operator's Ctrl-C reached the agent
// child, so the skip must still propagate out even though the run is
// left open for review. Nil-safe, like interrupted.
func (pi *pulseInterrupt) mark() {
	if pi != nil {
		pi.latched.Store(true)
	}
}

// Close tears the watcher down on a clean finish and waits for the
// watch goroutine to drain. Idempotent via the injected stop's guard,
// and nil-safe like interrupted and mark — the survey closes the window
// early before rooting a ride, and it may hold no latch at all.
func (pi *pulseInterrupt) Close() {
	if pi == nil {
		return
	}
	pi.stop()
	<-pi.done
}
