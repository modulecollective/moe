package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/run"
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

// HeartbeatDecision is one project's verdict for one tick: whether to
// sweep it, whether a stand-down held a sweep that wanted to run, and
// the gate's own words for why.
//
// The reason is the display string, not a code. Every one of them is a
// sentence the gate already had to compose to make the call, so
// returning them costs the gate nothing and buys the operator the whole
// trace — "the journal moved", "a sweep already surveyed the current
// tip", "somebody is already inside the project". Nothing parses them
// back, and nothing branches on them.
//
// Held is what keeps that true now that /serve renders only the
// non-trivial. The gate produces two kinds of quiet: nothing to do (the
// background hum) and held — a sweep blocked by something the operator
// can act on. Only the second earns a row and a line in the ring, and
// the split rides on this bit rather than on matching the reason text.
type HeartbeatDecision struct {
	Project string
	Sweep   bool
	Held    bool
	Reason  string
}

// Heartbeat is the cli-side gate the ticker consults. Implemented by
// cli's heartbeatGate; nil on Options disables the heartbeat entirely,
// which is how every test that doesn't care about it opts out.
type Heartbeat interface {
	// Due returns one decision per project the gate looked at, given the
	// tick length (the one-quiet-tick rule needs it). It also runs the
	// reap of dead machine sessions. Read-only otherwise, and warn-only:
	// a failed read drops a project rather than the tick.
	//
	// Every project's verdict crosses the seam, not just the ones to
	// sweep. "Serve owns the clock, cli owns the judgement" is unchanged
	// — the verdicts already crossed here; now they cross with their
	// reasons attached instead of losing them to stderr.
	Due(tick time.Duration, log io.Writer) []HeartbeatDecision
	// Swept records that a project's heartbeat sweep has finished, so
	// the sweep's own journal commits don't read as a delta worth
	// sweeping again. clean reports whether the child exited zero, which
	// is the difference between "a survey looked at this board and made
	// its calls" and "a survey died partway" — the gate needs both, and
	// only the first is a reason to stop offering the same parked work.
	Swept(projectID string, clean bool)
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

// record folds one finished sweep into the ledger and returns the run of
// failures it now stands at and the cool-off it earned (0, 0 for a clean
// sweep, which resets both). The activity record keeps a copy for the
// dash, which is why both come back rather than just the skip count.
func (h *heartbeat) record(projectID string, failed bool) (fails, skip int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !failed {
		delete(h.fails, projectID)
		delete(h.skip, projectID)
		return 0, 0
	}
	h.fails[projectID]++
	skip = min(1<<(h.fails[projectID]-1), heartbeatBackoffCap)
	h.skip[projectID] = skip
	return h.fails[projectID], skip
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
// state, spawn what's left, and record the whole verdict set — swept and
// skipped alike — into the activity record both dashes read.
func (s *Server) heartbeatTick() {
	now := time.Now()
	decisions := s.opts.Heartbeat.Due(heartbeatInterval, s.syncWriter())
	s.activity.recordTick(now, decisions)
	s.saveActivity()

	for _, d := range decisions {
		if !d.Sweep {
			continue
		}
		projectID := d.Project
		if cooling, left, fails := s.heartbeat.cooling(projectID); cooling {
			s.logf("heartbeat: %s cooling off after %d failure(s) — %d tick(s) left", projectID, fails, left)
			// The gate wanted this project swept and the backoff held it,
			// so the gate's reason is not what happened. Overwrite it or
			// the panel would read "sweeping — the journal moved" for a
			// project that is doing nothing of the kind.
			s.activity.recordSkip(projectID,
				fmt.Sprintf("cooling off after %d failure(s)", fails), fails, left)
			s.saveActivity()
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
		// The emit file and the row's run id both still name the last
		// sweep's run, and neither is this one's. Both clears belong here,
		// before the spawn: a child can write its emit file, exit, and have
		// its reader goroutine land recordSweepRun before this goroutine
		// reaches the next statement. takeSweepRun consumed the file on the
		// way through, so a clear after the spawn erases the new sweep's
		// link with nothing left to read it back — the row stays Failed
		// with no run for good.
		if err := clearSweepRun(s.opts.Root, projectID); err != nil {
			s.logf("heartbeat: clear sweep run file for %s: %v", projectID, err)
		}
		s.activity.recordSweepRun(projectID, "")
		child, err := s.children.spawn(id, s.opts.MoeBin,
			[]string{"pulse", "new", "--dynamic",
				"--emit-run", sweepRunPath(s.opts.Root, projectID), projectID},
			s.opts.Root, s.opts.Logger)
		if err != nil {
			s.logf("heartbeat: spawn %s: %v", projectID, err)
			continue
		}
		s.logf("heartbeat: sweeping %s", projectID)
		s.activity.recordSweepStart(projectID, child.started)
		s.saveActivity()
		go s.awaitHeartbeat(projectID, child)
	}
}

// awaitHeartbeat records a sweep's outcome once its child exits: the
// gate's cursors, and the backoff ledger.
//
// The tip cursor moves even on failure. A sweep that died still wrote
// its run-open commit, and leaving the cursor behind it would make the
// next tick read that commit as fresh delta and sweep straight into the
// same wall — the backoff would be pacing a loop it never gets to slow.
// The exit code is what separates that case from a sweep that actually
// surveyed the board; the gate keeps them apart, so the failure path
// still gets retried on the parked-work leg.
func (s *Server) awaitHeartbeat(projectID string, c *child) {
	<-c.done
	failed := c.exitErr != nil
	s.opts.Heartbeat.Swept(projectID, !failed)
	fails, skip := s.heartbeat.record(projectID, failed)
	if skip > 0 {
		s.logf("heartbeat: %s failed (%v) — skipping %d tick(s)", projectID, c.exitErr, skip)
	}
	s.activity.recordSweepEnd(projectID, time.Now(), !failed, fails, skip)
	s.saveActivity()
}

// sweepRunPath is where the sweep child for projectID writes its run —
// the slug's way back across the process boundary.
//
// A sweep mints its pulse run inside the child, and the only thing that
// crossed back before this was the exit code, which is why /serve could
// say "moe sweeping (3m)" and never name what it was sweeping into. The
// child writes the slug to this file (`moe pulse new --emit-run`) and
// serve reads it. One file per project, owned by serve: cleared before
// every spawn, so an absent file always means "no run minted", never "a
// run from some earlier sweep".
//
// Under `.moe/`, which carries a `*` gitignore — sweeps never dirty the
// tree.
func sweepRunPath(root, projectID string) string {
	return filepath.Join(root, ".moe", "sweep-"+projectID)
}

// clearSweepRun drops a project's emit file. Called before each spawn:
// a file left by a sweep whose exit serve never saw (a restart mid-sweep)
// would otherwise be read as the new sweep's run.
func clearSweepRun(root, projectID string) error {
	err := os.Remove(sweepRunPath(root, projectID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// readSweepRun returns the run slug a live sweep has already written, or
// "" when there is nothing there yet. Shape-validated because this is a
// file boundary; whether the run exists is not checked here, so a page
// load in the seconds after the slug lands but before the run's opening
// commit can show a link that 404s. takeSweepRun is where that gets
// checked.
func readSweepRun(root, projectID string) string {
	body, err := os.ReadFile(sweepRunPath(root, projectID))
	if err != nil {
		return ""
	}
	slug := strings.TrimSpace(string(body))
	if !slugPattern.MatchString(slug) {
		return ""
	}
	return slug
}

// takeSweepRun is the exit-time read: the slug, then the file goes. The
// run has to still exist, because a link to a run that isn't there is
// worse than the plain text /serve had before.
//
// Note what this does *not* catch: a Ctrl-C'd sweep. disposePulseRun
// closes the run it minted — stamping the skip note over the canvas —
// rather than deleting it, so run.json survives and the link stands.
// That is the better answer anyway: the row points at a real page that
// says the sweep was skipped. The stat covers the states where the run
// genuinely isn't on disk — a mint that failed after the emit write, a
// stale file whose run was since removed by hand.
func takeSweepRun(root, projectID string) string {
	slug := readSweepRun(root, projectID)
	_ = clearSweepRun(root, projectID)
	if slug == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, run.Dir(projectID, slug), "run.json")); err != nil {
		return ""
	}
	return slug
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
