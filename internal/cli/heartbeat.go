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
// decides whether a tick is worth an agent turn, and the two reaping
// steps that run ahead of it.
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
// Its state is two cursors per project, both journal shas, both written
// when a sweep finishes:
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
//
// Alongside them, one scratch entry per in-flight sweep: dispatched
// holds where the journal stood when the gate decided to sweep. It is
// not a cursor — Swept consumes it — and it exists so the exit can tell
// its own commits from an operator's landing mid-turn. See Swept.
type heartbeatGate struct {
	root string

	mu         sync.Mutex
	tips       map[string]string
	surveyed   map[string]string
	dispatched map[string]string
}

func newHeartbeatGate(root string) *heartbeatGate {
	return &heartbeatGate{
		root:       root,
		tips:       map[string]string{},
		surveyed:   map[string]string{},
		dispatched: map[string]string{},
	}
}

// Due answers one tick: for every project the gate looked at, whether it
// warrants a sweep right now and the gate's own words for why.
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
func (g *heartbeatGate) Due(tick time.Duration, log io.Writer) []serve.HeartbeatDecision {
	g.reap(log)
	g.gc(log)

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
	now := time.Now()
	occupied := openSessionProjects(g.root, now)

	// Every project's verdict comes back, not just the ones to sweep: the
	// stand-downs are the trace that used to be invisible, and the loop
	// already computed them. Only the sweeps are logged, as before — a
	// line per quiet project per tick would bury the ones that matter.
	decisions := make([]serve.HeartbeatDecision, 0, len(projects))
	for _, p := range projects {
		sweep, held, reason := g.projectDue(sc, p.ID, occupied, tick, now, log)
		if sweep {
			fmt.Fprintf(log, "heartbeat: sweeping %s — %s\n", p.ID, reason)
		}
		decisions = append(decisions, serve.HeartbeatDecision{
			Project: p.ID, Sweep: sweep, Held: held, Reason: reason,
		})
	}
	return decisions
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
//
// And neither cursor moves at all when an operator commit landed inside
// the sweep's own window. A survey's turn lasts minutes; a hand-commit
// arriving after its board read but before this exit sits below the tip
// recorded here, and recording it would mark as surveyed a board no
// survey ever saw — a journal move the machine permanently misreads as
// its own, and a silent wedge. So the exit walks the range the survey
// could not have seen and refuses both cursors if anything in it is the
// operator's. Next tick the moved leg fires (exit tip ≠ recorded tips),
// the quiet window gets its ordinary say, and the follow-up sweep sees
// the commit. Convergence is structural: that sweep's own range holds
// only machine commits, so its exit advances and the board goes quiet.
//
// Refusing tips too, not just surveyed, is what makes the mid-sweep case
// exactly equivalent to the ordinary one — with tips advanced, only the
// parked leg would re-look, and it fires solely on a parked kickable
// thread. A design edit on an unsettled run would still be swallowed.
// Failed sweeps refuse on the same terms: the backoff still paces the
// spawn, so re-offering costs a cool-off, not a loop.
func (g *heartbeatGate) Swept(projectID string, clean bool) {
	tip, _, _, ok := projectJournalTip(g.root, projectID)
	if !ok {
		return
	}

	g.mu.Lock()
	base, dispatched := g.dispatched[projectID]
	delete(g.dispatched, projectID)
	g.mu.Unlock()

	// No dispatched entry is unreachable through serve — every Swept
	// follows a Due that recorded one — so falling through to the advance
	// keeps a direct-call test fixture honest rather than silently wedged.
	if dispatched && base != tip && operatorActedIn(g.root, projectID, base, tip) {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.tips[projectID] = tip
	if clean {
		g.surveyed[projectID] = tip
	}
}

// projectDue is the per-project gate. Returns whether to sweep, whether
// a stand-down held a sweep back, and the reason either way. Order is
// cheapest-first and stand-downs come last, so a project that is going
// to be skipped costs the least work.
//
// The stand-down reasons are display strings — they cross the seam in
// the decision and land on both dashes. They are not parsed anywhere,
// and nothing branches on them. Which of them is worth a reader's
// attention is the held bit's job instead: the two stand-downs that name
// something outside the machine (an operator commit in the quiet window,
// somebody inside the project) are held; the rest are "nothing to do"
// spelled three ways, and /serve collapses them to a count.
func (g *heartbeatGate) projectDue(sc *pulseScan, projectID string, occupied map[string]bool, tick time.Duration, now time.Time, log io.Writer) (sweep, held bool, reason string) {
	tip, tipAt, operatorTip, ok := projectJournalTip(g.root, projectID)
	if !ok {
		// A project with no journal history at all — freshly registered,
		// nothing to survey.
		return false, false, "no journal history yet"
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
		return false, true, "an operator commit landed within the last tick"
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
			return false, false, "a sweep already surveyed the current tip"
		}
		parked = parkedKickableThread(g.root, sc, projectID)
		if parked == "" {
			return false, false, "the journal hasn't moved and nothing settled is parked"
		}
	}

	// The stand-down. A ride mid-hop, an operator sitting in a stage and
	// a survey mid-turn all look the same from here — a live session
	// branch somewhere in the project — and all mean the tail-pulse path
	// already owns the next sweep. Live, not merely present: a branch
	// whose claimant is provably dead has stopped owning anything, and
	// openSessionProjects no longer counts it.
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
		return false, true, "somebody is already inside the project"
	}

	// This tick is going to sweep, so record where the journal stands now:
	// it is the base of the range Swept walks to tell the survey's own
	// commits from an operator's landing mid-turn. The dispatch tip, not
	// the tips cursor — tips holds where the journal stood at the
	// *previous* sweep's exit, and the commits between it and here are
	// what triggered this sweep, which the survey does see. Walking from
	// there would find the triggering operator commit, refuse, re-offer
	// and re-sweep forever.
	g.mu.Lock()
	g.dispatched[projectID] = tip
	g.mu.Unlock()

	if moved {
		return true, false, "the journal moved"
	}
	return true, false, "settled work is parked at " + parked
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

// gc clears session branches whose *run* is over — merged, closed or
// promoted, or a run whose run.json or project directory is gone. It is
// `moe session gc`'s rule set on the tick's cadence instead of the
// operator's memory.
//
// It exists because those branches are the other half of "somebody is
// already inside the project", and the reap above cannot touch them.
// The reap reasons from a claim: a machine session whose process is
// provably dead. A branch left behind by an ending has no claim to
// probe — the clean endings delete it, and branches older than the
// mechanism never had one — and a claim it cannot read reads as
// occupied, deliberately. So the project stands down every tick,
// forever, and nothing but a hand-run `moe session gc` ever moves it.
//
// This step reasons from the run instead, which needs no claim: a
// terminal run has nobody inside it whatever its branch says. Between
// them the two steps cover both doors residue comes in by — a branch
// predating the claim mechanism, and an ending that deletes the claim
// while the branch survives.
//
// What keeps a timer safe to point at a destructive rule is that the
// rule was already codified as trash (gc's rule 1) and that a live
// claim holds a candidate back — see sessionClaimHeld. Scan first and
// lock only when there is something to reap: a clean board is the
// common case and must not take the repo lock every twenty minutes.
//
// Warn-only like everything else in the tick. A reap that fails is a
// branch that stays and a project that stays held; the next tick
// retries by construction, and the pile is visible in the serve log.
func (g *heartbeatGate) gc(log io.Writer) {
	candidates, err := collectSessionOrphans(g.root, time.Now())
	if err != nil {
		fmt.Fprintf(log, "heartbeat: session gc scan: %v\n", err)
		return
	}
	if candidates.empty() {
		return
	}
	reaped, reapErrs, err := sessionGCPass(g.root, repolock.Options{
		Purpose: "session-gc",
		// A pass with many candidates can outlive the stale threshold,
		// and a lock read as crashed is a lock somebody takes over.
		Heartbeat: true,
	})
	if err != nil {
		fmt.Fprintf(log, "heartbeat: session gc: %v\n", err)
		return
	}
	for _, e := range reapErrs {
		fmt.Fprintf(log, "heartbeat: session gc: %s\n", e)
	}
	for _, r := range reaped {
		// Same invisibility the reap has: Abandon writes no journal
		// commit, so a freed project's board changes without either
		// cursor noticing. Clearing surveyed is what lets the parked leg
		// re-offer a thread this pass just unblocked — in the same tick,
		// because gc runs at the top of Due.
		if r.project != "" {
			g.mu.Lock()
			delete(g.surveyed, r.project)
			g.mu.Unlock()
		}
		fmt.Fprintf(log, "heartbeat: session gc removed %s\n", r.label)
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
// tick" means.
func operatorActedSince(root, projectID string, since time.Time) bool {
	return operatorActed(root, projectID, "--since="+since.Format(time.RFC3339))
}

// operatorActedIn reports whether any journal commit touching the project
// in base..tip is operator-authored. It is Swept's question — did anything
// land inside my own sweep that the survey could not have seen.
func operatorActedIn(root, projectID, base, tip string) bool {
	return operatorActed(root, projectID, base+".."+tip)
}

// operatorActed walks the bodies of the project-scoped commits selected
// by rev, whatever the caller's way of naming them.
//
// Warn-only like every other read in this file, and in the same direction
// for both callers: an unreadable log reports no operator act, which
// leaves the quiet window's tip answer standing and leaves Swept
// advancing its cursors. Refusing on a persistent read failure would make
// the moved leg fire — and sweep — every tick, the runaway rather than
// the safe default.
func operatorActed(root, projectID, rev string) bool {
	out, err := git.Output(root, "log", rev, "--format=%x00%B", "--", "projects/"+projectID)
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
//
// It is only as good as the marks: a machine commit that lands unstamped
// reads as the operator's here, which costs the sweep-exit walk a
// redundant sweep and the quiet window a tick of hesitation. walkConsent
// (ridemode.go) is the rule that keeps the journal honest for it — an
// emit site that skips the stamp is what breaks this predicate, not
// anything in this file.
func machineAuthored(body string) bool {
	return strings.Contains(body, "\nMoE-Consent:") || strings.Contains(body, "\nMoE-Spawned-By:")
}

// parkedKickableThread returns a run the heartbeat's sweep could cause
// motion on, or "" when the project has none — asked ahead of time so a
// project with genuinely nothing to do never spends an agent turn
// finding that out.
//
// It is deliberately the *predicate*, not a decision: what actually gets
// started is the survey's call, made with the park reasons and the
// ordering bar in hand. This only says the board is not empty.
//
// "Not empty" is deliberately a wider question than pulseSelfKick's
// admit, and this is the one place the two are meant to differ. The
// kick evaluates a thread at its root; this leg is a pre-ask for the
// whole *sweep*, and a sweep grooms before it kicks. So the honest
// question is "could a sweep cause motion here", which includes settled
// work queued behind a root the floor holds: the groom can re-place it
// and the kick then starts it. Mirroring the root-only admit is what
// let one seed-only head strand two settled runs behind it on
// 2026-08-12 with no tick ever firing — the floor held the thread
// correctly, and the recovery pulse.md promises never got its turn.
//
// Three shapes, one per thread:
//
//   - An operator-minted `chain` head holds its whole thread, members
//     and all. That head is the operator's staging fence — the same one
//     stagingFenced enforces against the groom's move authority — and
//     this is the one leg that would otherwise reach past it into a
//     batch someone is composing by hand. It used to fall out
//     implicitly (a stageless head never clears the design admit);
//     the member walk below makes it worth saying out loud.
//   - A root that clears the floor itself: returned, as it always was.
//   - A root held for an unsettled design: the first member behind it
//     that clears the floor on its own. Whether those members actually
//     belong behind that head is the survey's question to answer, not
//     this predicate's — see the held-head annotation in
//     chainStateBlock, which is what gets asked.
//
// A root held for *occupancy* rather than design is not looked past.
// The corpse-branch shape has its own deliberate recovery (the skip
// line is the signpost, see openSessionStage) and nothing here changes
// it.
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
		if rootMd.Workflow == chainWorkflow && rootMd.SpawnedBy == "" {
			continue
		}
		if settled, _ := rootDesignSettled(root, rootMd, sc.idx); !settled {
			// Chain edges only ever point at live runs (NewChainGraph drops
			// terminal children), so in-progress is the only liveness the
			// walk owes on top of the floor's own two questions.
			for _, key := range sc.graph.Thread(rootKey)[1:] {
				memberMd := sc.byKey[key]
				if memberMd == nil || memberMd.Status != run.StatusInProgress {
					continue
				}
				if settled, _ := rootDesignSettled(root, memberMd, sc.idx); !settled {
					continue
				}
				if openSessionStage(root, memberMd) != "" {
					continue
				}
				return key
			}
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
// *Live* is the load-bearing word, and it is not the branch. A branch
// whose claimant is provably dead is a corpse the reap deliberately
// won't clear, because an operator's session may only ever be surfaced —
// a Ctrl-C'd `moe pulse new`, a stage pane lost to a box reboot. Counting
// it as occupancy is what stands the project's heartbeat down forever:
// nothing but a human running `moe session abandon` ever moves it, and
// nothing prompts them to. So a dead claim stops vouching for occupancy
// here while the branch, the worktree and the claim stay exactly where
// `moe session resolve` expects them.
//
// One `for-each-ref` for the whole bureaucracy rather than a HasRef per
// run per stage: the tick asks this about every project, and the refs
// are all in one namespace. Enumerating refs rather than session.List is
// deliberate too — List walks `git worktree list`, so an orphan branch
// with no worktree would vanish from the check entirely rather than
// hold, the wrong direction for an ambiguous shape. A read failure
// reports every project occupied — standing down on an unreadable ref
// list is the answer that loses nothing.
func openSessionProjects(root string, now time.Time) map[string]bool {
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
		p, tail, found := strings.Cut(rest, "/")
		if !found || p == "" {
			continue
		}
		// The full triple is what keys a claim. A ref that doesn't carry
		// one — hand-made, or a shape from some older binary — can't be
		// probed, so it holds its project the way every session branch did
		// before this.
		if r, d, ok := strings.Cut(tail, "/"); ok && r != "" && d != "" && session.Dead(root, p, r, d, now) {
			continue
		}
		occupied[p] = true
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
