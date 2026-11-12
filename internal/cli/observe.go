package cli

// The one read seam behind every status surface — `moe dash`, `moe
// control show`, serve's dash and run page, and the Rex status the dash
// reports. It overlays three observations on a run the journal gatherer
// already found: the durable control record, the run guard, and the
// owner's live endpoint. Each source degrades on its own: a slow or
// missing endpoint costs one row its live phase, never the board, and an
// unreachable owner never reads as idle or as permission to replace it.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
)

// Probe bounds. A refresh never waits longer than observeBudget for all
// endpoints together, nor observeProbeTimeout for any one.
var (
	observeProbeTimeout = 500 * time.Millisecond
	observeBudget       = 1500 * time.Millisecond
	observeParallel     = 8
)

// RunObservation is what the overlay knows about one run right now.
type RunObservation struct {
	Project, Run string
	Workflow     string
	Status       string
	// Control is the durable record; ControlErr means it could not be
	// read, which refuses motion and must never render as "no hold".
	Control    runcontrol.File
	ControlErr error
	// Unrecorded are final receipts an owner knew but could not commit,
	// by request id (runcontrol.KeepUnrecorded). They explain what
	// happened to a request the record still reads as open; they never
	// change what the record grants. UnrecordedErr means the local store
	// could not be read.
	Unrecorded    map[string]runcontrol.Settlement
	UnrecordedErr error
	// Owned is the run guard; OwnerErr means the probe itself failed.
	Owned    bool
	OwnerErr error
	// Live is the owner's snapshot when its endpoint answered. LiveErr is
	// why it did not: runsock.ErrNoOwner (nothing listening) or a contact
	// failure. Both nil means it was not probed (no owner).
	Live       *runsock.Status
	LiveErr    error
	ObservedAt time.Time
}

func (o RunObservation) key() string { return o.Project + "/" + o.Run }

// ObserveRuns observes the given runs, probing endpoints only for runs
// whose guard is held, with bounded concurrency and an overall budget.
// Callers pass runs already narrowed by their own filters.
func ObserveRuns(root string, mds []*run.Metadata, now time.Time) map[string]RunObservation {
	out := make(map[string]RunObservation, len(mds))
	var probe []*run.Metadata
	for _, md := range mds {
		o := RunObservation{Project: md.Project, Run: md.ID, Workflow: md.Workflow, Status: md.Status, ObservedAt: now}
		o.Control, o.ControlErr = runcontrol.Load(root, md.Project, md.ID)
		o.Unrecorded, o.UnrecordedErr = runcontrol.Unrecorded(root, md.Project, md.ID)
		o.Owned, o.OwnerErr = runowner.Busy(root, md.Project, md.ID)
		out[o.key()] = o
		if o.Owned {
			probe = append(probe, md)
		}
	}
	if len(probe) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), observeBudget)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, observeParallel)
	for _, md := range probe {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				o := out[md.Project+"/"+md.ID]
				o.LiveErr = ctx.Err()
				out[o.key()] = o
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, observeProbeTimeout)
			defer pcancel()
			st, err := runsock.ReadStatus(pctx, runsock.RuntimeDir(), runsock.Identity{Root: root, Project: md.Project, Run: md.ID})
			mu.Lock()
			defer mu.Unlock()
			o := out[md.Project+"/"+md.ID]
			if err != nil {
				o.LiveErr = err
			} else {
				o.Live = &st
			}
			out[o.key()] = o
		}()
	}
	wg.Wait()
	return out
}

// Summary is the concise row text: owner, phase, hold and request, in
// that order, or "" when there is nothing beyond the journal row to say.
func (o RunObservation) Summary(now time.Time) string {
	var parts []string
	if o.ControlErr != nil {
		parts = append(parts, "control record unreadable")
	}
	if o.UnrecordedErr != nil {
		parts = append(parts, "local receipts unreadable")
	}
	f := o.Control
	if f.Recovery != nil {
		parts = append(parts, "recovery required")
	}
	if f.Hold != nil {
		what := "held"
		if f.Hold.Kind == runcontrol.KindHandoff {
			what = "handed off"
		}
		// A hold whose settling turn ended reads as held, whether or not
		// its receipt made the journal.
		if r, ok := f.Find(f.Hold.Request); ok && !r.Settled() && o.Owned {
			if _, owed := o.owedReceipt(r); owed {
				what += " · receipt not recorded"
			} else if !o.outcomeUnknown(r) {
				what = string(f.Hold.Kind) + " pending"
			}
		}
		parts = append(parts, what)
	}
	if open := f.Open(runcontrol.KindFinish); len(open) > 0 {
		pending := "finish pending"
		if open[0].State == runcontrol.StateApplied {
			pending = "finishing"
		}
		parts = append(parts, o.openPhrase(open[0], pending))
	}
	if open := f.Open(runcontrol.KindPark); len(open) > 0 {
		parts = append(parts, o.openPhrase(open[0], "park pending"))
	}
	switch {
	case o.OwnerErr != nil:
		parts = append(parts, "owner unknown")
	case o.Live != nil:
		parts = append(parts, livePhrase(*o.Live, now))
	case o.Owned && errors.Is(o.LiveErr, runsock.ErrNoOwner):
		parts = append(parts, "owned · no live status")
	case o.Owned:
		parts = append(parts, "owner unreachable")
	}
	return strings.Join(parts, " · ")
}

// owedReceipt is the final receipt an owner kept locally for r because
// it could not commit it, while the record still carries r unsettled. A
// final receipt in the record wins over any copy left behind.
func (o RunObservation) owedReceipt(r runcontrol.Request) (runcontrol.Settlement, bool) {
	s, ok := o.Unrecorded[r.ID]
	if !ok || r.Settled() || !finalState(s.State) {
		return runcontrol.Settlement{}, false
	}
	return s, true
}

// outcomeUnknown reports that local evidence about an unsettled r cannot
// be read or makes no sense, so nothing may call it pending: an owner
// may already have ended it.
func (o RunObservation) outcomeUnknown(r runcontrol.Request) bool {
	if r.Settled() {
		return false
	}
	s, ok := o.Unrecorded[r.ID]
	return o.UnrecordedErr != nil || (ok && !finalState(s.State))
}

func finalState(s runcontrol.State) bool {
	return s == runcontrol.StateCompleted || s == runcontrol.StateFailed
}

// openPhrase is the row word for an unsettled request: pending as the
// record has it, or the outcome an owner kept locally.
func (o RunObservation) openPhrase(r runcontrol.Request, pending string) string {
	if s, ok := o.owedReceipt(r); ok {
		return string(r.Kind) + " " + string(s.State) + " · receipt not recorded"
	}
	if o.outcomeUnknown(r) {
		return string(r.Kind) + " outcome unknown"
	}
	return pending
}

// describe is describeRequest with local evidence overlaid: an owed
// outcome reads as what happened, labelled unrecorded, with the error
// that kept it out of the journal when one was saved.
func (o RunObservation) describe(r runcontrol.Request) string {
	if s, ok := o.owedReceipt(r); ok {
		r.State, r.Boundary, r.Outcome = s.State, s.Boundary, s.Outcome
		d := describeRequest(r) + " — receipt not recorded"
		if s.RecordingError != "" {
			// One line: a git error quotes the whole commit message.
			d += " (recording failed when kept: " + strings.Join(strings.Fields(s.RecordingError), " ") + ")"
		}
		return d
	}
	if o.outcomeUnknown(r) {
		why := "local receipts unreadable"
		if o.UnrecordedErr == nil {
			why = "its local receipt is invalid"
		}
		return string(r.State) + " in the record; outcome unknown — " + why
	}
	return describeRequest(r)
}

// localLines are the detail lines about local receipts: the store's
// read error, local evidence that matches nothing, and — once — what
// happens to an owed receipt.
func (o RunObservation) localLines() []string {
	if o.UnrecordedErr != nil {
		return []string{"local receipts: unreadable — " + o.UnrecordedErr.Error()}
	}
	var out []string
	owed := false
	for _, id := range slices.Sorted(maps.Keys(o.Unrecorded)) {
		s := o.Unrecorded[id]
		r, ok := o.Control.Find(id)
		switch {
		case !ok:
			out = append(out, "local receipt "+id+": names no recorded request; ignored")
		case r.Settled():
			// The record has it; the copy is spent.
		case !finalState(s.State):
			out = append(out, fmt.Sprintf("local receipt %s: %q is not a final outcome; ignored", id, s.State))
		default:
			owed = true
		}
	}
	if owed {
		out = append(out, "receipt not recorded: the next owner of this run records it before starting work")
	}
	return out
}

func livePhrase(st runsock.Status, now time.Time) string {
	if st.Phase == runsock.PhaseDeciding {
		return "waiting for operator"
	}
	s := st.Phase
	if st.Stage != "" && st.Phase != runsock.PhaseShipping {
		s += " " + st.Stage
	}
	if !st.PhaseSince.IsZero() {
		s += " " + dash.HumanAgo(now, st.PhaseSince)
	}
	return s
}

// Details is the long form `control show` and the run page print: one
// line per fact, each labelled with its source.
func (o RunObservation) Details(now time.Time) []string {
	var out []string
	switch {
	case o.OwnerErr != nil:
		out = append(out, "owner: unknown ("+o.OwnerErr.Error()+")")
	case !o.Owned:
		out = append(out, "owner: none")
	case o.Live != nil:
		st := *o.Live
		mode := "headless"
		if st.Interactive {
			mode = "attended"
		}
		out = append(out, fmt.Sprintf("owner: pid %d, generation %s, %s", st.OwnerPID, st.Generation, mode))
		phase := "phase: " + st.Phase
		if st.Stage != "" {
			phase += " (stage " + st.Stage + ")"
		}
		phase += " since " + dash.HumanAgo(now, st.PhaseSince)
		out = append(out, phase)
		if st.Waiting != "" {
			out = append(out, "waiting: "+st.Waiting)
		}
		if st.Continuation != "" {
			out = append(out, "continuation: "+st.Continuation)
		}
		if st.Activity.IsZero() {
			out = append(out, "agent activity: unknown")
		} else {
			out = append(out, "agent activity: "+dash.HumanAgo(now, st.Activity))
		}
		origin := "origin: " + st.Origin.Host
		if st.Origin.Sweep != "" {
			origin += ", sweep " + st.Origin.Sweep
		}
		if st.Origin.ServeLaunch != "" {
			origin += ", launched by serve " + st.Origin.ServeLaunch
		}
		out = append(out, origin)
		if st.Terminal != nil {
			out = append(out, "rex: session "+st.Terminal.Session+", block "+st.Terminal.Block)
		}
	case errors.Is(o.LiveErr, runsock.ErrNoOwner):
		out = append(out, "owner: a moe invocation holds the run but serves no live status")
	default:
		out = append(out, fmt.Sprintf("owner: held by a moe invocation; live status unavailable (%v)", o.LiveErr))
	}
	out = append(out, "observed: "+o.ObservedAt.Local().Format("15:04:05"))
	return out
}

// observeRows fills each active row's Control text from one observation
// pass over the live runs behind those rows.
func observeRows(root string, rows []dash.Row, mds []*run.Metadata, now time.Time) map[string]RunObservation {
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	var live []*run.Metadata
	seen := map[string]bool{}
	for _, r := range rows {
		key := r.Project + "/" + r.Run
		md := byKey[key]
		if md == nil || seen[key] || (md.Status != run.StatusInProgress && md.Status != run.StatusPushed) {
			continue
		}
		seen[key] = true
		live = append(live, md)
	}
	obs := ObserveRuns(root, live, now)
	for i := range rows {
		if o, ok := obs[rows[i].Project+"/"+rows[i].Run]; ok {
			rows[i].Control = o.Summary(now)
		}
	}
	return obs
}
