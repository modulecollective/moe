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
// when its most recent work turn is newer than every prereq's most
// recent work turn. The first unsatisfied stage is returned with
// NextKindStage. Once every stage is satisfied, Next returns
// NextKindDone. Terminal statuses (merged, closed) short-circuit to
// NextKindDone regardless of stage state. StatusPushed does too —
// there's no next stage for moe to run, even though the run is still
// "active" in the sense that a human owes the PR a click; dash
// surfaces that distinction separately.
func (w *Workflow) Next(root string, md *run.Metadata) (*Command, NextKind, error) {
	switch md.Status {
	case run.StatusPushed, run.StatusMerged, run.StatusClosed:
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
	for _, dep := range w.prereqs[stage] {
		_, depWhen, err := run.LatestWorkTurnSHA(root, md.Project, md.ID, dep)
		if err != nil {
			return false, err
		}
		if !depWhen.IsZero() && depWhen.After(stageWhen) {
			return false, nil
		}
	}
	return true, nil
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
