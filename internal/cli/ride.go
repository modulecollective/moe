package cli

import (
	"io"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
)

func init() {
	// Chains are gone, but closed chain runs stay on disk and must still
	// load, list and render. No stages, so nothing can open a session.
	w := NewWorkflow(dash.ChainWorkflow)
	w.RegisterDoc("chain")
	RegisterWorkflow(w)
}

// rideRun rides one parked run headlessly from its next pending stage to
// its ship, then stops. It is the pulse's one start. mode is the consent
// the ride's commits carry.
//
// oneStage bounds the ride to the next stage, the machine's `!` where the
// default is its `!!`. A design-only spawn's single design turn goes
// through it, so the bound shares this validation and consent stamp.
func rideRun(root, projectID, runID string, mode rideMode, oneStage bool, stdout, stderr io.Writer) int {
	defer withRideMode(mode)()
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "ride: %v\n", err)
		return 1
	}
	// Admission's authoritative half of the floor's busy read: take the
	// run's guard before reloading it and hold it through the ride, so an
	// operator prompt that opened after the pick still wins.
	fresh, acquired := heldRun(root, projectID, runID) == nil, time.Now()
	releaseRun, ok := ownRunForVerb(root, projectID, runID, stderr)
	if !ok {
		return 1
	}
	defer releaseRun()
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "ride: %v\n", err)
		return 1
	}
	// A machine admission: a hold, recovery block or park stops it here,
	// and nothing it does clears a hold.
	if _, code := admitStageEntry(root, md, "ride", fresh, false, acquired, stderr); code != 0 {
		return code
	}
	if !operatorCascades(md.Workflow) {
		moePrintf(stderr, "ride: %s/%s is a %s run — not rideable\n", projectID, runID, md.Workflow)
		return 1
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "ride: %v\n", err)
		return 1
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "ride: next stage: %v\n", err)
		return 1
	}
	if kind != NextKindStage || next == "" {
		// Nothing pending. A parked run is never auto-closed.
		return 0
	}
	// A pending operator finish turns this ride into this run's `!!`
	// through the ordinary gates.
	if r, ok := openFinish(root, md); ok {
		return applyFinish(root, md, r, "stage "+next, stdout, stderr, func() int {
			res, code := cascadeFromGate(next, "", false, md, stdout, stderr)
			if summary := renderCascadeSummary(projectID+"/"+runID, res); summary != "" {
				moePrintln(stdout, summary)
			}
			return code
		})
	}
	res, code := cascadeFromGate(next, "" /*destination*/, oneStage, md, stdout, stderr)
	if summary := renderCascadeSummary(projectID+"/"+runID, res); summary != "" {
		moePrintln(stdout, summary)
	}
	return finishAfterKick(root, md, res, code, oneStage, stdout, stderr)
}

// finishAfterKick acts on a finish the operator asked for while a kick's
// walk ran. The boundary that walk stopped at is the finish's next one,
// so it is applied or settled there rather than left open for another
// ride:
//
//   - a yield leaves it to the hold or park that caused it, and a quota
//     deferral to the ride that retries the kick;
//   - a stage that stopped (finishWalkStopped), a blocked gate, or a
//     full kick that ended without shipping is the finish's outcome,
//     recorded as failed;
//   - after a one-stage kick's stage settles cleanly, the finish runs
//     this run's `!!` from that stage's successor — not Workflow.Next,
//     which reports the just-settled stage as parked until its
//     successor has a turn.
//
// A full kick that shipped has completed the finish at the ship seam
// (finishAtShip). A finish accepted but never reached because this
// process died stays accepted for a later owner; that is a different
// rule and not this one.
func finishAfterKick(root string, md *run.Metadata, res cascadeResult, code int, oneStage bool, stdout, stderr io.Writer) int {
	if finishWalkStopped(root, md, res, code, stderr) {
		return code
	}
	r, ok := openFinish(root, md)
	if !ok || code != 0 || res.shipped {
		return code
	}
	if len(res.ran) == 0 {
		return code
	}
	last := res.ran[len(res.ran)-1].stage
	blocked := false
	if md.Workflow == "sdlc" && last == "review" {
		_, blocked = cascadeStageBlocked(md, last, io.Discard)
	}
	if blocked {
		finishGateBlocked(root, md, last, stderr)
		moePrintf(stderr, "control: finish on %s/%s stopped: %s gate blocked\n", md.Project, md.ID, last)
		return code
	}
	if !oneStage {
		outcome := "the kick ended at " + last + " without shipping"
		settleControl(root, md, r.ID, runcontrol.Settlement{State: runcontrol.StateFailed, Outcome: outcome}, stderr)
		moePrintf(stderr, "control: finish on %s/%s stopped: %s\n", md.Project, md.ID, outcome)
		return code
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "ride: %v\n", err)
		return 1
	}
	next := wf.Successor(last)
	if next == "" {
		return code // nothing left to finish; the next boundary settles it
	}
	fresh, err := run.Load(root, md.Project, md.ID)
	if err != nil {
		moePrintf(stderr, "ride: %v\n", err)
		return 1
	}
	if cc := controlCheckpoint(root, fresh, "stage "+next, stderr); cc != 0 {
		return cc
	}
	return applyFinish(root, fresh, r, "stage "+next, stdout, stderr, finishWalk(root, fresh, next, stdout, stderr))
}
