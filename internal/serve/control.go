package serve

// The run page's live status and control forms, and the dash's compact
// row status. The data comes from the CLI's observation seam through
// Options callbacks; serve only renders it and relays submissions.
//
// Read routes write nothing. The control route records a request or
// releases a hold — it never launches an agent; a released run waits
// for an eligible sweep, and the conversation command is printed for
// the operator to run at a terminal. Forms work without JavaScript and
// POST redirects to a fresh readback; the script only re-fetches the
// status and control slots while the page is visible.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RunControlView is one run's status block.
type RunControlView struct {
	// Summary is the same concise text the dash row shows.
	Summary string
	// Lines are labelled facts: owner, phase, waiting, origin, hold.
	Lines []string
	// Revision is the hold revision a resume form carries, so a stale
	// form cannot clear a newer hold.
	Revision int
	// Actions are the controls valid right now: pause, handoff, park,
	// finish, resume. Impossible ones are absent, and the server refuses
	// them anyway.
	Actions []string
	// Conversation is the stage command that resumes the conversation at
	// a terminal, when there is one.
	Conversation string
	// Requests are the control receipts, one line each, newest last.
	Requests []string
}

// ErrControlRefused marks a control the run refused: stale, held,
// unsupported, or otherwise not valid now. Rendered as a 409.
var ErrControlRefused = errors.New("control refused")

// runStatusVM is the status block's template input.
type runStatusVM struct {
	Control    *RunControlView
	StatusURL  string
	ControlURL string
	// ControlID is a fresh request id per render: a resubmitted form
	// reads back its first outcome instead of acting twice.
	ControlID string
}

func (s *Server) runStatusVM(projectID, slug string) runStatusVM {
	base := "/run/" + projectID + "/" + slug
	vm := runStatusVM{StatusURL: base + "/status", ControlURL: base + "/control"}
	if s.opts.RunControl == nil {
		return vm
	}
	view, ok, err := s.opts.RunControl(projectID, slug)
	if err != nil {
		s.logf("run status %s/%s: %v", projectID, slug, err)
		view, ok = RunControlView{Lines: []string{"status unavailable: " + err.Error()}}, true
	}
	if ok {
		vm.Control = &view
		vm.ControlID = newControlID()
	}
	return vm
}

// newControlID is overridable so tests can pin rendered forms.
var newControlID = func() string { return "web-" + strconv.FormatInt(time.Now().UnixNano(), 36) }

func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	projectID, slug := r.PathValue("project"), r.PathValue("slug")
	if _, ok := s.loadRunOr404(w, projectID, slug, "run status"); !ok {
		return
	}
	s.render(w, r, "run_status_fragments.html", s.runStatusVM(projectID, slug))
}

func (s *Server) handleRunControl(w http.ResponseWriter, r *http.Request) {
	projectID, slug := r.PathValue("project"), r.PathValue("slug")
	if _, ok := s.loadRunOr404(w, projectID, slug, "control"); !ok {
		return
	}
	if s.opts.SubmitControl == nil {
		http.Error(w, "control not configured", http.StatusInternalServerError)
		return
	}
	action := strings.TrimSpace(r.FormValue("action"))
	id := strings.TrimSpace(r.FormValue("id"))
	revision, _ := strconv.Atoi(r.FormValue("revision"))
	if action == "" || id == "" {
		http.Error(w, "control: action and id are required", http.StatusBadRequest)
		return
	}
	if _, err := s.opts.SubmitControl(projectID, slug, action, id, revision); err != nil {
		if errors.Is(err, ErrControlRefused) {
			http.Error(w, "control: "+err.Error(), http.StatusConflict)
			return
		}
		s.logf("control %s/%s %s: %v", projectID, slug, action, err)
		http.Error(w, "control: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug+"#run-status", http.StatusSeeOther)
}

// handleRunStatuses is the dash's compact status refresh: one
// observation pass, keyed by project/run.
func (s *Server) handleRunStatuses(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	if s.opts.RunStatuses != nil {
		var err error
		if out, err = s.opts.RunStatuses(r.URL.Query().Get("project")); err != nil {
			s.logf("run statuses: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
