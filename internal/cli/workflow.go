package cli

import (
	"fmt"
	"sort"

	"github.com/modulecollective/moe/internal/request"
)

// NextKind enumerates what Workflow.Next decided about a request's next move.
type NextKind int

const (
	// NextKindStage means the returned Command is the next incomplete stage.
	NextKindStage NextKind = iota
	// NextKindDone means every stage is satisfied (or the request is
	// already past its final stage). The returned Command is nil.
	NextKindDone
)

// Next reports what the request should do next. A stage is "satisfied"
// when its most recent work turn is newer than every prereq's most
// recent work turn. The first unsatisfied stage is returned with
// NextKindStage. Once every stage is satisfied, Next returns
// NextKindDone. A request in StatusPushed short-circuits to
// NextKindDone regardless of stage state.
func (w *Workflow) Next(root string, md *request.Metadata) (*Command, NextKind, error) {
	if md.Status == request.StatusPushed {
		return nil, NextKindDone, nil
	}
	for _, stage := range w.order {
		satisfied, err := w.stageSatisfied(root, md, stage)
		if err != nil {
			return nil, 0, err
		}
		if !satisfied {
			return w.stages[stage], NextKindStage, nil
		}
	}
	return nil, NextKindDone, nil
}

func (w *Workflow) stageSatisfied(root string, md *request.Metadata, stage string) (bool, error) {
	_, stageWhen, err := request.LatestWorkTurnSHA(root, md.ID, stage)
	if err != nil {
		return false, err
	}
	if stageWhen.IsZero() {
		return false, nil
	}
	for _, dep := range w.prereqs[stage] {
		_, depWhen, err := request.LatestWorkTurnSHA(root, md.ID, dep)
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
