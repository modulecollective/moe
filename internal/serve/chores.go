package serve

import (
	"errors"
	"net/http"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
)

// choreVM backs the chore detail page (GET /chore/{project}/{name}). It
// is the chore analog of the per-run page: the definition (schedule +
// seed prompt) plus the journal-computed state.
//
// Read-only, and deliberately: a due chore is the heartbeat's to open,
// on its own cadence, and `moe chore open --now` is the operator's
// override for when that wait is too long. What the page owes is the
// answer to "is this thing going to fire, and when" — the schedule, the
// due verdict, and a link to the run if one is already open.
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
		switch {
		case st.OpenRun != "":
			vm.Waiting = "open run " + st.OpenRun
		case st.CooldownBlocking:
			vm.Waiting = "cooling down until " + vm.NextEligible
		case vm.Judged:
			vm.Waiting = "judged — the sweep decides"
		default:
			vm.Waiting = "not due"
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
