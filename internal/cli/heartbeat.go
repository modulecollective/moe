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
// Its only state is two cursors per project, both journal shas, both
// written when a sweep finishes:
//
//   - tips — where the journal stood when the last sweep finished, clean
//     or not. Answers "has anything happened since I last looked".
//   - surveyed — where it stood when the last *clean* sweep finished.
//     Answers "has a survey already looked at this exact board", which
//     is what keeps a thread the survey deliberately parked with a
//     reason from being re-offered every twenty minutes forever. It
//     works because a project's kickable-thread state is a function of
//     its journal tip: everything the parked leg reads is
//     journal-derived, and a sweep surveys the whole board rather than
//     one thread.
//
// In-memory on purpose, both of them: a serve that just started has,
// correctly, never looked. Seeding tips lazily (the first observation
// records it and reads as *not* moved) is what keeps a restart from
// sweeping every project on a quiet board — an armed serve picks up
// existing work through the parked-thread leg instead, which is the leg
// that means something, and an empty surveyed map leaves that pickup
// untouched.
type heartbeatGate struct {
	root string

	mu       sync.Mutex
	tips     map[string]string
	surveyed map[string]string
}

func newHeartbeatGate(root string) *heartbeatGate {
	return &heartbeatGate{root: root, tips: map[string]string{}, surveyed: map[string]string{}}
}

// Due answers one tick: which projects warrant a sweep right now.
//
// Read-only and cheap by construction — one run scan, one journal index
// (memoized on HEAD), one ref listing, and one `git log -1` per project.
// A project whose tip is machine-authored and younger than one tick pays
// one more log, scanning the window for a masked operator act; a quiet
// board never reaches it. A quiet board therefore costs no agent turn, no
// run, and no journal noise, which is the whole reason a fixed cadence
// can be baked rather than tuned.
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
//
// Only a clean sweep also moves the surveyed cursor. A sweep that died
// answered nothing, so the parked leg must keep offering its board —
// that is what the failure backoff is pacing, and what keeps a dead
// vendor night from wedging the project.
func (g *heartbeatGate) Swept(projectID string, clean bool) {
	tip, _, _, ok := projectJournalTip(g.root, projectID)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tips[projectID] = tip
	if clean {
		g.surveyed[projectID] = tip
	}
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
	//
	// The whole window, not just its tip: a machine commit landing on top
	// of a hand-edit — a ride merging and closing while the operator
	// stages loose runs — would otherwise mask the act it landed on and
	// hand the half-arranged board straight to a sweep.
	if now.Sub(tipAt) < tick && (operatorTip || operatorActedSince(g.root, projectID, now.Add(-tick))) {
		return ""
	}

	g.mu.Lock()
	seen, known := g.tips[projectID]
	if !known {
		g.tips[projectID] = tip
	}
	surveyed := g.surveyed[projectID] == tip
	g.mu.Unlock()

	moved := known && seen != tip
	parked := ""
	if !moved {
		// A clean sweep already surveyed this exact board. Whatever is
		// parked on it, the survey saw and left parked on purpose, so
		// offering it again is re-asking a question the machine has
		// already answered — at the cost of a full agent turn every tick,
		// forever. Any change to a thread's kickability is a journal
		// commit, which the moved leg above catches.
		if surveyed {
			return ""
		}
		parked = parkedKickableThread(g.root, sc, projectID)
		if parked == "" {
			return ""
		}
	}

	// The stand-down. A ride mid-hop, an operator sitting in a stage and
	// a survey mid-turn all look the same from here — a live session
	// branch somewhere in the project — and all mean the tail-pulse path
	// already owns the next sweep.
	//
	// Deliberately *not* also "no open pulse run", which is what the
	// design's gate list says and what the recovery story it sits beside
	// contradicts. A survey holds a session branch for its whole agent
	// turn, so an in-flight one is already caught here and the ticker
	// single-flights its own children besides; all a run-status check
	// adds on top is the sweeps that *died*. Those are exactly the ones
	// that must not block. A failed survey leaves its run open on the
	// dash's ACTIVE list until a human closes it (escalation by
	// visibility), so standing down on it would let the first vendor
	// failure wedge this project's heartbeat indefinitely — and would
	// leave the failure backoff pacing a loop that never gets to run.
	// Repeat failures are bounded by that backoff instead, which is the
	// job it was written for.
	if occupied[projectID] {
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
		// Abandon removes a branch and a worktree and writes no journal
		// commit, so a reap changes the board invisibly to both cursors.
		// Clearing surveyed is what lets the parked leg re-offer the freed
		// thread — and because reap runs at the top of Due, it happens in
		// the same tick, which is today's recovery behaviour for "moe died
		// mid-turn".
		g.mu.Lock()
		delete(g.surveyed, s.Project)
		g.mu.Unlock()
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
	return parts[0], when, !machineAuthored(parts[2]), true
}

// operatorActedSince reports whether any journal commit touching the
// project since t is operator-authored. It is the quiet window's real
// question — the tip alone answers a narrower one, and a machine commit
// landing on top of a hand-edit is exactly how the two diverge.
//
// --since filters on committer date, the same field the tip read compares
// (%cI), so both halves of the window agree on what "younger than one
// tick" means. Warn-only like every other read here: an unreadable log
// reports no operator act and leaves the tip's answer standing.
func operatorActedSince(root, projectID string, since time.Time) bool {
	out, err := git.Output(root, "log", "--since="+since.Format(time.RFC3339), "--format=%x00%B", "--", "projects/"+projectID)
	if err != nil {
		return false
	}
	for body := range strings.SplitSeq(out, "\x00") {
		if strings.TrimSpace(body) == "" {
			// The leading empty field before the first commit's %x00, and
			// the padding git puts between bodies. Neither is a commit, and
			// reading one as operator-authored would hold every project.
			continue
		}
		if !machineAuthored(body) {
			return true
		}
	}
	return false
}

// machineAuthored reports whether a commit body carries either machine
// mark. Its inverse is what both halves of the quiet window call
// operator-authored, so the two cannot drift apart.
func machineAuthored(body string) bool {
	return strings.Contains(body, "\nMoE-Consent:") || strings.Contains(body, "\nMoE-Spawned-By:")
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
