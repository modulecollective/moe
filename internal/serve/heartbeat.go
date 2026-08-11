package serve

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// The resident heartbeat: the clock that makes MoE's find → order →
// start loop survive the operator walking away.
//
// Everything here is a ticker, a gate check and a child spawn. It
// decides nothing — on each tick it asks the cli-side gate which
// projects warrant a look, and for each one runs `moe pulse new
// --dynamic <project>` as an ordinary PTY child. The sweep that child
// runs is the same sweep a `!!!!` tail fires, held to the same floor:
// only settled designs with nobody inside get started, every ride walks
// review and test, and every act it takes is journal-marked.
//
// Three properties are load-bearing:
//
//   - **Armed or nothing.** No Options.Dynamic, no ticker. An unarmed
//     serve is today's MoE byte for byte, which makes the fallback for
//     every failure here "the status quo".
//   - **A quiet board is free.** The gate is read-only and cheap, so a
//     project with no delta and no parked settled work costs no agent
//     turn, no run and no journal noise. That is what lets the cadence
//     be baked rather than tuned.
//   - **The binary on disk is what runs.** Spawning the CLI as a child
//     rather than calling in-process means a rebuilt `moe` takes effect
//     at the next tick — the stale-binary-tail problem doesn't extend to
//     the heartbeat.

// heartbeatInterval is the tick. Baked, not configurable: the gate makes
// quiet ticks free, so anything in the ten-to-sixty-minute range changes
// little, and a knob here would be one more thing to get wrong. Variable
// rather than const so tests can shorten it.
var heartbeatInterval = 20 * time.Minute

// heartbeatBackoffCap bounds the exponential cool-off. Six skipped ticks
// is two hours of quiet at the default cadence — long enough that a
// night of exhausted plan limits or a dead network mints a handful of
// failed sweeps rather than a pile, short enough that the loop comes
// back on its own once the vendor does.
const heartbeatBackoffCap = 6

// heartbeatChildPrefix namespaces heartbeat children in the PTY child
// registry. The colon is what keeps them from colliding with a run id:
// ids there are "<project>/<slug>" and both halves are slugs, so no run
// can ever be spelled this way. Riding the same registry is what gets
// the heartbeat serve's shutdown wind-down (two Ctrl-Cs, then hangup)
// and its notify hook for free.
const heartbeatChildPrefix = "heartbeat:"

// Heartbeat is the cli-side gate the ticker consults. Implemented by
// cli's heartbeatGate; nil on Options disables the heartbeat entirely,
// which is how every test that doesn't care about it opts out.
type Heartbeat interface {
	// Due returns the project ids whose board warrants a sweep now,
	// given the tick length (the one-quiet-tick rule needs it). It also
	// runs the reap of dead machine sessions. Read-only otherwise, and
	// warn-only: a failed read drops a project rather than the tick.
	Due(tick time.Duration, log io.Writer) []string
	// Swept records that a project's heartbeat sweep has finished, so
	// the sweep's own journal commits don't read as a delta worth
	// sweeping again.
	Swept(projectID string)
}

// heartbeat is the ticker's own state: per project, the cool-off a run
// of failures earned and how much of it is left. Guarded because the
// tick reads it on the ticker goroutine while each sweep's watcher
// writes it on its own.
type heartbeat struct {
	mu    sync.Mutex
	fails map[string]int
	skip  map[string]int
}

func newHeartbeat() *heartbeat {
	return &heartbeat{fails: map[string]int{}, skip: map[string]int{}}
}

// cooling reports whether a project is still serving out a cool-off,
// consuming one tick of it when it is.
func (h *heartbeat) cooling(projectID string) (bool, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	left := h.skip[projectID]
	if left <= 0 {
		return false, 0, h.fails[projectID]
	}
	h.skip[projectID] = left - 1
	return true, left - 1, h.fails[projectID]
}

// record folds one finished sweep into the ledger and returns the
// cool-off it earned (0 for a clean sweep, which resets).
func (h *heartbeat) record(projectID string, failed bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !failed {
		delete(h.fails, projectID)
		delete(h.skip, projectID)
		return 0
	}
	h.fails[projectID]++
	skip := min(1<<(h.fails[projectID]-1), heartbeatBackoffCap)
	h.skip[projectID] = skip
	return skip
}

// runHeartbeat is the ticker loop. Started by ListenAndServe when the
// process is armed and a gate is wired; returns when ctx is cancelled.
//
// Failure cools itself off. Consecutive failed children back a project's
// tick off exponentially and a clean sweep resets it — deliberately
// generic, so a night of exhausted plan limits, a dead network and a
// broken `gh` all read the same. There is no vendor-error parsing and no
// budget accounting here on purpose: the first failure's run, left open
// on the dash's ACTIVE list, is the operator's tell, and the backoff is
// only what keeps a dead vendor from minting a pile of them by morning.
func (s *Server) runHeartbeat(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	s.logf("heartbeat: armed, every %s", heartbeatInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.heartbeatTick()
		}
	}
}

// heartbeatTick is one pass: ask the gate, filter by the ticker's own
// state, spawn what's left.
func (s *Server) heartbeatTick() {
	for _, projectID := range s.opts.Heartbeat.Due(heartbeatInterval, s.syncWriter()) {
		if cooling, left, fails := s.heartbeat.cooling(projectID); cooling {
			s.logf("heartbeat: %s cooling off after %d failure(s) — %d tick(s) left", projectID, fails, left)
			continue
		}
		// A heartbeat child of our own still running is the ticker's
		// single-flight: a sweep that kicked a ride can outlive many
		// ticks, and the ride's own tail pulses own growth while it does.
		id := heartbeatChildPrefix + projectID
		if c, ok := s.children.get(id); ok {
			if exited, _ := c.snapshot(); !exited {
				continue
			}
		}
		child, err := s.children.spawn(id, s.opts.MoeBin,
			[]string{"pulse", "new", "--dynamic", projectID}, s.opts.Root, s.opts.Logger)
		if err != nil {
			s.logf("heartbeat: spawn %s: %v", projectID, err)
			continue
		}
		s.logf("heartbeat: sweeping %s", projectID)
		go s.awaitHeartbeat(projectID, child)
	}
}

// awaitHeartbeat records a sweep's outcome once its child exits: the
// gate's tip cursor either way, and the backoff ledger.
//
// The cursor moves even on failure. A sweep that died still wrote its
// run-open commit, and leaving the cursor behind it would make the next
// tick read that commit as fresh delta and sweep straight into the same
// wall — the backoff would be pacing a loop it never gets to slow.
func (s *Server) awaitHeartbeat(projectID string, c *child) {
	<-c.done
	s.opts.Heartbeat.Swept(projectID)
	if skip := s.heartbeat.record(projectID, c.exitErr != nil); skip > 0 {
		s.logf("heartbeat: %s failed (%v) — skipping %d tick(s)", projectID, c.exitErr, skip)
	}
}

// heartbeatProject returns the project a child id names when the child
// is a heartbeat sweep, and "" for an ordinary run child. The notify
// payload reads it so a phone glance can tell a sweep that died from a
// run that did.
func heartbeatProject(childID string) string {
	p, ok := strings.CutPrefix(childID, heartbeatChildPrefix)
	if !ok {
		return ""
	}
	return p
}
