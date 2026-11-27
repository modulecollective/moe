package serve

import (
	"errors"
	"net/http"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
)

// pulseReadShown is how many read notes the dash's PULSE section keeps
// under the pending ones. /input has the whole history.
const pulseReadShown = 3

// pulseVM is the dash's PULSE section beyond the serve cluster: the last
// pulse run, and the operator's notes to the survey.
type pulseVM struct {
	// LastRun is the newest pulse run's "<project>/<run>", empty before
	// the first; LastAgo is how long since it last moved.
	LastRun string
	LastAgo string
	// Waiting says what a pending note waits on: the countdown, a pause,
	// or no armed serve.
	Waiting string
	Quota   string
	Pending []pulseNoteVM
	Read    []pulseNoteVM
}

// pulseNoteVM is one note. ReadBy is the qualified pulse run that read
// it, empty while pending.
type pulseNoteVM struct {
	ID     int
	Text   string
	ReadBy string
}

func newPulseNoteVMs(entries []input.Entry) []pulseNoteVM {
	out := make([]pulseNoteVM, 0, len(entries))
	for _, e := range entries {
		out = append(out, pulseNoteVM{ID: e.ID, Text: e.Text, ReadBy: e.DeliveredTo})
	}
	return out
}

// pulseView assembles the section from the board's rows, the serve panel
// (for the countdown and pause) and the pulse-notes record. A malformed
// record is logged and shows no notes; the section still renders.
func (s *Server) pulseView(now time.Time, rows []dash.Row, panel servePanelVM) pulseVM {
	var vm pulseVM
	if last := dash.LastPulse(rows); last != nil {
		vm.LastRun, vm.LastAgo = last.Project+"/"+last.Run, dash.HumanAgo(now, last.When)
	}
	pause := ""
	if panel.Pause.Active {
		pause = panel.Pause.Summary
	}
	if s.opts.SurveyAdmission != nil {
		vm.Quota = s.opts.SurveyAdmission(now).Summary(now)
	}
	for _, p := range s.activity.snapshot(now).Projects {
		if p.Sweeping() {
			vm.Quota = "survey running"
			break
		}
	}
	vm.Waiting = dash.PulseWaiting(panel.Armed, panel.NextSweep, pause, vm.Quota)
	f, err := input.LoadPulse(s.opts.Root)
	if err != nil {
		s.logf("pulse notes: %v", err)
	}
	pending, read := dash.SplitPulseNotes(f)
	vm.Pending = newPulseNoteVMs(pending)
	vm.Read = newPulseNoteVMs(read[:min(len(read), pulseReadShown)])
	return vm
}

// handlePulseNote takes the operator's note to the pulse from the dash's
// composer: one journal commit, no agent, then back to the dash. The
// commit is what wakes the heartbeat.
func (s *Server) handlePulseNote(w http.ResponseWriter, r *http.Request) {
	e, err := input.AddPulse(s.opts.Root, r.FormValue("text"))
	if errors.Is(err, input.ErrEmpty) {
		http.Error(w, "pulse note: nothing to send", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.logf("pulse note: %v", err)
		http.Error(w, "pulse note: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logf("pulse note #%d saved", e.ID)
	http.Redirect(w, r, "/#pulse", http.StatusSeeOther)
}
