package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// Workflow is the stage DAG for a run-bearing verb. It owns three
// things: the ordered stage list, the prereq edges, and the
// satisfaction walk (Next). Dispatch lives separately in a paired
// CommandGroup — Workflow holds only stage *names*, never *Command
// pointers, so the DAG is decoupled from the dispatch table. The
// three-part test for whether a verb deserves a Workflow is:
//
//  1. owns canvas documents (one per stage)
//  2. has a stage ladder Next can walk
//  3. shows up in `moe dash`
//
// Verbs that miss any leg of the test (queue, project, session, twin)
// live as plain CommandGroups; they have no Workflow at all.
type Workflow struct {
	Name       string
	stageOrder []string
	prereqs    map[string][]string
	// successors is the inverse of prereqs, computed at RegisterStage
	// time. A stage's successor is any stage that names it as a prereq;
	// stageSatisfied uses this to walk forward (a stage stays "parked"
	// until something downstream commits a fresher turn). The chain
	// prompt asks Successor(stage) for the DAG-level "what's next?"
	// answer, decoupled from Next()'s git-derived satisfaction walk.
	successors map[string][]string
	// stageGates lets a stage layer an additional satisfiability check
	// on top of the default "has a work-turn newer than upstream" rule.
	// Today only sdlc's test stage uses it: a committed test-canvas
	// that left the structural sections empty must not advance the
	// stage. The gate runs *after* the default work-turn check, only
	// when that check passed — so a stage with no work-turn stays
	// parked regardless of gate state.
	stageGates map[string]StageGate
}

// StageGate is the optional canvas-aware check a stage can register
// alongside its work-turn rule. Returning (false, nil) parks the stage;
// returning a non-nil error bubbles up. Receives the bureaucracy root
// and the run metadata so it can read the canvas from disk.
type StageGate func(root string, md *run.Metadata) (bool, error)

// NewWorkflow constructs an empty workflow. Callers add stages with
// RegisterStage and then hand the workflow to RegisterWorkflow.
func NewWorkflow(name string) *Workflow {
	return &Workflow{
		Name:       name,
		prereqs:    map[string][]string{},
		successors: map[string][]string{},
		stageGates: map[string]StageGate{},
	}
}

// RegisterStageGate attaches an additional satisfiability check to
// stage. Panics if stage isn't already registered or if a gate is
// already attached — same fail-loud contract as RegisterStage.
func (w *Workflow) RegisterStageGate(stage string, gate StageGate) {
	found := false
	for _, s := range w.stageOrder {
		if s == stage {
			found = true
			break
		}
	}
	if !found {
		panic("cli: gate registered for unknown stage " + w.Name + " " + stage)
	}
	if _, dup := w.stageGates[stage]; dup {
		panic("cli: duplicate stage gate " + w.Name + " " + stage)
	}
	w.stageGates[stage] = gate
}

// RegisterStage adds a stage name to this workflow's ladder. Panics on
// duplicate names — same contract the dispatcher uses. Optional prereq
// stage names record that this stage's satisfaction depends on those
// stages' latest work turns; the list is consumed by Next,
// upstreamChangeBanner (stage session), and checkStaleness (push).
//
// The stage name is the document id under projects/<p>/runs/<r>/documents/.
// Whether the name is also a typed verb on the workflow's paired
// CommandGroup is a separate question — the workflow doesn't know or
// care. (idea is the example: stage name "idea" lives in the DAG but
// no `moe idea idea` verb is registered.)
func (w *Workflow) RegisterStage(name string, prereqs ...string) {
	for _, s := range w.stageOrder {
		if s == name {
			panic("cli: duplicate stage " + w.Name + " " + name)
		}
	}
	w.stageOrder = append(w.stageOrder, name)
	if len(prereqs) > 0 {
		w.prereqs[name] = append([]string(nil), prereqs...)
		for _, p := range prereqs {
			w.successors[p] = append(w.successors[p], name)
		}
	}
}

// Stages returns the registered stage names in registration order.
func (w *Workflow) Stages() []string {
	out := make([]string, len(w.stageOrder))
	copy(out, w.stageOrder)
	return out
}

// Prereqs returns the prereq stage names for stage, or nil if stage has
// none (or isn't part of this workflow).
func (w *Workflow) Prereqs(stage string) []string {
	return w.prereqs[stage]
}

// Successor returns the stage name that has stage as a prereq, or ""
// for unknown stages and stages with no successor (the terminal stage
// in a linear ladder). Pure DAG lookup — no git involved. The chain
// prompt uses this to ask "what's next after the stage I just
// finished?" without going through Next()'s satisfaction walk: under
// the forward-walking rule Next() reports the just-finished stage as
// parked, which is the wrong answer.
//
// Today every workflow's DAG is linear (one successor per stage), so
// the first registered successor is the right answer; the
// implementation returns "" if none exist. If a future workflow adds a
// fan-out, callers will need to choose between successors — surface
// the design question then rather than inventing one here.
func (w *Workflow) Successor(stage string) string {
	succs := w.successors[stage]
	if len(succs) == 0 {
		return ""
	}
	return succs[0]
}

// NextKind enumerates what Workflow.Next decided about a run's next move.
type NextKind int

const (
	// NextKindStage means the returned name is the next incomplete stage.
	NextKindStage NextKind = iota
	// NextKindDone means every stage is satisfied (or the run is
	// already past its final stage). The returned name is "".
	NextKindDone
)

// Next reports what the run should do next. A stage is "satisfied"
// when it has a work-turn AND either has no successors or some
// successor has a turn newer than its own. The first unsatisfied
// stage is returned with NextKindStage. Once every stage is satisfied,
// Next returns NextKindDone. Terminal statuses (merged, closed)
// short-circuit to NextKindDone regardless of stage state.
// StatusPushed does too — there's no next stage for moe to run, even
// though the run is still "active" in the sense that a human owes the
// PR a click; dash surfaces that distinction separately.
//
// The satisfaction rule walks forward (successors) rather than
// backward (prereqs): a committed turn whose successor has not also
// committed stays "parked" at that stage. This matches the operator's
// intuition that declining the post-stage chain prompt should leave
// the run at the just-finished stage, instead of silently advancing
// to the next one. Re-opens (a fresh design turn after code) still
// flip the run back to the affected stage by the same rule, since
// the stale stage now has a successor whose turn is older.
//
// Returns the stage name, not a *Command — the caller looks up the
// command via LookupGroup(md.Workflow).Lookup(stage) when it needs to
// invoke one.
//
// Next is a shim over NextWithIndex(nil). Callers that already hold a
// *run.JournalIndex (dash builds one per render) thread it through to
// collapse per-stage forks into in-memory lookups; everything else
// keeps the per-call git log shape.
func (w *Workflow) Next(root string, md *run.Metadata) (string, NextKind, error) {
	return w.NextWithIndex(root, md, nil)
}

// NextWithIndex is Next with an optional precomputed journal index.
// A non-nil idx promotes the per-stage LatestWorkTurnSHA fork into a
// WorkTurnTime map lookup — dash with M runs and N stages drops from
// M×N git forks (plus one journal-wide BuildJournalIndex scan) to
// just the one scan. Passing nil restores the per-call fork shape,
// which is the right answer for one-off callers (stage prompt,
// follow, queue walker) that don't keep an index in scope. Same
// satisfaction rule either way: see stageSatisfied.
func (w *Workflow) NextWithIndex(root string, md *run.Metadata, idx *run.JournalIndex) (string, NextKind, error) {
	switch md.Status {
	case run.StatusPushed, run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		return "", NextKindDone, nil
	}
	for _, stage := range w.stageOrder {
		satisfied, err := w.stageSatisfied(root, md, stage, idx)
		if err != nil {
			return "", 0, err
		}
		if !satisfied {
			return stage, NextKindStage, nil
		}
	}
	return "", NextKindDone, nil
}

func (w *Workflow) stageSatisfied(root string, md *run.Metadata, stage string, idx *run.JournalIndex) (bool, error) {
	stageWhen, err := workTurnTime(root, md.Project, md.ID, stage, idx)
	if err != nil {
		return false, err
	}
	if stageWhen.IsZero() {
		return false, nil
	}
	// Stage gates run as an AND with the work-turn rule, but only when
	// a turn has already landed — a gate that read an empty canvas
	// would just report "not filled" for the same reason the work-turn
	// check already would. Layering it on top of the time check keeps
	// the gate honest: it speaks to the *quality* of a committed turn,
	// not its existence.
	if gate, ok := w.stageGates[stage]; ok {
		ok, err := gate(root, md)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	succs := w.successors[stage]
	if len(succs) == 0 {
		return true, nil
	}
	for _, succ := range succs {
		succWhen, err := workTurnTime(root, md.Project, md.ID, succ, idx)
		if err != nil {
			return false, err
		}
		// Forward progression counts when the successor's turn is at
		// least as recent as the stage's. Strict After would flake on
		// same-second commits — a design→code cascade chain lands
		// both turns inside one tick of git's second-resolution
		// committer time, and we want that case satisfied. The old
		// backward-walking rule had the same precision limit on the
		// re-open side (a same-second design re-commit didn't
		// invalidate code), so we're not regressing the re-open
		// detection — that path requires a strictly later timestamp
		// either way, which a human-driven re-open always produces.
		if !succWhen.IsZero() && !succWhen.Before(stageWhen) {
			return true, nil
		}
	}
	return false, nil
}

// workTurnTime returns the committer time of (project, run, doc)'s
// latest work-turn commit, reading from idx when supplied and falling
// back to a per-call LatestWorkTurnSHA fork otherwise. Returns the
// zero time when no work-turn commit exists yet — the same "first
// turn, nothing to diff against" signal LatestWorkTurnSHA produces.
func workTurnTime(root, projectID, runID, docID string, idx *run.JournalIndex) (time.Time, error) {
	if idx != nil {
		return idx.WorkTurnTime[run.WorkTurnKey{Project: projectID, Run: runID, Doc: docID}], nil
	}
	_, when, err := run.LatestWorkTurnSHA(root, projectID, runID, docID)
	return when, err
}

var workflows = map[string]*Workflow{}

// RegisterWorkflow adds w to the workflow registry. Panics on duplicates.
func RegisterWorkflow(w *Workflow) {
	if _, dup := workflows[w.Name]; dup {
		panic("cli: duplicate workflow " + w.Name)
	}
	workflows[w.Name] = w
}

// LookupWorkflow returns the registered workflow named name. An unknown
// or empty name returns a listing of what is registered so the operator
// can see the typo; Metadata.Workflow is required on disk, so an empty
// name reaching here is a bug upstream rather than a normal default.
func LookupWorkflow(name string) (*Workflow, error) {
	w, ok := workflows[name]
	if !ok {
		return nil, fmt.Errorf("workflow %q not registered (known: %v)", name, WorkflowNames())
	}
	return w, nil
}

// WorkflowNames returns the registered workflow names, sorted.
func WorkflowNames() []string {
	names := make([]string, 0, len(workflows))
	for n := range workflows {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
