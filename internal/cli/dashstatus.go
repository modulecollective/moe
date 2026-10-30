package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
)

// dashStatus owns only this watch's subtree. A successful write is the
// comparison checkpoint; a failed write is retried on the next refresh.
type dashStatus struct {
	w       io.Writer
	started bool
	last    map[string]cliout.ProgramStatus
}

func (s *dashStatus) update(reports []cliout.ProgramStatus) error {
	if s.w == nil {
		return nil
	}
	if !s.started {
		if _, err := (cliout.ProgramStatus{ID: "moe/dash", State: "clear"}).WriteTo(s.w); err != nil {
			return err
		}
		s.started = true
		s.last = make(map[string]cliout.ProgramStatus)
	}
	want := make(map[string]cliout.ProgramStatus, len(reports))
	for _, r := range reports {
		want[r.ID] = r.Normalized()
	}
	// Fixed order also makes clearing and reseeding deterministic.
	for _, id := range []string{"moe/dash/activity", "moe/dash/questions", "moe/dash/health"} {
		r, present := want[id]
		old, existed := s.last[id]
		if !present && !existed || present && existed && old == r {
			continue
		}
		if !present {
			r = cliout.ProgramStatus{ID: id, State: "clear"}
		}
		if _, err := r.WriteTo(s.w); err != nil {
			return err
		}
		if present {
			s.last[id] = r
		} else {
			delete(s.last, id)
		}
	}
	return nil
}

func (s *dashStatus) close() {
	if s.w != nil && s.started {
		_, _ = (cliout.ProgramStatus{ID: "moe/dash", State: "clear"}).WriteTo(s.w)
	}
}

// dashStatusReports projects observations, never row prose or inferred agent
// thinking. Serve health is deliberately global; activity and questions follow
// the board filters. No timestamps or elapsed ages enter the comparison.
func dashStatusReports(snap DashSnapshot, gatherErr error, st serveState, filter DashFilter) []cliout.ProgramStatus {
	var reports []cliout.ProgramStatus
	add := func(slot, state, kind, title, message string) {
		reports = append(reports, cliout.ProgramStatus{
			ID: "moe/dash/" + slot, State: state, Kind: kind, App: "moe", Title: title, Message: message,
		})
	}
	scope := "MoE fleet"
	if filter.ProjectFilter != "" {
		scope += " / " + filter.ProjectFilter
	}
	if filter.WorkflowFilter != "" {
		scope += " / " + filter.WorkflowFilter
	}
	var health []string
	if gatherErr != nil {
		health = append(health, "Dashboard observation failed: "+gatherErr.Error())
	}
	for _, err := range snap.InputErrors {
		health = append(health, "Question observation failed: "+err.Error())
	}
	if snap.SessionError != nil {
		health = append(health, "Session observation failed: "+snap.SessionError.Error())
	}
	serveKnown := true
	switch {
	case st.err != nil:
		health = append(health, "Serve-wide observation failed: "+st.err.Error())
		serveKnown = false
	case st.ok && st.live == serveUnknown:
		health = append(health, "Serve-wide: process liveness unknown")
		serveKnown = false
	case st.ok && st.live == serveDead:
		health = append(health, "Serve-wide: serve process is dead")
		serveKnown = false
	}
	var sweeping []string
	if st.ok && st.live == serveAlive && st.err == nil {
		for _, p := range st.snap.Projects {
			if p.Sweeping() {
				if (filter.ProjectFilter == "" || p.Project == filter.ProjectFilter) &&
					(filter.WorkflowFilter == "" || filter.WorkflowFilter == "pulse") {
					sweeping = append(sweeping, p.Project)
				}
			} else if p.Cooling() || p.Failed() {
				health = append(health, "Serve-wide: last sweep failed for "+p.Project)
			}
		}
	}
	if len(sweeping) > 0 {
		slices.Sort(sweeping)
		add("activity", "working", "", scope, "Live sweeps: "+strings.Join(sweeping, ", "))
	} else if serveKnown && gatherErr == nil && snap.SessionError == nil {
		sessions := make(map[string]bool)
		for _, row := range snap.Rows {
			if row.RunningDoc != "" {
				sessions[row.Project+"/"+row.Run] = true
			}
		}
		message := "No live sweeps or open sessions observed"
		if len(sessions) > 0 {
			message = fmt.Sprintf("%d open run sessions; agent activity unknown", len(sessions))
		}
		add("activity", "idle", "", scope, message)
	}
	if gatherErr == nil && len(snap.Questions) > 0 {
		keys := make([]string, 0, len(snap.Questions))
		for key := range snap.Questions {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		questions := make([]string, 0, len(keys))
		for _, key := range keys {
			questions = append(questions, key+": "+snap.Questions[key])
		}
		add("questions", "blocked", "question", scope+" / operator questions",
			fmt.Sprintf("%d outstanding operator questions: %s", len(keys), strings.Join(questions, "; ")))
	}
	if len(health) > 0 {
		slices.Sort(health)
		add("health", "error", "", "MoE observations / serve-wide health", strings.Join(health, "; "))
	}
	return reports
}
