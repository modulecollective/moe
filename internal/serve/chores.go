package serve

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
)

// choreVM backs the chore detail page (GET /chore/{project}/{name}). It
// is the chore analog of the per-run page: the definition (schedule +
// seed prompt) plus the journal-computed state.
//
// It answers "is this thing going to fire, and when" — the schedule,
// the due verdict, and a link to the run if one is already open — and
// offers edit, plus "open now" when no run is open: the open commit
// only, which an armed heartbeat rides on its next tick.
type choreVM struct {
	Project  string
	Name     string
	Key      string
	Workflow string

	Trigger  string
	Cadence  string
	Cooldown string
	// When is the judged family's prose due-condition, empty for a
	// mechanical chore. Judged mirrors it as the flag the page branches
	// on: a judged chore has no due/not-due answer to render — the pulse
	// survey decides — so "not due" would misreport it as a schedule
	// that simply hasn't fired.
	When   string
	Judged bool
	Prompt string

	Due           bool
	Reasons       string
	LastCompleted string
	NextEligible  string

	// OpenRun is the slug of the chore's currently-open run, if any;
	// OpenRunURL links to its per-run page. Empty when none.
	OpenRun    string
	OpenRunURL string

	// Waiting explains, for a chore that isn't going to fire yet, what it
	// is waiting on — an open run, a cooldown, a judgment, or simply its
	// schedule. Empty for a due chore, whose Due badge says it.
	Waiting string
}

// newChoreVM projects a chore.State onto the detail-page view model.
func newChoreVM(now time.Time, st chore.State) choreVM {
	d := st.Definition
	vm := choreVM{
		Project:       d.Project,
		Name:          d.Name,
		Key:           d.Key(),
		Workflow:      d.Workflow,
		Trigger:       d.Trigger,
		Cadence:       humanChoreInterval(d.Cadence),
		Cooldown:      humanChoreInterval(d.Cooldown),
		When:          d.When,
		Judged:        d.Judged(),
		Prompt:        d.Prompt,
		Due:           st.Due,
		Reasons:       st.ReasonString(),
		LastCompleted: dash.HumanAgo(now, st.LastCompleted),
	}
	if st.OpenRun != "" {
		vm.OpenRun = st.OpenRun
		vm.OpenRunURL = "/run/" + d.Project + "/" + st.OpenRun
	}
	if !st.NextEligible.IsZero() {
		// .Local() before formatting: the instant arrives UTC from the
		// journal index, so the MST verb was honestly printing "UTC" at
		// an operator who wanted the box's clock.
		vm.NextEligible = st.NextEligible.Local().Format("2006-01-02 15:04 MST")
	}
	// Mirror the dash/CLI precedence for why a chore isn't going to fire:
	// an open run wins, then cooldown, then plain not-due — except a
	// judged chore, which is never mechanically due and waits on the
	// sweep's judgment.
	if !st.Due {
		vm.Waiting = st.Status()
	}
	return vm
}

// humanChoreInterval renders a schedule duration for display; a zero
// duration (unset cadence/cooldown) shows an em dash rather than "0s".
func humanChoreInterval(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.String()
}

// handleChorePage renders the chore detail page. A chore isn't a run, so
// it has its own namespace; the page mirrors the per-run frame and shows
// the definition, the journal-computed state, and a link to the chore's
// open run when it has one.
func (s *Server) handleChorePage(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	name := r.PathValue("name")
	vm, status, err := s.gatherChoreVM(project, name)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	s.render(w, r, "chore.html", vm)
}

// gatherChoreVM looks the chore up through the GatherChore callback and
// builds its view model. Returns the HTTP status to use on the error
// path: 500 when the callback is unwired or errors, 404 when no chore
// matches.
func (s *Server) gatherChoreVM(project, name string) (choreVM, int, error) {
	key := project + "/" + name
	if s.opts.GatherChore == nil {
		return choreVM{}, http.StatusInternalServerError, errors.New("chore page not configured (Options.GatherChore is nil)")
	}
	st, ok, err := s.opts.GatherChore(project, name)
	if err != nil {
		s.logf("chore page %s: %v", key, err)
		return choreVM{}, http.StatusInternalServerError, errors.New("chore page: " + err.Error())
	}
	if !ok {
		return choreVM{}, http.StatusNotFound, errors.New("no such chore: " + key)
	}
	return newChoreVM(time.Now(), st), http.StatusOK, nil
}

// choreFormVM backs the one form that creates and edits a chore. Every
// field is a wire string as typed, so a refused submit re-renders with
// nothing lost.
type choreFormVM struct {
	Project string
	Name    string
	// Create: the name is typed here, and the form posts to the
	// project's chores/new route; otherwise it edits Name.
	Create bool
	chore.Spec
	ErrorBanner string
}

func choreFormSpec(r *http.Request) chore.Spec {
	return chore.Spec{
		Trigger:  r.FormValue("trigger"),
		Cadence:  r.FormValue("cadence"),
		Cooldown: r.FormValue("cooldown"),
		When:     r.FormValue("when"),
		Workflow: r.FormValue("workflow"),
		Prompt:   strings.ReplaceAll(r.FormValue("prompt"), "\r\n", "\n"),
	}
}

func (s *Server) handleChoreNewForm(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.requireKnownProject(project); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.render(w, r, "chore_form.html", choreFormVM{Project: project, Create: true})
}

// handleChoreNewSubmit registers a chore and lands on its page. The
// route hangs off the project, not /chore/{project}/new, which would
// shadow a chore named "new".
func (s *Server) handleChoreNewSubmit(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	vm := choreFormVM{Project: project, Name: strings.TrimSpace(r.FormValue("name")), Create: true, Spec: choreFormSpec(r)}
	if err := s.requireKnownProject(project); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if s.opts.CreateChore == nil {
		http.Error(w, "chore writes not configured (Options.CreateChore is nil)", http.StatusInternalServerError)
		return
	}
	if err := s.opts.CreateChore(project, vm.Name, vm.Spec); err != nil {
		s.renderChoreFormError(w, r, vm, err)
		return
	}
	http.Redirect(w, r, "/chore/"+project+"/"+vm.Name, http.StatusSeeOther)
}

func (s *Server) handleChoreEditForm(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("name")
	spec, err := chore.LoadSpec(s.opts.Root, project, name)
	if errors.Is(err, chore.ErrNotFound) {
		http.Error(w, "no such chore: "+project+"/"+name, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "chore edit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "chore_form.html", choreFormVM{Project: project, Name: name, Spec: spec})
}

func (s *Server) handleChoreEditSubmit(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.opts.UpdateChore == nil {
		http.Error(w, "chore writes not configured (Options.UpdateChore is nil)", http.StatusInternalServerError)
		return
	}
	vm := choreFormVM{Project: project, Name: name, Spec: choreFormSpec(r)}
	err := s.opts.UpdateChore(project, name, vm.Spec)
	if errors.Is(err, chore.ErrNotFound) {
		http.Error(w, "no such chore: "+project+"/"+name, http.StatusNotFound)
		return
	}
	if err != nil {
		s.renderChoreFormError(w, r, vm, err)
		return
	}
	http.Redirect(w, r, "/chore/"+project+"/"+name, http.StatusSeeOther)
}

func (s *Server) renderChoreFormError(w http.ResponseWriter, r *http.Request, vm choreFormVM, err error) {
	vm.ErrorBanner = err.Error()
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "chore_form.html", vm)
}

// handleChoreDelete removes the definition and lands on the project
// hub. The form lives on the edit page, off the page people land on.
func (s *Server) handleChoreDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("name")
	err := chore.Remove(s.opts.Root, project, name)
	if errors.Is(err, chore.ErrNotFound) {
		http.Error(w, "no such chore: "+project+"/"+name, http.StatusNotFound)
		return
	}
	if err != nil {
		s.logf("chore delete %s/%s: %v", project, name, err)
		http.Error(w, "chore delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+project, http.StatusSeeOther)
}

// handleChoreOpen opens the chore's run now, ahead of its schedule,
// and lands on the run's page. It writes the open commit and spawns
// nothing: the run carries MoE-Chore, so an armed heartbeat picks it up
// on its next tick, and an unarmed one leaves it parked like any other.
func (s *Server) handleChoreOpen(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("name")
	if s.opts.OpenChore == nil {
		http.Error(w, "chore open not configured (Options.OpenChore is nil)", http.StatusInternalServerError)
		return
	}
	runID, err := s.opts.OpenChore(project, name)
	switch {
	case errors.Is(err, chore.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, chore.ErrNotOpenable):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logf("chore open %s/%s: %v", project, name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+project+"/"+runID, http.StatusSeeOther)
}
