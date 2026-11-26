package serve

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/modulecollective/moe/internal/servepause"
)

type pauseVM struct {
	Summary  string
	Revision int
	Active   bool
	Error    string
}

func pauseView(root string, now time.Time) pauseVM {
	state, err := servepause.Read(root)
	if err != nil {
		return pauseVM{Summary: "automatic sweep pause unavailable", Error: err.Error()}
	}
	return pauseVM{Summary: state.Summary(now), Revision: state.Revision, Active: state.Active(now)}
}

func (s *Server) pausePanel(now time.Time) servePanelVM {
	panel := s.activity.panel(now)
	panel.Pause = pauseView(s.opts.Root, now)
	if panel.Pause.Error != "" {
		panel.Cluster += " · automatic sweep pause unavailable"
	} else if panel.Pause.Active {
		panel.Cluster += " · " + panel.Pause.Summary
	}
	return panel
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	revision, err := strconv.Atoi(r.FormValue("revision"))
	if err != nil || revision < 0 {
		http.Error(w, "serve pause: revision is required", http.StatusBadRequest)
		return
	}
	_, err = servepause.Set(s.opts.Root, r.URL.Path == "/serve/pause", &revision, time.Now)
	if errors.Is(err, servepause.ErrStale) {
		view := pauseView(s.opts.Root, time.Now())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		s.render(w, r, "pause_conflict.html", struct {
			Pause pauseVM
			Error string
		}{view, err.Error()})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/serve", http.StatusSeeOther)
}
