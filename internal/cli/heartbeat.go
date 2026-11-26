package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/stylesheet"
	"github.com/modulecollective/moe/internal/trailers"
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
// designs, nobody inside, test and review on every ride.

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
//     reason from being re-offered every tick forever. It
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
// holds where the journal stood when the ticker started the sweep. It is
// not a cursor — Swept consumes it — and it exists so the exit can tell
// its own commits from an operator's landing mid-turn. See Swept.
// Due only stages those bases in pending; Dispatching pins them, because
// only the ticker knows whether a sweep verdict actually spawns. A base
// pinned on a held tick would be consumed by whatever sweep exits next,
// stamping as surveyed a board no survey read.
// PR observations are separate from both journal cursors: prSeen holds
// acknowledged failures per run; prOffered pins the evidence passed to
// each child. Neither failed fetches nor a re-read at exit can move it.
type heartbeatGate struct {
	root string
	// beforeReapLock lets tests pause between candidate selection and locking.
	beforeReapLock func()

	mu         sync.Mutex
	tips       map[string]string
	surveyed   map[string]string
	dispatched map[string]string
	// pending is this tick's would-be dispatched set, reset by every Due.
	pending   map[string]string
	prSeen    map[string]string
	prOffered map[string][]ownedPRChecks
	// tickPRs is this tick's observation per project, so a project with
	// nothing due still shows the one sweep its PR state.
	tickPRs map[string][]ownedPRChecks
	log     io.Writer
}

func newHeartbeatGate(root string) *heartbeatGate {
	return &heartbeatGate{
		root:       root,
		tips:       map[string]string{},
		surveyed:   map[string]string{},
		dispatched: map[string]string{},
		pending:    map[string]string{},
		prSeen:     map[string]string{},
		prOffered:  map[string][]ownedPRChecks{},
		tickPRs:    map[string][]ownedPRChecks{},
	}
}

// Due answers one tick: for every project the gate looked at, whether it
// warrants a sweep right now and the gate's own words for why.
//
// Read-only and cheap by construction — one run scan, one journal index
// (memoized on HEAD), one ref listing, and a HEAD read plus two bounded
// `git log -1` queries per project (path changes and project trailers).
// Recorded pushed PRs also pay one bounded `gh pr view` per tick, outside
// the journal lock, and a red one reads its run's input file; a project
// without them pays no added network read.
// A quiet board costs no agent turn, run or journal noise, which is the
// whole reason a fixed cadence can be baked rather than tuned.
//
// Warn-only throughout, mirroring the pulse itself: a read that fails
// drops the project from this tick rather than failing the tick. The
// next one is a few minutes away and re-derives everything.
func (g *heartbeatGate) Due(log io.Writer) []serve.HeartbeatDecision {
	g.log = log
	g.mu.Lock()
	g.pending = map[string]string{}
	g.mu.Unlock()
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

	// Every project's verdict comes back, not just the ones to sweep: the
	// stand-downs are the trace that used to be invisible, and the loop
	// already computed them. None is logged here: the ticker logs a sweep
	// where it starts one, since it may still hold what the gate offered,
	// and a line per quiet project per tick would bury the ones that matter.
	decisions := make([]serve.HeartbeatDecision, 0, len(projects))
	anySweep := false
	for _, p := range projects {
		sweep, held, reason := g.projectDue(sc, p, now)
		if sweep {
			anySweep = true
		}
		decisions = append(decisions, serve.HeartbeatDecision{
			Project: p.ID, Sweep: sweep, Held: held, Reason: reason,
		})
	}
	if !anySweep {
		return decisions
	}
	// One sweep surveys every project, so each one it will see stages a
	// base now, pinned only if the ticker dispatches (Dispatching). A held
	// or paused project does not: its cursors must not move past work the
	// sweep may not start.
	for _, d := range decisions {
		if d.Sweep || d.Held || d.Reason == "paused" {
			continue
		}
		if tip, _, _, ok := projectJournalTip(g.root, d.Project); ok {
			g.mu.Lock()
			g.pending[d.Project] = tip
			g.mu.Unlock()
		}
	}
	offered := g.offeredPRJSON()
	for i := range decisions {
		if decisions[i].Sweep {
			decisions[i].PRChecks = offered
		}
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
// And neither cursor moves at all when work the survey could not have
// seen landed inside the sweep's own window. A survey's turn lasts
// minutes; anything arriving after its board read but before this exit
// sits below the tip recorded here, and recording it would mark as
// surveyed a board no survey ever saw — a journal move the machine
// permanently misreads as its own, and a silent wedge. So the exit walks
// the range the survey could not have seen and refuses both cursors if
// anything in it is unseen work. Next tick the moved leg fires (exit tip
// ≠ recorded tips), and the follow-up sweep sees it.
//
// Two things land in that range, and they refuse on the same terms
// because they are the same fact. One is an operator's hand-commit. The
// other is a ride's — a dynamic sweep runs the rides it kicks inside
// this child, so every commit those rides land is below the exit tip
// too. That half is the whole autonomous path: without it the survey
// stamps the post-ride board as surveyed, and pulse_kick's walk until a
// survey finds nothing worth chaining never takes its second step. See
// rideAuthored for which commits count.
//
// Convergence is structural for both: the follow-up sweep's own range
// holds only its own commits — machine-marked, pulse-workflow or
// workflow-less — unless it kicks rides of its own, in which case
// refusing again is exactly the point. The walk is clock-paced, one
// generation per tick at most, and it ends the first time a survey kicks
// nothing.
//
// A run the sweep minted in that range — a chore it nominated, a run its
// gate opened, a followup it harvested — is the sweep's own, so tips
// advances over it. Surveyed does not: the survey read the board before
// the mint, so the minted run is a thread it never saw, let alone
// parked. Holding surveyed drops the next tick past the surveyed
// early-return to the parked leg, which sweeps once if the mint is
// kickable and stays quiet if not. The follow-up survey sees the run,
// picks or parks it and mints nothing, and its exit sets surveyed.
//
// Refusing tips too, not just surveyed, is what makes the mid-sweep case
// exactly equivalent to the ordinary one — with tips advanced, only the
// parked leg would re-look, and it fires solely on a parked kickable
// thread. A design edit on an unsettled run would still be swallowed.
// Failed sweeps refuse on the same terms: the backoff still paces the
// spawn, so re-offering costs a cool-off, not a loop.
func (g *heartbeatGate) Swept(projectID string, clean bool) {
	if projectID != project.RootID {
		g.sweptProject(projectID, clean)
		return
	}
	g.mu.Lock()
	ids := slices.Collect(maps.Keys(g.dispatched))
	g.mu.Unlock()
	if !slices.Contains(ids, project.RootID) {
		ids = append(ids, project.RootID)
	}
	for _, id := range ids {
		g.sweptProject(id, clean)
	}
}

// Dispatching pins the bases the last Due staged: the ticker is about to
// spawn the sweep that tick asked for. It replaces rather than merges.
// A sweep spawns only when no other holds the slot, so anything still in
// dispatched is debris — a spawn that errored, an exit that skipped
// Swept — and dropping it here is why a failed spawn needs no unwind.
// prOffered is rebuilt from tickPRs, which holds exactly what this tick
// observed for every project that staged a base.
func (g *heartbeatGate) Dispatching() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dispatched = g.pending
	g.pending = map[string]string{}
	g.prOffered = map[string][]ownedPRChecks{}
	for id := range g.dispatched {
		g.prOffered[id] = g.tickPRs[id]
	}
}

// sweptProject moves one project's cursors; see Swept.
func (g *heartbeatGate) sweptProject(projectID string, clean bool) {
	tip, _, _, ok := projectJournalTip(g.root, projectID)
	if !ok {
		return
	}

	g.mu.Lock()
	base, dispatched := g.dispatched[projectID]
	if clean {
		if offered, ok := g.prOffered[projectID]; ok {
			for _, p := range offered {
				if p.Error == "" {
					g.prSeen[p.Run] = prFailureSnapshot([]ownedPRChecks{p})
				}
			}
		}
	}
	delete(g.prOffered, projectID)
	delete(g.dispatched, projectID)
	g.mu.Unlock()

	// No dispatched entry is unreachable through serve — every Swept
	// follows a Dispatching that pinned one — so falling through to the
	// advance keeps a direct-call test fixture honest rather than silently
	// wedged.
	minted := false
	if dispatched && base != tip {
		var unseen bool
		if unseen, minted = unseenWorkIn(g.root, projectID, base, tip); unseen {
			return
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.tips[projectID] = tip
	if clean && !minted {
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
// and one thing branches on them: Due reads "paused" to keep a paused
// project's cursors still. Which of them is worth a reader's
// attention is the held bit's job instead. Normal quiet decisions collapse
// to a count; the ticker may hold would-start decisions at admission.
func (g *heartbeatGate) projectDue(sc *pulseScan, p *project.Metadata, now time.Time) (sweep, held bool, reason string) {
	projectID := p.ID

	// The operator's standing cap, ahead of every read: a paused project
	// costs this tick nothing at all, which is the whole of what paused
	// promises. Not held — the operator chose this state, so a held row
	// every tick would be the panel nagging about a decision.
	// The mode badge carries the visibility instead.
	mode := project.ModeOf(p)
	if mode == project.ModePaused {
		return false, false, "paused"
	}

	tip, tipAt, _, ok := projectJournalTip(g.root, projectID)
	if !ok {
		// A project with no journal history at all — freshly registered,
		// nothing to survey.
		return false, false, "no journal history yet"
	}

	g.mu.Lock()
	seen, known := g.tips[projectID]
	if !known {
		g.tips[projectID] = tip
	}
	surveyed := g.surveyed[projectID] == tip
	g.mu.Unlock()

	// Poll before the journal-tip short circuit. External PR state and
	// check results do not move the journal. Failed reads are unknown.
	log := g.log
	if log == nil {
		log = io.Discard
	}
	prs := gatherOwnedPRChecks(sc, projectID, log)
	// A red head whose one repair attempt is spent, with no fresh note on
	// the run, does not wake: the survey could only read its own repair
	// hold and pick nothing. Only this durable hold suppresses; transient
	// ones (session, owner, control) lift without a journal commit, so
	// suppressing on them could swallow a wake for good. prSeen is left
	// unwritten so the fingerprint stays pending: the note's commit, or a
	// new head, wakes with the PR reason. Asking the journal each tick,
	// rather than acknowledging, is what keeps a serve restart (prSeen is
	// in-memory) from re-surveying every spent red PR. Read before the
	// lock, because it loads the run's input file.
	spentHead := map[string]bool{}
	for _, p := range prs {
		md := sc.byKey[p.Run]
		if p.Error != "" || md == nil || prFailureSnapshot([]ownedPRChecks{p}) == "" {
			continue
		}
		if _, spent, err := ciRepairSpent(sc, md, p); err == nil && spent {
			spentHead[p.Run] = true
		}
	}
	g.mu.Lock()
	g.tickPRs[projectID] = prs
	prChanged := false
	prFinished := false
	for _, p := range prs {
		if p.Error != "" {
			continue
		}
		if p.State == "MERGED" || p.State == "CLOSED" {
			// Reconcile is warn-only. Keep offering this fact until the
			// journal transition removes the run from the pushed set,
			// even if a survey succeeds after reconciliation failed.
			prFinished = true
		}
		snapshot := prFailureSnapshot([]ownedPRChecks{p})
		if snapshot == "" {
			g.prSeen[p.Run] = ""
		} else if snapshot != g.prSeen[p.Run] && !spentHead[p.Run] {
			prChanged = true
		}
	}
	g.mu.Unlock()
	moved := known && seen != tip
	retryAt := sc.idx.NextQuotaRetry(projectID)
	retryDue := !retryAt.IsZero() && !retryAt.After(now)
	retryChanged := false
	if !retryAt.IsZero() && !retryDue {
		var err error
		retryChanged, err = quotaSelectionChanged(g.root, sc, projectID, now)
		if err != nil {
			fmt.Fprintf(log, "quota retry selection %s: %v\n", projectID, err)
		}
	}
	unswept := false
	parked := ""
	choreReason := ""
	if !moved {
		// Ahead of the surveyed early-return, because that is exactly the
		// state a quiet project is in when a chore's clock elapses: nothing
		// has landed, the last sweep surveyed this tip, and the only thing
		// that changed is the time. Both chore legs are pure clock events
		// on a board no journal commit has touched, so every leg below
		// would read quiet and the chore would sit until a human clicked.
		//
		// Not gated on safe mode. A chore-rooted run is operator-marked by
		// construction — the chore definition *is* the mark — so the ride
		// the sweep kicks passes safe on its own terms.
		choreReason = choreClockReason(sc, projectID, now)
	}
	if !moved && choreReason == "" && !prChanged && !prFinished && !retryDue && !retryChanged {
		// A clean sweep already surveyed this exact board. Whatever is
		// parked on it, the survey saw and left parked on purpose, so
		// offering it again is re-asking a question the machine has
		// already answered — at the cost of a full agent turn every tick,
		// forever. Any change to a thread's kickability is a journal
		// commit, which the moved leg above catches.
		if surveyed {
			if !retryAt.IsZero() {
				return false, false, "deferred for quota until " + retryAt.Local().Format("2006-01-02 15:04:05 MST")
			}
			return false, false, "a sweep already surveyed the current tip"
		}
		// Work landed that no survey has seen. The moved leg is
		// edge-triggered on an in-memory cursor a restart doesn't have, and
		// seeding that cursor to the current tip makes the landing its own
		// permanent baseline — so on a board with nothing parked, a restart
		// on top of a merge reads quiet forever, not for one tick. The
		// durable answer is in the journal both sides already live in: the
		// project tip against the newest activity on the last pulse run.
		//
		// It converges because both sides are committer times on journal
		// commits at second resolution. A cleanly-swept board compares
		// equal — the pulse's close commit *is* the project tip — and After
		// is strict, so a restart on a swept board stays quiet. A sweep
		// that died leaves its open run's last commit at the tip, equal
		// again, so this leg does not self-retrigger a failure; pacing the
		// retries stays the backoff's job through the parked leg.
		//
		// Ahead of the parked probe because it is map lookups over the
		// tick's shared scan where that one forks git per thread root, and
		// when it fires the parked answer is moot — the sweep grooms and
		// kicks regardless.
		//
		// A project with no pulse history at all abstains. "Never swept" is
		// not the claim "has unswept work", and firing on it would sweep
		// every never-surveyed project at once on every restart — the storm
		// the lazy seed above exists to prevent. Those projects stay
		// reachable through the moved and parked legs exactly as before.
		if last := lastPulseAt(sc, projectID, ""); !last.IsZero() && tipAt.After(last) {
			unswept = true
		}
		if !unswept && !known && !retryAt.IsZero() {
			// On restart, second-resolution timestamps can tie an operator
			// edit to the deferral immediately before it. A future quota
			// date must not hide that fresh work until a weekly reset.
			pulseTip, err := git.Output(g.root, "log", "-1", "--format=%H", "--grep", "^MoE-Workflow: pulse$", "--", "projects/"+projectID)
			if err == nil && strings.TrimSpace(pulseTip) != "" && strings.TrimSpace(pulseTip) != tip {
				unswept = true
			}
		}
		if !unswept {
			if !retryAt.IsZero() {
				return false, false, "deferred for quota until " + retryAt.Local().Format("2006-01-02 15:04:05 MST")
			}
			// Under safe the leg offers only what the kick could actually
			// start, so a board with plenty parked and nothing marked spends
			// no agent turn discovering that every tick. The moved
			// and unswept legs above are untouched: safe stops the machine
			// disposing, not proposing, and a delta still deserves a survey.
			parked = parkedKickableThread(g.root, sc, projectID, mode == project.ModeSafe)
			if parked == "" {
				if mode == project.ModeSafe {
					return false, false, "the journal hasn't moved and nothing operator-marked is parked"
				}
				return false, false, "the journal hasn't moved and nothing settled is parked"
			}
		}
	}

	// This tick wants to sweep, so stage where the journal stands now
	// (pinned only if the ticker dispatches — see Dispatching): it is the
	// base of the range Swept walks to tell the survey's own commits from
	// an operator's landing mid-turn. The dispatch tip, not the tips
	// cursor — tips holds where the journal stood at the *previous*
	// sweep's exit, and the commits between it and here are what
	// triggered this sweep, which the survey does see. Walking from there
	// would find the triggering operator commit, refuse, re-offer and
	// re-sweep forever.
	g.mu.Lock()
	g.pending[projectID] = tip
	g.mu.Unlock()

	if prFinished {
		return true, false, "an owned PR finished; reconciliation pending"
	}
	if prChanged {
		return true, false, "new failing checks on an owned PR"
	}
	if moved {
		return true, false, "the journal moved"
	}
	if unswept {
		return true, false, "work landed since the last sweep"
	}
	if choreReason != "" {
		return true, false, choreReason
	}
	if retryDue {
		return true, false, "quota retry is due — survey the current board"
	}
	if retryChanged {
		return true, false, "quota selection changed — survey the current board"
	}
	if md := sc.byKey[parked]; md != nil && md.Workflow == dash.IdeaWorkflow {
		// Display only, like every reason here — but this is the operator's
		// whole trace of why a tick moved, and a tagged idea is not settled
		// work. It is a licence nobody has spent yet.
		return true, false, "a tagged idea is parked at " + parked
	}
	return true, false, "settled work is parked at " + parked
}

// A retry is evidence about the backend/model it checked, not a hold on all
// future selections. Read configuration only; the new survey owns quota reads
// and resolution/replacement of this journal evidence.
func quotaSelectionChanged(root string, sc *pulseScan, projectID string, now time.Time) (bool, error) {
	sheet, err := stylesheet.Load(root)
	if err == nil {
		err = sheet.Validate(stylesheetVocab())
	}
	if err != nil {
		return false, err
	}
	changed := false
	for _, retry := range sc.idx.QuotaRetries {
		if retry.Project != projectID {
			continue
		}
		for _, target := range retry.Targets {
			md := sc.byKey[target.Project+"/"+target.Run]
			if md == nil {
				continue
			}
			if md.Workflow == pulseWorkflow && target.Stage == pulseDoc {
				// The skipped survey is closed and may have had a binding;
				// the next heartbeat starts a new, unbound survey.
				md = &run.Metadata{Workflow: pulseWorkflow}
			} else {
				w := workflows[md.Workflow]
				if w == nil {
					continue
				}
				stage, kind, err := w.NextWithIndex(root, md, sc.idx)
				if err != nil {
					return false, err
				}
				if kind != NextKindStage || stage != target.Stage {
					continue
				}
			}
			name, model, _, err := selectStageAgent(root, sheet, stageSessionOpts{}, md, target.Stage, "", now)
			if err != nil {
				return false, err
			}
			changed = changed || name != target.Backend || model != target.Model
		}
	}
	return changed, nil
}

// choreClockReason is the two chore legs of the gate, in one read: does this
// project have a chore whose *clock* — not the journal — says a sweep is
// worth an agent turn? Returns the gate's words for it, or "" for
// neither.
//
// The autonomous loop already handles a chore end-to-end once a sweep
// fires: the sweep auto-opens every due chore's run, and a chore-rooted
// run is settled-by-construction and operator-marked, so the same
// sweep's kick rides it. What was missing is the trigger. A chore coming
// due is a pure clock event on a quiet journal — invisible to the moved
// leg, invisible to the parked leg — so on a quiet project a due chore
// sat in the backlog until a human clicked the web's open button. These
// two legs are that button, moved into the machine.
//
// **Mechanical.** A due chore is due: cadence elapsed, trigger paths
// touched, definition edited. Converges in one firing — the sweep opens
// the chore's run, and chore.Evaluate's open-run guard makes it not-due
// from there. Cooldown, the open-run refusal and the failure backoff
// bound the re-firing this can't converge away.
//
// **Judged.** A judged chore is never mechanically due; the survey
// judges its prose condition against what landed. On a quiet board that
// judgment is a function of exactly two things — landed work, which the
// moved leg already covers, and the cooldown expiring, which is the same
// pure clock event as above. Sweeping whenever a judged chore is merely
// eligible would burn an agent turn every tick until the
// condition happened to hold, so the leg is edge-triggered on the expiry
// with no new state: probe only when no pulse run has looked at the
// board since the chore became eligible.
//
// That converges because the probe sweep mints a pulse run of its own,
// which moves lastPulseAt past NextEligible — exactly one probe per
// expiry. A survey that declines writes nothing (a ChoreSkipped marker
// is the operator's `moe chore skip`, never the agent's), so a declined
// probe stays quiet until work lands, or until the chore completes and
// mints a fresh expiry.
//
// A judged chore with no cooldown, or one never completed, has a zero
// NextEligible and abstains — "eligible forever" is not an edge. It
// stays reachable the way it always was: registering or editing a chore
// is itself a journal commit, so the moved leg sweeps and judges it. A
// judged chore that wants periodic probing owes a cooldown.
func choreClockReason(sc *pulseScan, projectID string, now time.Time) string {
	judged := false
	for _, st := range sc.choreStates(now) {
		if st.Definition.Project != projectID {
			continue
		}
		if st.Due {
			return "a chore is due"
		}
		// CooldownBlocking is exactly now < NextEligible, so a non-zero
		// NextEligible that isn't blocking is a cooldown that has expired.
		if st.Definition.Judged() && st.OpenRun == "" && !st.NextEligible.IsZero() && !st.CooldownBlocking &&
			lastPulseAt(sc, projectID, "").Before(st.NextEligible) {
			judged = true
		}
	}
	if judged {
		return "a judged chore is eligible"
	}
	return ""
}

// reap clears orphaned machine sessions — a session branch whose
// claimant is provably dead — so the run behind it stops being held by
// the occupancy guard. This is the whole answer to "moe itself died
// mid-turn": the guard correctly holds a run with a live session branch,
// and without this nothing ever clears the branch of a process that is
// never coming back.
//
// The rule is the operator's: a robot session that died may be cleared;
// a human-started one may not. session.Reapable is where that lives —
// machine-marked *and* provably dead, with every ambiguous shape (no
// claim, another host, a live pid, a fresh heartbeat) reading as
// untouchable. Those sessions stay exactly as they are and surface on
// the dash's ACTIVE row; recovery is `moe session resolve` / `abandon`,
// one glance and one verb.
//
// *How* it clears depends on whether the turn committed a canvas
// change, and session.Close is the arbiter — the same rebase +
// ff-merge `moe session resolve` runs, not a second opinion:
//
//   - **Close succeeds** → the turn committed real work and it lands on
//     main. A dead process is not a reason to destroy a finished
//     artifact, and the journal tip moving is what gets the project
//     swept again with the landed state in view. The run stays open at
//     its stage; landing is recovery, concluding is a sweep's job.
//   - **CanvasUnchangedError** → the turn has no committed canvas
//     change. This does not establish why it stopped. Record a tombstone
//     on the run, abandon, and the run re-parks for the ordinary groom-and-kick
//     loop to retry — the original behaviour, now scoped to the shape it
//     was right for and no longer silent about it.
//   - **Any other Close error** → a branch that won't land cleanly may
//     hold real work in a broken state, and destroying it to unblock a
//     tick is the wrong trade. Leave it, log it, let the operator's
//     recovery verbs have it.
//
// The motivating incident was a pulse run: the survey finished and
// committed, the close died on a contended lock, and the next tick's
// reap deleted the only copy of a complete report — for a workflow with
// no retry leg that could ever have regenerated it.
//
// Under the repolock because both endings rewrite worktree state, and
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
		if g.beforeReapLock != nil {
			g.beforeReapLock()
		}
		landed := false
		completed := false
		tip := ""
		err := repolock.With(g.root, repolock.Options{
			Purpose:   "heartbeat-reap",
			Run:       s.Project + "/" + s.Run,
			Heartbeat: true,
		}, func() error {
			// Admission may have resumed or replaced this candidate since
			// selection. Only the current registered worktree and claim
			// can authorize destruction while we hold the same lock.
			current, err := session.List(g.root)
			if err != nil {
				return err
			}
			var matched *session.Session
			for _, candidate := range current {
				if candidate.Branch == s.Branch && candidate.WorktreePath == s.WorktreePath {
					matched = candidate
					break
				}
			}
			now := time.Now()
			if matched == nil || !session.Reapable(matched, now) {
				return nil
			}
			s = matched
			closeErr := session.Close(s)
			printSessionRecovery(s, log)
			var unchanged *session.CanvasUnchangedError
			switch {
			case closeErr == nil:
				landed = true
				completed = true
				return nil
			case errors.As(closeErr, &unchanged):
				// Ordering rule: the note lands before the evidence is
				// destroyed. A tombstone that fails leaves the branch
				// exactly where it was — the outer log says so and the
				// next tick retries — because the alternative is
				// destroying the only copy of the turn on the way to
				// recording that it existed.
				var err error
				if tip, err = tombstoneReap(g.root, s, now); err != nil {
					return err
				}
				if err := session.Abandon(s); err != nil {
					return err
				}
				completed = true
				return nil
			default:
				return closeErr
			}
		})
		if err != nil {
			fmt.Fprintf(log, "heartbeat: reap %s: %v\n", s.Branch, err)
			fmt.Fprintf(log, "heartbeat: left %s in place — `moe session resolve %s` or `moe session abandon %s`\n",
				s.Branch, s.Branch, s.Branch)
			continue
		}
		if !completed {
			continue
		}
		// The abandon ending writes a tombstone of its own; Close's
		// fast-forward moves main without either cursor having looked.
		// Clearing surveyed is what lets this tick's legs see the freed
		// (or landed) thread, and because reap runs at the top of Due it
		// happens in the same tick.
		g.mu.Lock()
		delete(g.surveyed, s.Project)
		g.mu.Unlock()
		if landed {
			fmt.Fprintf(log, "heartbeat: landed dead machine session %s — %s/%s keeps its committed %s turn\n",
				s.Branch, s.Project, s.Run, s.Doc)
			continue
		}
		fmt.Fprintf(log, "heartbeat: reaped unfinished machine session %s — %s/%s re-parks at %s, evidence at %s\n",
			s.Branch, s.Project, s.Run, s.Doc, git.ShortSHA(tip))
	}
}

// tombstoneReap records an unfinished machine turn on its run and returns the
// session branch tip it recorded, so the reap's log line can name the
// evidence too. Caller holds the repolock.
//
// Without the note, an abandoned turn reads like one the loop never
// reached. Reaping knows the canvas was unchanged, not why the agent
// stopped; the stage log can show the recorded outcome when available.
//
// It writes scalars only — see run.ReapNote. Tip is the reach: the
// abandoned commits dangle rather than vanish, so session evidence stays
// readable until gc prunes them. Archived diagnostics survive separately.
//
// The commit is machine-marked so sweep-exit accounting does not treat
// recovery bookkeeping as unseen human work. `dynamic` is the honest
// level: this gate only ticks on an armed serve (serve.Options.Dynamic).
//
// No MoE-Document trailer: the doc is in the subject and in the note,
// and stamping one would offer this commit to every reader that treats
// MoE-Document as "a turn landed for this stage" — which is exactly what
// did not happen.
func tombstoneReap(root string, s *session.Session, now time.Time) (string, error) {
	tip, err := git.RevParse(root, s.Branch)
	if err != nil {
		return "", fmt.Errorf("reap tombstone: read %s tip: %w", s.Branch, err)
	}
	md, err := run.Load(root, s.Project, s.Run)
	if err != nil {
		return "", fmt.Errorf("reap tombstone: %w", err)
	}
	md.Reaped = &run.ReapNote{Doc: s.Doc, At: now.UTC().Format(time.RFC3339), Tip: tip}
	if err := run.Save(root, md); err != nil {
		return "", fmt.Errorf("reap tombstone: %w", err)
	}
	runJSON := filepath.Join(run.Dir(s.Project, s.Run), "run.json")
	msg := fmt.Sprintf("reap: %s/%s %s turn unfinished — abandoned %s\n\n"+
		"Read the recorded outcome, when available: `moe %s log %s/%s %s`.\n"+
		"Reaping alone does not establish the cause.\n"+
		"The abandoned tip may contain session evidence readable until Git GC prunes it;\n"+
		"archived diagnostics are retained separately.\n\n",
		s.Project, s.Run, s.Doc, git.ShortSHA(tip), md.Workflow, s.Project, s.Run, s.Doc) +
		trailers.Block{
			Run:      s.Run,
			Project:  s.Project,
			Workflow: md.Workflow,
			Consent:  rideDynamic.String(),
		}.String()
	// ErrNothingToCommit means an earlier tick already landed this exact
	// note and died before abandoning; the retry proceeds to the abandon
	// it owes. Rare in practice — At has second precision, so a retry a
	// tick later stamps a fresh commit instead, and the latest reap wins.
	// Any other failure restores run.json — the caller keeps the
	// branch, so leaving the edit loose in the canonical tree would only
	// give the next session's rebase something to trip on. From HEAD, not
	// the index: Stage already ran, so a bare checkout would restore the
	// staged copy of the very edit being rolled back.
	switch err := run.StageAndCommit(root, msg, runJSON); {
	case err == nil, errors.Is(err, run.ErrNothingToCommit):
		return tip, nil
	default:
		if out, rErr := git.Combined(root, "checkout", "HEAD", "--", runJSON); rErr != nil {
			return "", fmt.Errorf("reap tombstone: %w (and restoring %s failed: %v: %s)",
				err, runJSON, rErr, strings.TrimSpace(out))
		}
		return "", fmt.Errorf("reap tombstone: %w", err)
	}
}

// gc clears session branches whose *run* is over — merged, closed or
// promoted, or a run whose run.json or project directory is gone. It is
// `moe session gc`'s rule set on the tick's cadence instead of the
// operator's memory.
//
// Unlike claim-based reap, this recovers claimless ending residue. A
// present claim must match the branch and prove its owner dead; unreadable
// or uncertain claims hold even on terminal runs (session.GCClaimHeld).
// Scan first and lock only with candidates, then recollect under the lock.
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
	}, log)
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

// projectJournalTip reports the newest journal commit touching a project
// path or carrying its exact MoE-Project trailer (including empty advance
// marks): its sha, its commit time, and whether it was operator-
// authored. ok is false when the project has no history yet or the read
// failed.
//
// Operator-authored here means absence of both machine marks; attribution
// still treats missing provenance as unknown.
func projectJournalTip(root, projectID string) (sha string, at time.Time, operator bool, ok bool) {
	head, err := git.HEAD(root)
	if err != nil {
		return "", time.Time{}, false, false
	}
	// Pin both reads to one HEAD. The range after the path candidate picks
	// by journal order, even when a later empty mark shares its timestamp.
	// %x00 is git's NUL escape; a literal NUL in argv would be rejected.
	out, err := git.Output(root, "log", head, "-1", "--format=%H%x00%cI%x00%B", "--", "projects/"+projectID)
	if err != nil {
		return "", time.Time{}, false, false
	}
	rev := head
	if pathSHA, _, found := strings.Cut(out, "\x00"); found {
		rev = pathSHA + ".." + head
	}
	marked, err := git.Output(root, "log", rev, "-1", "--format=%H%x00%cI%x00%B", "--grep", trailers.GrepPattern("MoE-Project", projectID), "--")
	if err != nil {
		return "", time.Time{}, false, false
	}
	if strings.TrimSpace(marked) != "" {
		out = marked
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

// unseenWorkIn reports whether anything landed in base..tip that the
// sweep's own survey could not have seen. It is Swept's question, and
// both halves reach it for the same reason: an operator commit arriving
// after the board read, and a ride commit — work the sweep itself
// kicked, which by construction lands after the board it was chosen
// from.
//
// minted is the third thing a sweep's window can hold: a run the sweep
// itself opened (sweptRunOpen). It is not unseen work — the survey
// asked for it — so Swept still advances tips, but it is a board no
// survey read, so Swept leaves surveyed where it was.
func unseenWorkIn(root, projectID, base, tip string) (unseen, minted bool) {
	operator, ride, minted := journalActs(root, projectID, base+".."+tip)
	return operator || ride, minted
}

// journalActs walks the bodies of the project-scoped commits selected by
// rev, whatever the caller's way of naming them, and reports both facts
// its callers need. Path changes and exact project trailers form a union:
// hand edits need no trailer, and advance marks need no changed file.
// Duplicate bodies cannot change these boolean answers.
//
// Warn-only like every other read in this file, and in the same direction
// for both acts: an unreadable log reports neither, leaving Swept advancing
// its cursors. Refusing on a persistent read failure would make the moved
// leg fire — and sweep — every tick, the runaway rather than the safe default.
func journalActs(root, projectID, rev string) (operator, ride, minted bool) {
	out, err := git.Output(root, "log", rev, "--format=%x00%B", "--", "projects/"+projectID)
	if err != nil {
		return false, false, false
	}
	marked, err := git.Output(root, "log", rev, "--format=%x00%B", "--grep", trailers.GrepPattern("MoE-Project", projectID), "--")
	if err != nil {
		return false, false, false
	}
	for body := range strings.SplitSeq(out+marked, "\x00") {
		if strings.TrimSpace(body) == "" {
			// The leading empty field before the first commit's %x00, and
			// the padding git puts between bodies. Neither is a commit, and
			// reading one as operator-authored would hold every project.
			continue
		}
		if !machineAuthored(body) {
			operator = true
		}
		if rideAuthored(body) {
			ride = true
		}
		if sweptRunOpen(body) {
			minted = true
		}
		if operator && ride {
			break
		}
	}
	return operator, ride, minted
}

// sweptRunOpen reports whether a commit body opens a run a sweep caused:
// a judged chore it nominated (MoE-Chore), a run its gate minted or an
// idea it promoted (MoE-Spawned-By), or a followup it harvested
// (MoE-From-Run). Those are every run-open a survey's own turn lands, and
// none of them was on the board the survey read — so the survey cannot
// have parked them, and the sweep's exit must not stamp the post-mint
// board as surveyed. Without this, a mint the survey left out of `next`
// sits unstarted until something unrelated moves the journal.
//
// The subject pin is the journal index's own (run.go pins OpenConsent
// the same way). It keeps out the trailered commits that follow an open
// — a promote's idea mark, an intent link — and the sweep's own pulse
// open, which carries none of the three.
//
// Over-matches one shape on purpose: an intent the gate opened carries
// MoE-Spawned-By too. Holding surveyed for it costs nothing unless
// something else kickable is parked, and then one follow-up sweep.
func sweptRunOpen(body string) bool {
	if !strings.HasPrefix(body, "Open run ") {
		return false
	}
	return strings.Contains(body, "\nMoE-Chore:") ||
		strings.Contains(body, "\nMoE-Spawned-By:") ||
		strings.Contains(body, "\nMoE-From-Run:")
}

// rideAuthored reports whether a commit body is a ride's: its
// MoE-Workflow trailer names a workflow whose runs spend agent turns.
// That is every way a ride lands work — a stage turn, a push's merge, a
// ridden run's close — and it is the mark that tells
// a sweep's window "something the survey never read is now on the board".
//
// Two workflows are excluded, and the sweep's whole bookkeeping falls out
// of the two:
//
//   - The pulse's own. A survey's run open, work and close commits are
//     the sweep describing itself.
//   - The capture workflows (dash.IsCapture — ideas and intents). No
//     agent ever runs on them, so a survey's own grooms — a tag, an
//     untag, a promote's status bump — are the sweep acting on the board
//     it just read, not new work landing under it. Excluding them is what
//     keeps an ordinary groom-only sweep from earning itself a follow-up
//     every tick.
//
// Commits with no MoE-Workflow trailer at all — a run mint, a harvest, a
// sync bump — are nobody's ride and fall out by construction.
func rideAuthored(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "MoE-Workflow:")
		if !ok {
			continue
		}
		if w := strings.TrimSpace(v); w != pulseWorkflow && !dash.IsCapture(w) {
			return true
		}
	}
	return false
}

// machineAuthored reports whether a commit body carries either machine mark.
// An unstamped machine commit reads as unknown/operator work here and costs
// the sweep-exit walk a redundant sweep. walkConsent (ridemode.go) keeps the
// journal honest; an emit site skipping the stamp breaks this predicate.
func machineAuthored(body string) bool {
	return strings.Contains(body, "\nMoE-Consent:") || strings.Contains(body, "\nMoE-Spawned-By:")
}

// parkedKickableThread returns a run a sweep could cause motion on, or ""
// when the project has none, so a project with nothing to do never spends
// an agent turn finding that out. It is a predicate rather than a decision:
// what starts is the survey's pick, and the floor asks again at the ride.
//
// markedOnly is the project's `safe` mode: the question narrows to motion
// the operator has already licensed. A tagged idea passes by construction,
// since the tag is the mark.
func parkedKickableThread(root string, sc *pulseScan, projectID string, markedOnly bool) string {
	for _, key := range kickableRuns(sc, projectID) {
		md := sc.byKey[key]
		// A reaped turn's release is an operator touch, and it is on the dash.
		if reapHeldReason(md, sc.idx.LastOperatorActivity[key]) != "" {
			continue
		}
		if settled, turnClosed := rootDesignSettled(root, md, sc.idx); !settled {
			if adm, _ := unsettledAdmission(sc, md, turnClosed); adm == admitNone {
				continue
			}
		}
		if openSessionStage(root, md) != "" || runOwnerHold(root, md) != "" {
			continue
		}
		if markedOnly && !operatorMarked(root, md, sc.mds, sc.idx) {
			continue
		}
		return key
	}
	for _, md := range sc.mds {
		if md.Project == projectID && md.Status == run.StatusInProgress &&
			md.Workflow == dash.IdeaWorkflow && md.PromoteTo != "" {
			return md.Project + "/" + md.ID
		}
	}
	return ""
}

// heartbeatGate satisfies the serve seam.
var _ serve.Heartbeat = (*heartbeatGate)(nil)

// offeredPRJSON is the exact PR observation this tick hands the sweep,
// across every project it staged a base for. Dispatching pins the same
// set into prOffered, so the child is handed what Swept acknowledges.
func (g *heartbeatGate) offeredPRJSON() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 {
		return ""
	}
	prs := []ownedPRChecks{}
	for _, id := range slices.Sorted(maps.Keys(g.pending)) {
		prs = append(prs, g.tickPRs[id]...)
	}
	b, _ := json.Marshal(prs)
	return string(b)
}
