package cli

// serve's doors to run controls: the run page's status block, the dash's
// compact row status, and the control form. Every one reads through
// ObserveRuns and writes through the same writer `moe control` uses.

import (
	"errors"
	"fmt"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/serve"
)

// serveRunControl builds a run's status block for the web run page.
func serveRunControl(root, projectID, runID string) (serve.RunControlView, bool, error) {
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return serve.RunControlView{}, false, err
	}
	if controlPolicy(md) != nil || (md.Status != run.StatusInProgress && md.Status != run.StatusPushed) {
		return serve.RunControlView{}, false, nil
	}
	now := time.Now()
	o := ObserveRuns(root, []*run.Metadata{md}, now)[projectID+"/"+runID]
	v := serve.RunControlView{Summary: o.Summary(now), Lines: o.Details(now), Revision: o.Control.Revision}
	if o.ControlErr != nil {
		v.Lines = append(v.Lines, "control record unreadable: "+o.ControlErr.Error())
		return v, true, nil
	}
	f := o.Control
	stage, agent := conversationAddress(md)
	if f.Hold != nil {
		r, _ := f.Find(f.Hold.Request)
		v.Lines = append(v.Lines, fmt.Sprintf("hold: %s %s — %s", f.Hold.Kind, f.Hold.Request, holdPhase(r)))
		if f.Hold.Stage != "" {
			stage, agent = f.Hold.Stage, f.Hold.Agent
		}
	}
	if f.Recovery != nil {
		v.Lines = append(v.Lines, "recovery required: "+f.Recovery.Reason)
	}
	v.Conversation = conversationCommand(md, stage, agent)
	v.Actions = controlActions(md, o)
	for _, r := range f.Requests {
		v.Requests = append(v.Requests, fmt.Sprintf("%s %s %s — %s", r.AcceptedAt.Local().Format("Jan 2 15:04"), r.Kind, r.ID, describeRequest(r)))
	}
	return v, true, nil
}

// controlActions are the controls valid on a run right now. A held or
// recovering run offers only resume; otherwise pause and handoff always,
// park only with an owner to yield, finish only where it could ship.
func controlActions(md *run.Metadata, o RunObservation) []string {
	if o.ControlErr != nil {
		return nil
	}
	f := o.Control
	if f.Hold != nil || f.Recovery != nil {
		return []string{string(runcontrol.KindResume)}
	}
	out := []string{string(runcontrol.KindPause), string(runcontrol.KindHandoff)}
	if o.Owned {
		out = append(out, string(runcontrol.KindPark))
	}
	if len(f.Open(runcontrol.KindFinish)) == 0 && (md.Status == run.StatusInProgress || o.Owned) {
		out = append(out, string(runcontrol.KindFinish))
	}
	return out
}

// serveRunStatuses is the dash's compact row status: one bounded
// observation pass over the live runs, optionally in one project.
func serveRunStatuses(root, projectID string) (map[string]string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var live []*run.Metadata
	for _, md := range mds {
		if (projectID == "" || md.Project == projectID) &&
			(md.Status == run.StatusInProgress || md.Status == run.StatusPushed) {
			live = append(live, md)
		}
	}
	now := time.Now()
	out := map[string]string{}
	for key, o := range ObserveRuns(root, live, now) {
		if s := o.Summary(now); s != "" {
			out[key] = s
		}
	}
	return out, nil
}

// serveSubmitControl records a control from the web. Resume takes the run
// guard directly — serve is a long-lived process whose writers never
// borrow — and releases the hold for a later eligible sweep; it starts
// nothing. A revision from a stale page cannot clear a newer hold.
func serveSubmitControl(root, projectID, runID, action, id string, revision int) (string, error) {
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return "", err
	}
	if err := controlPolicy(md); err != nil {
		return "", fmt.Errorf("%w: %v", serve.ErrControlRefused, err)
	}
	kind := runcontrol.Kind(action)
	if kind == runcontrol.KindResume {
		h, err := runowner.Acquire(root, projectID, runID)
		if err != nil {
			return "", fmt.Errorf("%w: %v", serve.ErrControlRefused, err)
		}
		defer h.Release()
		r, released, err := runcontrol.Resume(root, projectID, runID, h, runcontrol.Request{ID: id, Revision: revision})
		if err != nil {
			return "", refusedOr(err)
		}
		if !released && r.ID == "" {
			return "not held", nil
		}
		return r.Outcome, nil
	}
	req := runcontrol.Request{ID: id, Kind: kind}
	if kind == runcontrol.KindHandoff {
		req.Stage, req.Agent = conversationAddress(md)
	}
	r, note, err := deliverControl(root, md, req)
	if err != nil {
		return "", refusedOr(err)
	}
	if note != "" {
		return note, nil
	}
	return describeRequest(r), nil
}

// refusedOr marks the record's own refusals as such; anything else is a
// failure to report.
func refusedOr(err error) error {
	for _, target := range []error{runcontrol.ErrBadKind, runcontrol.ErrConflict, runcontrol.ErrHeld,
		runcontrol.ErrRecovery, runcontrol.ErrNoOwner, runcontrol.ErrNotLive, runcontrol.ErrStale} {
		if errors.Is(err, target) {
			return fmt.Errorf("%w: %v", serve.ErrControlRefused, err)
		}
	}
	if _, refused := errors.AsType[*runsock.RefusedError](err); refused {
		return fmt.Errorf("%w: %v", serve.ErrControlRefused, err)
	}
	return err
}
