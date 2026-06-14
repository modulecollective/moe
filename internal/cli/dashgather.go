package cli

import (
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// DashSnapshot is everything a dash renderer needs in one shot:
// pre-built rows, the project count footer, and the active-project
// count derived from the rows themselves.
type DashSnapshot struct {
	Rows           []dash.Row
	ProjectCount   int
	ActiveProjects int
}

// DashFilter mirrors the `moe dash` flag set so the gatherer can be
// reached identically from the CLI and from `moe serve`.
type DashFilter struct {
	ProjectFilter  string
	WorkflowFilter string
}

// GatherDashSnapshot is the data-assembly pass behind every dash
// render (CLI or HTTP). It scans runs, builds the journal index,
// resolves per-run next-stage decisions through LookupWorkflow, and
// hands the result to dash.BuildRows.
//
// Lives in package cli because it depends on the workflow registry
// (`LookupWorkflow` / `NextKindDone`) and `internal/dash` deliberately
// stays free of that plumbing.
func GatherDashSnapshot(root string, now time.Time, filter DashFilter) (DashSnapshot, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return DashSnapshot{}, err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return DashSnapshot{}, err
	}

	// Open-session liveness is best-effort: a session.List failure
	// silently yields no markers, same as the CLI dash handler.
	sessionDocsByRun := make(map[string][]string)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionDocsByRun[s.Run] = append(sessionDocsByRun[s.Run], s.Doc)
		}
	}

	nextByRun := make(map[string]dash.NextDecision, len(mds))
	for _, md := range mds {
		if md.Workflow == dash.IdeaWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		wf, err := LookupWorkflow(md.Workflow)
		if err != nil {
			return DashSnapshot{}, err
		}
		next, kind, err := wf.NextWithIndex(root, md, idx)
		if err != nil {
			return DashSnapshot{}, err
		}
		dec := dash.NextDecision{
			Done:      kind == NextKindDone,
			Perpetual: wf.Perpetual(),
		}
		if next != "" {
			dec.Stage = next
		} else if dec.Done && dec.Perpetual {
			stages := wf.Stages()
			if len(stages) > 0 {
				dec.Stage = stages[len(stages)-1]
			}
		}
		nextByRun[md.ID] = dec
	}
	var choreInputs []dash.ChoreInput
	if filter.WorkflowFilter == "" {
		defs, err := chore.LoadAll(root)
		if err != nil {
			return DashSnapshot{}, err
		}
		for _, s := range chore.EvaluateAll(defs, mds, idx, now) {
			if !s.Due {
				continue
			}
			when := s.LastCompleted
			if touched := s.Definition.EditedAt; touched.After(when) {
				when = touched
			}
			choreInputs = append(choreInputs, dash.ChoreInput{
				Project: s.Definition.Project,
				Name:    s.Definition.Name,
				Reason:  s.ReasonString(),
				When:    when,
			})
		}
	}

	rows, err := dash.BuildRows(dash.Inputs{
		Now:              now,
		ProjectFilter:    filter.ProjectFilter,
		WorkflowFilter:   filter.WorkflowFilter,
		Runs:             mds,
		Index:            idx,
		SessionDocsByRun: sessionDocsByRun,
		NextByRun:        nextByRun,
		Chores:           choreInputs,
	})
	if err != nil {
		return DashSnapshot{}, err
	}

	projectCount, err := dash.CountProjects(root)
	if err != nil {
		return DashSnapshot{}, err
	}

	activeProjects := map[string]struct{}{}
	for _, r := range rows {
		if r.Bucket == dash.BucketActiveRuns {
			activeProjects[r.Project] = struct{}{}
		}
	}

	return DashSnapshot{
		Rows:           rows,
		ProjectCount:   projectCount,
		ActiveProjects: len(activeProjects),
	}, nil
}

// GatherRunRow returns the dash.Row for a single run, computed the same
// way GatherDashSnapshot computes rows for the dash. ok is false when
// the run is filtered out (e.g. classified into BucketNone) or doesn't
// exist on disk.
//
// Implementation reuses GatherDashSnapshot with a ProjectFilter so the
// classify logic stays in one place. One extra single-project scan per
// detail-page hit, which is cheap on a single-operator localhost
// server.
func GatherRunRow(root string, projectID, slug string, now time.Time) (dash.Row, bool, error) {
	snap, err := GatherDashSnapshot(root, now, DashFilter{
		ProjectFilter: projectID,
	})
	if err != nil {
		return dash.Row{}, false, err
	}
	for _, r := range snap.Rows {
		if r.Run == slug {
			return r, true, nil
		}
	}
	return dash.Row{}, false, nil
}
