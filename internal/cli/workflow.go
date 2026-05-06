package cli

import (
	"fmt"
	"sort"

	"github.com/modulecollective/moe/internal/run"
)

// NextKind enumerates what Workflow.Next decided about a run's next move.
type NextKind int

const (
	// NextKindStage means the returned Command is the next incomplete stage.
	NextKindStage NextKind = iota
	// NextKindDone means every stage is satisfied (or the run is
	// already past its final stage). The returned Command is nil.
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
func (w *Workflow) Next(root string, md *run.Metadata) (*Command, NextKind, error) {
	switch md.Status {
	case run.StatusPushed, run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		return nil, NextKindDone, nil
	}
	for _, stage := range w.stageOrder {
		satisfied, err := w.stageSatisfied(root, md, stage)
		if err != nil {
			return nil, 0, err
		}
		if !satisfied {
			return w.commands[stage], NextKindStage, nil
		}
	}
	return nil, NextKindDone, nil
}

func (w *Workflow) stageSatisfied(root string, md *run.Metadata, stage string) (bool, error) {
	_, stageWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, stage)
	if err != nil {
		return false, err
	}
	if stageWhen.IsZero() {
		return false, nil
	}
	succs := w.successors[stage]
	if len(succs) == 0 {
		return true, nil
	}
	for _, succ := range succs {
		_, succWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, succ)
		if err != nil {
			return false, err
		}
		// Forward progression counts when the successor's turn is at
		// least as recent as the stage's. Strict After would flake on
		// same-second commits — the design→code one-shot chain lands
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
