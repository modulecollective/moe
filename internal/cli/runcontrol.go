package cli

// The owner's side of operator run controls (internal/runcontrol). Every
// owner consults the run's control record at each boundary — before a
// stage is admitted or its agent launched, at a next-stage decision,
// before a gate kickback, a ship attempt, a nominated close or a chain
// ride — and at a quota wait. The record is re-read every time: a
// sweep's snapshot from before the request is exactly what must not
// decide.
//
// Boundaries read the record without the repository lock. Writers
// replace it atomically, so a read sees a whole record — but possibly
// one whose commit has not landed and may yet roll back. That read is
// only ever used to stop: a hold or park it sees costs at most one
// spurious yield. Nothing that grants or acknowledges reads it that
// way. The one grant — finish — is applied through runcontrol.Settle,
// which re-reads under the lock before anything is dispatched, and a
// client confirming a request it lost the answer to goes through
// runcontrol.Submit, which reads under the lock too.
//
// Workflow code still owns gates, recovery and shipping. This file only
// decides whether the next transition may start, and finish only picks
// the `!!` walk the operator could have typed at the same boundary.

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runsock"
)

// exitYielded is the exit code of an owner that stopped because the
// operator asked it to: a hold or park honoured at a boundary. It is
// neither a failure (no backoff, no "failed" summary) nor an interrupt
// (a sweep carries on with unrelated threads).
const exitYielded = 77

// controlPollInterval paces the control watcher behind waits (quota,
// the next-stage prompt). A hold wakes a wait within this long.
var controlPollInterval = 2 * time.Second

// controlCheckpoint returns 0 when the owner may start boundary, or
// exitYielded after honouring a hold, a recovery block or a park. An
// unreadable record yields rather than reading as "no hold".
//
// With a live endpoint the read runs under its dispatch lock and an
// admitted boundary is published before the lock is released, so a
// request arriving over the socket lands either before this read or
// after the transition is visibly under way — never in between.
func controlCheckpoint(root string, md *run.Metadata, boundary string, stderr io.Writer) int {
	if !operatorCascades(md.Workflow) {
		return 0
	}
	srv := runEndpoint(root, md.Project, md.ID)
	if srv == nil {
		return checkControl(root, md, boundary, stderr)
	}
	code := 0
	phase, stage := boundaryPhase(boundary)
	srv.Checkpoint(func() bool {
		code = checkControl(root, md, boundary, stderr)
		return code == 0
	}, func(s *runsock.Status) {
		s.Phase, s.Waiting = phase, ""
		if stage != "" {
			if s.Stage != stage {
				s.Agent = ""
			}
			s.Stage = stage
		}
	})
	return code
}

func checkControl(root string, md *run.Metadata, boundary string, stderr io.Writer) int {
	key := md.Project + "/" + md.ID
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		moePrintf(stderr, "control: %v — stopping before %s\n", err, boundary)
		return exitYielded
	}
	settled, err := reconcileFinishes(root, md, f)
	if err != nil {
		moePrintf(stderr, "control: %v — stopping before %s\n", err, boundary)
		return exitYielded
	}
	if settled {
		if f, err = runcontrol.Load(root, md.Project, md.ID); err != nil {
			moePrintf(stderr, "control: %v — stopping before %s\n", err, boundary)
			return exitYielded
		}
	}
	if f.Held() {
		if r, ok := f.Find(f.Hold.Request); ok && !r.Settled() {
			settleControl(root, md, r.ID, runcontrol.Settlement{
				State: runcontrol.StateCompleted, Boundary: boundary, Outcome: "held before " + boundary,
			}, stderr)
		}
		moePrintf(stderr, "control: %s is held (%s) — stopped before %s; `moe control resume %s` releases it\n",
			key, f.Hold.Kind, boundary, key)
		return exitYielded
	}
	if f.Recovery != nil {
		moePrintf(stderr, "control: %s needs recovery (%s) — stopped before %s; `moe control resume %s` retires it\n",
			key, f.Recovery.Reason, boundary, key)
		return exitYielded
	}
	if parks := f.Open(runcontrol.KindPark); len(parks) > 0 {
		gen := ownerGeneration(root, md.Project, md.ID)
		yielded := false
		for _, r := range parks {
			if r.Owner != "" && r.Owner != gen {
				settleControl(root, md, r.ID, runcontrol.Settlement{
					State: runcontrol.StateCompleted, Outcome: "retired: the owner it addressed has exited",
				}, stderr)
				continue
			}
			settleControl(root, md, r.ID, runcontrol.Settlement{
				State: runcontrol.StateCompleted, Boundary: boundary, Outcome: "yielded before " + boundary,
			}, stderr)
			yielded = true
		}
		if yielded {
			moePrintf(stderr, "control: %s parked by the operator — yielded before %s\n", key, boundary)
			return exitYielded
		}
	}
	return 0
}

// cascadeCheckpoint is controlCheckpoint for the cascade seams, which
// carry no root. A root that cannot be found admits: the dispatch that
// follows needs one too and fails loudly on its own.
func cascadeCheckpoint(md *run.Metadata, boundary string, stderr io.Writer) int {
	root, err := findRoot(io.Discard)
	if err != nil {
		return 0
	}
	return controlCheckpoint(root, md, boundary, stderr)
}

// stageEntry is the stage-admission half of the checkpoint, read once
// the run guard is held. fresh reports that this entry acquired the
// guard rather than borrowing an outer one; interactive that the
// operator typed it at a terminal.
//
// A hold or recovery block refuses every borrowed, headless and machine
// entry. A fresh interactive entry is the operator explicitly resuming:
// it is admitted, and release (called once quota admission succeeds)
// clears the hold, so a failed admission leaves the run held.
//
// A fresh entry also retires parks still open: they were accepted
// against an owner that has since exited.
type stageEntry struct {
	root    string
	md      *run.Metadata
	release bool
}

func admitStageEntry(root string, md *run.Metadata, boundary string, fresh, interactive bool, acquired time.Time, stderr io.Writer) (*stageEntry, int) {
	e := &stageEntry{root: root, md: md}
	if !operatorCascades(md.Workflow) {
		return e, 0
	}
	key := md.Project + "/" + md.ID
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		moePrintf(stderr, "control: %v — not starting %s\n", err, key)
		return nil, exitYielded
	}
	if fresh {
		for _, r := range f.Open(runcontrol.KindPark) {
			if r.AcceptedAt.Before(acquired.Truncate(time.Second)) {
				settleControl(root, md, r.ID, runcontrol.Settlement{
					State: runcontrol.StateCompleted, Outcome: "retired: the owner it addressed has exited",
				}, stderr)
			}
		}
	}
	if (f.Held() || f.Recovery != nil) && fresh && interactive {
		e.release = true
		return e, 0
	}
	return e, controlCheckpoint(root, md, boundary, stderr)
}

// admitted runs once quota admission has passed, before the agent
// launches: it re-checks for a control that arrived during the wait, and
// for an explicit interactive entry clears the hold it was let in past.
func (e *stageEntry) admitted(boundary string, stderr io.Writer) int {
	if e == nil {
		return 0
	}
	if !e.release {
		return controlCheckpoint(e.root, e.md, boundary, stderr)
	}
	md := e.md
	r, released, err := runcontrol.ReleaseForEntry(e.root, md.Project, md.ID, heldRun(e.root, md.Project, md.ID))
	if err != nil {
		moePrintf(stderr, "control: release %s/%s: %v — not starting\n", md.Project, md.ID, err)
		return 1
	}
	if released {
		moePrintf(stderr, "control: %s/%s — %s; this invocation resumes it\n", md.Project, md.ID, r.Outcome)
	}
	return 0
}

// controlHoldPhrase is the planners' read: the skip-line tail for a run
// the operator holds, or "" when it may be admitted.
func controlHoldPhrase(root string, md *run.Metadata) string {
	if !operatorCascades(md.Workflow) {
		return ""
	}
	f, err := runcontrol.Load(root, md.Project, md.ID)
	switch {
	case err != nil:
		return "has an unreadable control record (" + err.Error() + ") — skipping"
	case f.Held():
		return "is held by the operator (" + string(f.Hold.Kind) + ") until `moe control resume` — skipping"
	case f.Recovery != nil:
		return "needs operator recovery (" + f.Recovery.Reason + ") — skipping"
	}
	return ""
}

// finishInFlight marks finish requests this process has applied and not
// yet settled, so a checkpoint nested inside the finish's own `!!` walk
// does not read its applied state as a dead owner's.
var (
	finishInFlightMu sync.Mutex
	finishInFlight   = map[string]bool{}
)

func markFinishInFlight(id string, on bool) {
	finishInFlightMu.Lock()
	defer finishInFlightMu.Unlock()
	if on {
		finishInFlight[id] = true
	} else {
		delete(finishInFlight, id)
	}
}

// reconcileFinishes settles finishes some earlier owner applied and
// never completed. Only a guard holder may: otherwise the applier may
// still be running. A run that has shipped since the finish was applied
// completes it; any other outcome is unknown and raises recovery —
// never a replay. A run already pushed at application (a CI repair)
// proves nothing by still being pushed, so it raises recovery too.
//
// Reports whether it settled anything. An error means a receipt could
// not be recorded: the caller must not admit work on a record that
// still reads as if nothing were uncertain.
func reconcileFinishes(root string, md *run.Metadata, f runcontrol.File) (settled bool, err error) {
	if heldRun(root, md.Project, md.ID) == nil {
		return false, nil
	}
	for _, r := range f.Requests {
		if r.Kind != runcontrol.KindFinish || r.State != runcontrol.StateApplied {
			continue
		}
		finishInFlightMu.Lock()
		mine := finishInFlight[r.ID]
		finishInFlightMu.Unlock()
		if mine {
			continue
		}
		s := runcontrol.Settlement{State: runcontrol.StateFailed, Boundary: r.Boundary,
			Outcome: "the owner that applied it exited; outcome unknown", Recovery: true}
		if fresh, err := run.Load(root, md.Project, md.ID); err == nil && r.ShippedBy(fresh.Status) {
			s = runcontrol.Settlement{State: runcontrol.StateCompleted, Boundary: r.Boundary,
				Outcome: "shipped (reconciled after its owner exited)"}
		}
		if err := runcontrol.Settle(root, md.Project, md.ID, r.ID, s, walkConsent()); err != nil {
			return settled, fmt.Errorf("reconcile finish %s: %w", r.ID, err)
		}
		settled = true
	}
	return settled, nil
}

// openFinish returns the run's accepted finish, if any.
func openFinish(root string, md *run.Metadata) (runcontrol.Request, bool) {
	if !operatorCascades(md.Workflow) {
		return runcontrol.Request{}, false
	}
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		return runcontrol.Request{}, false
	}
	for _, r := range f.Open(runcontrol.KindFinish) {
		if r.State == runcontrol.StateAccepted {
			return r, true
		}
	}
	return runcontrol.Request{}, false
}

// applyFinish carries a finish out from boundary: it records the
// request as applied — refusing to start if that cannot be recorded —
// runs walk, which must be this run's `!!` through the ordinary gates
// and ship route, and records what actually happened. A clean ship has
// usually completed it already (finishAtShip).
func applyFinish(root string, md *run.Metadata, r runcontrol.Request, boundary string, stdout, stderr io.Writer, walk func() int) int {
	markFinishInFlight(r.ID, true)
	defer markFinishInFlight(r.ID, false)
	if err := runcontrol.Settle(root, md.Project, md.ID, r.ID, runcontrol.Settlement{
		State: runcontrol.StateApplied, Boundary: boundary,
	}, walkConsent()); err != nil {
		moePrintf(stderr, "control: finish %s: %v — not starting\n", r.ID, err)
		return 1
	}
	moePrintf(stdout, "control: finishing %s/%s at the operator's request (!! from %s)\n", md.Project, md.ID, boundary)
	code := walk()
	s := runcontrol.Settlement{State: runcontrol.StateFailed, Boundary: boundary, Outcome: "stopped (exit " + strconv.Itoa(code) + ")"}
	switch {
	case finishShipped(root, md, r.ID):
		s.State, s.Outcome = runcontrol.StateCompleted, "shipped"
	case code == exitYielded:
		s.State, s.Outcome = runcontrol.StateCompleted, "superseded by a later hold or park"
	}
	settleControl(root, md, r.ID, s, stderr)
	return code
}

// finishShipped reports whether the run has shipped since the finish id
// was applied (runcontrol.Request.ShippedBy).
func finishShipped(root string, md *run.Metadata, id string) bool {
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		return false
	}
	r, ok := f.Find(id)
	fresh, err := run.Load(root, md.Project, md.ID)
	return ok && err == nil && r.ShippedBy(fresh.Status)
}

// finishGateBlocked completes an open finish that met a blocked gate:
// finish obeys the gates, so a blocked one is its outcome, not a reason
// to queue it for another ride.
func finishGateBlocked(root string, md *run.Metadata, stage string, stderr io.Writer) {
	if r, ok := openFinish(root, md); ok {
		settleControl(root, md, r.ID, runcontrol.Settlement{
			State: runcontrol.StateFailed, Boundary: "stage " + stage, Outcome: stage + " gate blocked",
		}, stderr)
	}
}

// finishWalkStopped completes an open finish whose run's walk just
// stopped with a failure: that stop is the finish's outcome, recorded as
// failed, so it can't widen a later invocation. Every walk that carries
// a finish's run through cascadeFromGate settles here — a kick, the
// operator's typed cascade, and each run a chain ride reaches. A yield
// leaves the finish to the hold or park that caused it, a quota deferral
// to the ride that retries, and a shipped run completed it at the ship
// seam (finishAtShip). Reports whether it settled one.
func finishWalkStopped(root string, md *run.Metadata, res cascadeResult, code int, stderr io.Writer) bool {
	if code == 0 || code == exitYielded || code == exitQuotaDeferred || res.shipped {
		return false
	}
	r, ok := openFinish(root, md)
	if !ok {
		return false
	}
	outcome := fmt.Sprintf("the walk stopped (exit %d)", code)
	if len(res.ran) > 0 {
		outcome = fmt.Sprintf("%s stopped (exit %d)", res.ran[len(res.ran)-1].stage, code)
	}
	settleControl(root, md, r.ID, runcontrol.Settlement{State: runcontrol.StateFailed, Outcome: outcome}, stderr)
	moePrintf(stderr, "control: finish on %s/%s stopped: %s\n", md.Project, md.ID, outcome)
	return true
}

// finishAtShip is the ship seam's half of finish: once this run has
// shipped, any finish waiting on it is complete and the chain ride it
// would have started is withheld. Returns whether to withhold the ride.
func finishAtShip(md *run.Metadata, stderr io.Writer) bool {
	root, err := findRoot(io.Discard)
	if err != nil || !operatorCascades(md.Workflow) {
		return false
	}
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		return false
	}
	withheld := false
	for _, r := range f.Open(runcontrol.KindFinish) {
		settleControl(root, md, r.ID, runcontrol.Settlement{
			State: runcontrol.StateCompleted, Boundary: "push", Outcome: "shipped; chain continuation withheld",
		}, stderr)
		withheld = true
	}
	return withheld
}

// settleControl records a receipt, warning rather than failing the walk:
// the act has already happened, and an unrecorded receipt stays visible
// as a request still open.
func settleControl(root string, md *run.Metadata, id string, s runcontrol.Settlement, stderr io.Writer) {
	if err := runcontrol.Settle(root, md.Project, md.ID, id, s, walkConsent()); err != nil {
		moePrintf(stderr, "control: record %s %s on %s/%s: %v\n", id, s.State, md.Project, md.ID, err)
	}
}

// watchControl wakes a wait when the run gains something a boundary
// must act on: a hold, a recovery block, an open park, or (when
// withFinish) an accepted finish. It polls the record on
// controlPollInterval; stop joins the poller.
func watchControl(root string, md *run.Metadata, withFinish bool) (<-chan struct{}, func()) {
	if !operatorCascades(md.Workflow) {
		return nil, func() {}
	}
	wake := make(chan struct{})
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(controlPollInterval)
		defer t.Stop()
		for {
			if controlWants(root, md, withFinish) {
				close(wake)
				return
			}
			select {
			case <-quit:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return wake, func() {
		once.Do(func() { close(quit) })
		<-done
	}
}

func controlWants(root string, md *run.Metadata, withFinish bool) bool {
	f, err := runcontrol.Load(root, md.Project, md.ID)
	if err != nil {
		return true // the boundary refuses motion on it
	}
	if f.Held() || f.Recovery != nil || len(f.Open(runcontrol.KindPark)) > 0 {
		return true
	}
	if withFinish {
		for _, r := range f.Open(runcontrol.KindFinish) {
			if r.State == runcontrol.StateAccepted {
				return true
			}
		}
	}
	return false
}

// ownerGeneration is this invocation's endpoint generation for a run,
// or "" when it serves none.
func ownerGeneration(root, projectID, runID string) string {
	if srv := runEndpoint(root, projectID, runID); srv != nil {
		return srv.Snapshot().Generation
	}
	return ""
}

// errControlWake ends a decision prompt that a control woke.
var errControlWake = errors.New("woken by an operator control")

// controlAtDecision answers a next-stage prompt that a control woke.
// A hold or park yields; a finish runs this run's `!!` from stage, or,
// when stage is a blocked gate, completes as gate-blocked.
func controlAtDecision(root string, md *run.Metadata, stage string, blocked bool, stdout, stderr io.Writer) int {
	moePrintln(stdout, "")
	if code := controlCheckpoint(root, md, "the next-stage decision", stderr); code != 0 {
		return code
	}
	r, ok := openFinish(root, md)
	if !ok {
		return 0
	}
	if blocked {
		finishGateBlocked(root, md, stage, stderr)
		moePrintf(stderr, "control: finish on %s/%s stopped: %s gate blocked\n", md.Project, md.ID, stage)
		return 0
	}
	return applyFinish(root, md, r, "stage "+stage, stdout, stderr, finishWalk(root, md, stage, stdout, stderr))
}

// finishWalk is the `!!` a finish runs from stage: the same walk the
// operator would have typed at that prompt.
func finishWalk(root string, md *run.Metadata, stage string, stdout, stderr io.Writer) func() int {
	return func() int {
		if stage == "push" {
			return promptPushCascadeShip(md, false, rideModeForAnswer("!!"), stdout, stderr)
		}
		return dispatchCascade("!!", stage, root, md, stdout, stderr)
	}
}
