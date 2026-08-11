package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/session"
)

// The cli half of serve's resident heartbeat: the read-only gate that
// decides whether a tick is worth an agent turn, and the reap step that
// runs ahead of it.
//
// The split is the same one every other serve seam takes. Serve owns the
// clock, the backoff and the child; every question that needs the
// journal, the chain graph or the workflow registry is answered here and
// crosses as one callback. That is also what makes the gate testable
// without a listener.
//
// Nothing here decides anything the operator hasn't already licensed.
// The tick only asks the question a bang answer used to ask by being
// typed; what a sweep may *do* once looking is unchanged — settled
// designs, nobody inside, review and test on every ride.

// heartbeatGate implements serve.Heartbeat against a bureaucracy root.
//
// Its only state is the journal tip each project stood at when its last
// heartbeat sweep finished. In-memory on purpose: the question it
// answers is "has anything happened since I last looked", and a serve
// that just started has, correctly, never looked. Seeding lazily (the
// first observation records the tip and reads as *not* moved) is what
// keeps a restart from sweeping every project on a quiet board — an
// armed serve picks up existing work through the parked-thread leg
// instead, which is the leg that means something.
type heartbeatGate struct {
	root string

	mu   sync.Mutex
	tips map[string]string
}

func newHeartbeatGate(root string) *heartbeatGate {
	return &heartbeatGate{root: root, tips: map[string]string{}}
}

// Due answers one tick: which projects warrant a sweep right now.
//
// Read-only and cheap by construction — one run scan, one journal index
// (memoized on HEAD), one ref listing, and one `git log -1` per project.
// A quiet board therefore costs no agent turn, no run, and no journal
// noise, which is the whole reason a fixed cadence can be baked rather
// than tuned.
//
// Warn-only throughout, mirroring the pulse itself: a read that fails
// drops the project from this tick rather than failing the tick. The
// next one is twenty minutes away and re-derives everything.
func (g *heartbeatGate) Due(tick time.Duration, log io.Writer) []string {
	g.reap(log)

	projects, _, err := project.List(g.root)
	if err != nil {
		fmt.Fprintf(log, "heartbeat: list projects: %v\n", err)
		return nil
	}
	sc, ok := newPulseScan(g.root)
	if !ok {
		fmt.Fprintf(log, "heartbeat: could not read the journal — standing down this tick\n")
		return nil
	}
	occupied := openSessionProjects(g.root)

	now := time.Now()
	var due []string
	for _, p := range projects {
		if reason := g.projectDue(sc, p.ID, occupied, tick, now, log); reason != "" {
			fmt.Fprintf(log, "heartbeat: sweeping %s — %s\n", p.ID, reason)
			due = append(due, p.ID)
		}
	}
	return due
}

// Swept records where a project's journal stood once its heartbeat
// sweep finished. Called by the ticker on child exit, and it is what
// makes "the journal moved" mean *something other than my own sweep*:
// a survey writes its own run open and close commits, so recording the
// tip at dispatch time would leave every quiet board reading as moved
// forever.
func (g *heartbeatGate) Swept(projectID string) {
	tip, _, _, ok := projectJournalTip(g.root, projectID)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tips[projectID] = tip
}

// projectDue is the per-project gate. Returns the reason to sweep, or
// "" to stand down. Order is cheapest-first and stand-downs come last,
// so a project that is going to be skipped costs the least work.
func (g *heartbeatGate) projectDue(sc *pulseScan, projectID string, occupied map[string]bool, tick time.Duration, now time.Time, log io.Writer) string {
	tip, tipAt, operatorTip, ok := projectJournalTip(g.root, projectID)
	if !ok {
		// A project with no journal history at all — freshly registered,
		// nothing to survey.
		return ""
	}

	// One quiet tick. The operator gets a full tick with their hands on
	// the board before the machine moves, which is what closes the
	// staging race on loose runs: work minted, promoted or edited by
	// hand is never picked up while it is still being arranged. Staging
	// can skip the window entirely by minting straight into a chain head
	// — the fence the groom step honours.
	if operatorTip && now.Sub(tipAt) < tick {
		return ""
	}

	g.mu.Lock()
	seen, known := g.tips[projectID]
	if !known {
		g.tips[projectID] = tip
	}
	g.mu.Unlock()

	moved := known && seen != tip
	parked := ""
	if !moved {
		parked = parkedKickableThread(g.root, sc, projectID)
		if parked == "" {
			return ""
		}
	}

	// Stand-downs. A ride or a sitting in flight means the tail-pulse
	// path already owns the next sweep, and a survey already open means
	// one is in flight — the pulse's own single-flight, read from here so
	// the tick doesn't pile a second one on.
	if occupied[projectID] {
		return ""
	}
	if slug := openPulseRun(sc, projectID); slug != "" {
		return ""
	}
	if moved {
		return "the journal moved"
	}
	return "settled work is parked at " + parked
}

// reap abandons orphaned machine sessions — a session branch whose
// claimant is provably dead — so the run behind it re-parks at its stage
// and the ordinary groom-and-kick loop can retry it. This is the whole
// answer to "moe itself died mid-turn": the occupancy guard correctly
// holds a run with a live session branch, and without this nothing ever
// clears the branch of a process that is never coming back.
//
// The rule is the operator's: a robot session that died may be abandoned
// and retried; a human-started one may not. session.Reapable is where
// that lives — machine-marked *and* provably dead, with every ambiguous
// shape (no claim, another host, a live pid, a fresh heartbeat) reading
// as untouchable. Those sessions stay exactly as they are and surface on
// the dash's ACTIVE row; recovery is `moe session resolve` / `abandon`,
// one glance and one verb.
//
// Under the repolock because Abandon rewrites worktree state, and
// warn-only because a reap that fails is a session that stays put.
func (g *heartbeatGate) reap(log io.Writer) {
	sessions, err := session.List(g.root)
	if err != nil {
		fmt.Fprintf(log, "heartbeat: list sessions: %v\n", err)
		return
	}
	now := time.Now()
	for _, s := range sessions {
		if !session.Reapable(s, now) {
			continue
		}
		err := repolock.With(g.root, repolock.Options{
			Purpose:   "heartbeat-reap",
			Run:       s.Project + "/" + s.Run,
			Heartbeat: true,
		}, func() error { return session.Abandon(s) })
		if err != nil {
			fmt.Fprintf(log, "heartbeat: reap %s: %v\n", s.Branch, err)
			continue
		}
		fmt.Fprintf(log, "heartbeat: reaped dead machine session %s — %s/%s re-parks at %s\n",
			s.Branch, s.Project, s.Run, s.Doc)
	}
}

// projectJournalTip reports the newest journal commit touching a
// project: its sha, its commit time, and whether it was operator-
// authored. ok is false when the project has no history yet or the read
// failed.
//
// Operator-authored is the absence of both machine marks — no
// MoE-Consent, no MoE-Spawned-By. Absence normally means *unknown*
// rather than operator, and it still does for attribution; here the
// unknown case is deliberately folded into "operator", because the only
// thing it buys is one quiet tick of hesitation.
func projectJournalTip(root, projectID string) (sha string, at time.Time, operator bool, ok bool) {
	// %x00 is git's own NUL escape, expanded by git rather than passed in
	// the argv — a literal NUL in an exec argument is rejected outright.
	out, err := git.Output(root, "log", "-1", "--format=%H%x00%cI%x00%B", "--", "projects/"+projectID)
	if err != nil {
		return "", time.Time{}, false, false
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\x00", 3)
	if len(parts) != 3 || parts[0] == "" {
		return "", time.Time{}, false, false
	}
	when, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return "", time.Time{}, false, false
	}
	machine := strings.Contains(parts[2], "\nMoE-Consent:") || strings.Contains(parts[2], "\nMoE-Spawned-By:")
	return parts[0], when, !machine, true
}

// parkedKickableThread returns a thread root the heartbeat could start,
// or "" when the project has none. This is the same admit pulseSelfKick
// applies — a live thread whose root has a settled design and nobody
// inside — asked ahead of time so a project with genuinely nothing to do
// never spends an agent turn finding that out.
//
// It is deliberately the *predicate*, not a decision: what actually gets
// started is the survey's call, made with the park reasons and the
// ordering bar in hand. This only says the board is not empty.
func parkedKickableThread(root string, sc *pulseScan, projectID string) string {
	seen := map[string]bool{}
	for _, md := range sc.mds {
		if md.Project != projectID || md.Status != run.StatusInProgress || !chainableWorkflow(md.Workflow) {
			continue
		}
		rootKey := sc.graph.Root(md.Project + "/" + md.ID)
		if seen[rootKey] {
			continue
		}
		seen[rootKey] = true
		rootMd := sc.byKey[rootKey]
		if rootMd == nil || !run.ChainChildLive(rootKey, sc.byKey) {
			continue
		}
		if settled, _ := rootDesignSettled(root, rootMd, sc.idx); !settled {
			continue
		}
		if openSessionStage(root, rootMd) != "" {
			continue
		}
		return rootKey
	}
	return ""
}

// openPulseRun returns the project's in-flight survey, or "". The
// pulse's own single-flight rule read from outside: a sweep already
// running owns this generation.
func openPulseRun(sc *pulseScan, projectID string) string {
	for _, md := range sc.mds {
		if md.Project == projectID && md.Workflow == pulseWorkflow && md.Status == run.StatusInProgress {
			return md.ID
		}
	}
	return ""
}

// openSessionProjects reports which projects have a live session branch
// anywhere in them — a ride mid-hop, or the operator sitting in a stage.
// Either way somebody is already inside that project and the tail-pulse
// path owns the next sweep.
//
// One `for-each-ref` for the whole bureaucracy rather than a HasRef per
// run per stage: the tick asks this about every project, and the refs
// are all in one namespace. A read failure reports every project
// occupied — standing down on an unreadable ref list is the answer that
// loses nothing.
func openSessionProjects(root string) map[string]bool {
	out, err := git.Output(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/session/")
	if err != nil {
		return allProjectsOccupied(root)
	}
	occupied := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		branch := strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(branch, "session/")
		if !ok {
			continue
		}
		if p, _, found := strings.Cut(rest, "/"); found && p != "" {
			occupied[p] = true
		}
	}
	return occupied
}

func allProjectsOccupied(root string) map[string]bool {
	occupied := map[string]bool{}
	projects, _, err := project.List(root)
	if err != nil {
		return occupied
	}
	for _, p := range projects {
		occupied[p.ID] = true
	}
	return occupied
}

// heartbeatGate satisfies the serve seam.
var _ serve.Heartbeat = (*heartbeatGate)(nil)
