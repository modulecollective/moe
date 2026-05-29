package serve

import (
	"errors"
	"net/http"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
)

// ErrChoreNotFound and ErrChoreNotOpenable are the two guard outcomes the
// OpenChore callback signals by wrapping. The open route maps the first
// to 404 and the second to 409; cli/serve.go translates its internal
// guard errors into these so serve needn't import the cli package.
var (
	ErrChoreNotFound    = errors.New("chore not found")
	ErrChoreNotOpenable = errors.New("chore not openable")
)

// ChoreOpen is the result of an in-process chore open: the destination
// run's identity plus the workflow + first stage serve spawns to host
// the agent session (serve stays workflow-registry-free, so the caller
// resolves both).
type ChoreOpen struct {
	Project    string
	Slug       string
	Workflow   string
	FirstStage string
}

// choreVM backs the chore detail page (GET /chore/{project}/{name}). It
// is the chore analog of the per-run page: the definition (schedule +
// seed prompt) plus the journal-computed state, and an open affordance
// when the chore is openable.
type choreVM struct {
	Project  string
	Name     string
	Key      string
	Workflow string

	Trigger  string
	Cadence  string
	Cooldown string
	Prompt   string

	Due           bool
	Reasons       string
	LastCompleted string
	NextEligible  string

	// OpenRun is the slug of the chore's currently-open run, if any;
	// OpenRunURL links to its per-run page. Empty when none.
	OpenRun    string
	OpenRunURL string

	// Openable is true when the open affordance should render live. A
	// due chore is always openable by construction (chore.Evaluate only
	// sets Due with no open run and no cooldown); BlockReason explains
	// the disabled state otherwise.
	Openable    bool
	BlockReason string

	// ErrorBanner is populated on a 409 re-render after a raced/stale
	// open POST, mirroring the promote page's inline banner.
	ErrorBanner string
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
		Prompt:        d.Prompt,
		Due:           st.Due,
		Reasons:       st.ReasonString(),
		LastCompleted: dash.HumanAgo(now, st.LastCompleted),
		Openable:      st.Due,
	}
	if st.OpenRun != "" {
		vm.OpenRun = st.OpenRun
		vm.OpenRunURL = "/run/" + d.Project + "/" + st.OpenRun
	}
	if !st.NextEligible.IsZero() {
		vm.NextEligible = st.NextEligible.Format("2006-01-02 15:04 MST")
	}
	// Mirror the dash/CLI precedence for why a chore can't open: an open
	// run wins, then cooldown, then plain not-due.
	if !vm.Openable {
		switch {
		case st.OpenRun != "":
			vm.BlockReason = "open run " + st.OpenRun
		case st.CooldownBlocking:
			vm.BlockReason = "cooling down until " + vm.NextEligible
		default:
			vm.BlockReason = "not due"
		}
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
// the definition, the journal-computed state, and (when openable) the
// open affordance.
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

// handleChoreOpen opens the chore's configured-workflow run in-process,
// then spawns `moe <workflow> <firstStage> <dest>` as a PTY-backed agent
// session and redirects to the new run's page — the chore analog of
// handlePromote. The OpenChore callback re-checks the guards, so a stale
// or raced POST (a chore that gained an open run / went on cooldown
// since the page rendered) maps to 404/409 here too.
func (s *Server) handleChoreOpen(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	name := r.PathValue("name")
	key := project + "/" + name

	if s.opts.OpenChore == nil {
		http.Error(w, "chore open not configured (Options.OpenChore is nil)", http.StatusInternalServerError)
		return
	}
	dest, err := s.opts.OpenChore(project, name)
	if err != nil {
		switch {
		case errors.Is(err, ErrChoreNotFound):
			http.Error(w, "no such chore: "+key, http.StatusNotFound)
		case errors.Is(err, ErrChoreNotOpenable):
			s.renderChoreOpenConflict(w, r, project, name)
		default:
			s.logf("chore open %s: %v", key, err)
			http.Error(w, "chore open: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	id := dest.Project + "/" + dest.Slug
	args := []string{dest.Workflow, dest.FirstStage, id}
	if _, err := s.children.spawn(id, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.logf("chore open %s: spawn: %v", key, err)
		http.Error(w, "chore open: spawn: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+id, http.StatusSeeOther)
}

// renderChoreOpenConflict re-renders the chore page with an inline error
// banner at 409 — the chore raced from openable to blocked between the
// page render and the POST. The banner reads the freshly-gathered block
// reason so it reflects current disk state. Falls back to a plain 409 if
// the re-gather fails (e.g. the chore vanished outright).
func (s *Server) renderChoreOpenConflict(w http.ResponseWriter, r *http.Request, project, name string) {
	vm, _, err := s.gatherChoreVM(project, name)
	if err != nil {
		http.Error(w, "chore open: "+project+"/"+name+" is no longer openable", http.StatusConflict)
		return
	}
	if vm.BlockReason != "" {
		vm.ErrorBanner = "open failed — " + vm.BlockReason
	} else {
		vm.ErrorBanner = "open failed — chore state changed, retry"
	}
	w.WriteHeader(http.StatusConflict)
	s.render(w, r, "chore.html", vm)
}
