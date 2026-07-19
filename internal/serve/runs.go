package serve

import (
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/workspace"
)

// slugPattern is the kebab-case shape `moe sdlc new` accepts. Mirrors
// the validation moe does itself so a bad slug fails at the form
// rather than after the child has spawned. Lowercase letters, digits,
// and hyphens; must start with a letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// splitID parses the single `project/slug` field both new-* forms now
// take into its two halves, mirroring the CLI's splitProjectRun
// (internal/cli/args.go): cut on the first slash, reject either half
// empty. Kept local rather than shared because internal/cli imports
// internal/serve, so serve can't import the original back without a
// cycle — the two are meant to stay in sync by eye.
func splitID(id string) (project, slug string, err error) {
	project, slug, ok := strings.Cut(id, "/")
	if !ok || project == "" || slug == "" {
		return "", "", errors.New("expected `project/slug`")
	}
	return project, slug, nil
}

// agentOptions is the hardcoded set offered in the new-run form's
// agent dropdown. Two registered agents today; if a third ever
// appears, surface it here rather than pulling from internal/agent
// (which has no exported enumeration). The empty value means "use
// the run's default" — resolved at stage time.
var agentOptions = []string{"", "claude", "codex"}

// workspaceOption is one entry in the new-run form's workspace
// dropdown. Pre-joined as "project/name" so the template doesn't
// need to compose strings.
type workspaceOption struct {
	Project string
	Name    string
	Label   string // "project/name"
}

// newRunVM backs the new-run form. Projects and workspaces are
// gathered from disk at request time; the agent list is static and
// the workflow list comes from Options.NewRunWorkflows.
type newRunVM struct {
	Projects    []string          // project IDs
	Workspaces  []workspaceOption // every named workspace this host has on disk, across all projects
	Agents      []string          // includes "" for "use default"
	Workflows   []NewRunWorkflow  // selector entries; first is the default
	ErrorBanner string            // populated on a POST validation failure (slice #4)
	// ID, Workspace, Agent, Workflow echo the operator's submitted values
	// back into the form on an error re-render so a validation failure
	// doesn't wipe what they typed. ID is the raw `project/slug` text
	// (echoed verbatim, not re-joined, so a malformed entry shows exactly
	// as typed); Workspace/Agent/Workflow re-select the matching dropdown
	// option. On GET, Workflow is pre-selected from the ?workflow= query
	// param when present; unknown or absent falls back to the default.
	ID        string
	Workspace string
	Agent     string
	Workflow  string
}

// newRunWorkflow resolves a submitted (or query-string) workflow name
// against Options.NewRunWorkflows. An empty name falls back to the
// first entry — the form default — so a stale page that POSTs without
// the field keeps working.
func (s *Server) newRunWorkflow(name string) (NewRunWorkflow, bool) {
	if name == "" && len(s.opts.NewRunWorkflows) > 0 {
		return s.opts.NewRunWorkflows[0], true
	}
	for _, wf := range s.opts.NewRunWorkflows {
		if wf.Name == name {
			return wf, true
		}
	}
	return NewRunWorkflow{}, false
}

func (s *Server) handleNewRunForm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.gatherNewRunVM()
	if err != nil {
		s.logf("new-run form gather: %v", err)
		http.Error(w, "new-run form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Pre-select the workflow named in the ?workflow= query string;
	// unknown or absent falls back to the default.
	if wf, ok := s.newRunWorkflow(r.URL.Query().Get("workflow")); ok {
		vm.Workflow = wf.Name
	}
	s.render(w, r, "new.html", vm)
}

// handleNewRunSubmit validates the form, opens the run in-process,
// then spawns `moe <workflow> <first-stage> <p>/<slug>` as a
// PTY-backed agent session and redirects to the per-run page. Opening
// synchronously means an open failure surfaces in the HTTP response
// (instead of the prior spawn-succeeded-but-open-failed half-state),
// and the child has no slug-discovery to do on its way to the agent.
//
// Validation failures re-render the form with an ErrorBanner so the
// operator can correct without retyping.
func (s *Server) handleNewRunSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.spawnAllowed(w) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	wsName := strings.TrimSpace(r.FormValue("workspace"))
	agentName := strings.TrimSpace(r.FormValue("agent"))
	wfName := strings.TrimSpace(r.FormValue("workflow"))
	fail := func(msg string) { s.renderFormError(w, r, id, wsName, agentName, wfName, msg) }

	wf, ok := s.newRunWorkflow(wfName)
	if !ok {
		fail("workflow: unknown workflow " + strconv.Quote(wfName))
		return
	}
	projectID, slug, err := splitID(id)
	if err != nil {
		fail(err.Error())
		return
	}
	if !slugPattern.MatchString(slug) {
		fail("slug: must be kebab-case (lowercase, digits, hyphens; start with letter/digit)")
		return
	}
	if err := s.requireKnownProject(projectID); err != nil {
		fail(err.Error())
		return
	}
	if wsName != "" {
		if err := workspace.ValidateName(wsName); err != nil {
			fail("workspace: " + err.Error())
			return
		}
		// Same refusal the CLI's runNew makes — the binding means
		// nothing to the other workflows and would strand the claim.
		if !wf.Workspace {
			fail("workspace: only sdlc and hooks accept a workspace binding")
			return
		}
	}
	// Agent validity is checked by runopen.Open via run.New; we trust
	// the hardcoded dropdown set here.

	md, err := runopen.Open(s.opts.Root, projectID, run.Options{
		ID:        slug,
		Workflow:  wf.Name,
		Workspace: wsName,
		Agent:     agentName,
	}, s.syncWriter(), s.syncWriter())
	if err != nil {
		fail("open: " + err.Error())
		return
	}

	runID := md.Project + "/" + md.ID
	args := []string{wf.Name, wf.FirstStage, runID}
	if _, err := s.children.spawn(runID, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		fail("spawn: " + err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+md.Project+"/"+md.ID, http.StatusSeeOther)
}

func (s *Server) renderFormError(w http.ResponseWriter, r *http.Request, id, wsName, agentName, wfName, msg string) {
	vm, err := s.gatherNewRunVM()
	if err != nil {
		http.Error(w, msg+" (and form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	vm.ID = id
	vm.Workspace = wsName
	vm.Agent = agentName
	vm.Workflow = wfName
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "new.html", vm)
}

// runVM backs the per-run page (GET /run/{project}/{slug}). It is a
// static panel — no PTY tail, no chain-prompt buttons, no
// remote-controlled end-agent affordance — so the same shape covers
// both the live-parented and read-only render paths.
type runVM struct {
	ID      string
	Project string
	Slug    string
	// RowNote / RowWhen are the dash-row Note and (humanised) When for
	// this run, computed the same way the dash computes them. Empty
	// strings when the row gathered as "no row" (e.g. the run classified
	// into BucketNone, or no GatherRunRow callback wired) — template
	// falls back to the Started / Status line in that case.
	RowNote template.HTML
	RowWhen string
	// NextStage is the run's bare next-stage name (row.Stage), or "" when
	// there's no next stage / no row. The cascade trio keys off it,
	// rendering only when the next stage is spawnable (see
	// composeRunActions).
	NextStage string
	// Started / Status are the fallback meta line shown when the
	// dash-row lookup didn't return a row. Started is empty on the
	// read-only path; Status is "live" / "exited: …" / "exited
	// cleanly" / run.Status.
	Started     string
	Status      string
	Live        bool
	CanvasLinks []canvasLink
	// ChainMembers is the live batch hanging off a chain head, head→tail;
	// empty for every other workflow. The head's canvas is the operator's
	// purpose note, so this — not the canvas — is where membership and
	// per-member status live on the page.
	ChainMembers []chainMemberVM
	// Actions is the peer-affordances block on the per-run page. For
	// an in-progress idea this is edit, promote, and close; for a
	// closed idea this is reopen; other runs render no actions.
	Actions []runAction
}

// runAction is one peer affordance on the per-run page. Empty Method
// renders as a link; POST renders as a small form button.
type runAction struct {
	Label  string
	Href   string
	Method string
}

type canvasLink struct {
	Stage   string
	URL     string // /run/<p>/<r>/canvas/<stage>
	ModTime string // human "Xm ago"
	// Transcripts are the per-agent transcript links for this stage (one
	// per backend thread on disk), rendered beside the canvas link.
	Transcripts []transcriptLink
}

type transcriptLink struct {
	Agent string // "claude" | "codex"
	URL   string // /run/<p>/<r>/transcript/<stage>?agent=<agent>
}

// chainMemberVM is one run in a chain head's batch, rendered with the
// dash's own note and timestamp so the head page and the dash agree
// about what a member's state is called.
type chainMemberVM struct {
	ID   string // <project>/<slug>
	URL  string // /run/<project>/<slug>
	Note template.HTML
	When string
}

// chainState is the head-page slice of live chain truth, gathered once
// per page: the batch, and whether the head is kickable from here. The
// callback replays the journal index, so both consumers (the members
// section and the kick chip) read one gather.
type chainState struct {
	Members []chainMemberVM
	// Kickable narrows to what the dash's `parked · kick?` hint already
	// promises: an in-progress head with a live batch behind it, not
	// itself chained under a live parent. `moe chain kick` would also
	// accept an *empty* head — it closes it and rides nothing — but the
	// dash calls that head `done · close?`, and a chip labelled "kick"
	// that silently closes a placeholder is not the action it names.
	// The web never exceeds the CLI; here it deliberately offers less.
	Kickable bool
}

// gatherChainState reads the live chain for a chain head. Everything
// else — a non-chain run, no callback wired, a gather error — yields
// the zero value, which renders as the read-only page chain heads had
// before: no members section, no kick chip. A gather error is logged
// and swallowed rather than failing the page, matching fillRunRow: the
// canvas link and the meta line are still worth serving.
func (s *Server) gatherChainState(md *run.Metadata, projectID, slug string, now time.Time) chainState {
	if md.Workflow != dash.ChainWorkflow || s.opts.ChainMembers == nil {
		return chainState{}
	}
	rows, chainedUnder, err := s.opts.ChainMembers(projectID, slug)
	if err != nil {
		s.logf("chain members %s/%s: %v", projectID, slug, err)
		return chainState{}
	}
	out := chainState{
		Kickable: chainedUnder == "" && len(rows) > 0 && md.Status == run.StatusInProgress,
	}
	for _, row := range rows {
		id := row.Project + "/" + row.Run
		out.Members = append(out.Members, chainMemberVM{
			ID:   id,
			URL:  "/run/" + id,
			Note: noteHTML(row.Project, row.Note),
			When: dash.HumanAgo(now, row.When),
		})
	}
	return out
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if c, ok := s.children.get(id); ok {
		s.render(w, r, "run.html", s.buildRunVM(c, projectID, slug, id))
		return
	}
	vm, err := s.buildReadOnlyRunVM(projectID, slug, id)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("run page: %v", err)
		http.Error(w, "run page: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "run.html", vm)
}

// handleClose closes a run from the per-run page. It loads the run's
// metadata and dispatches by workflow: capture runs (idea, intent) flip
// closed in-process via runopen.CloseCapture (no harvest, no sandbox);
// everything else
// routes through the CloseRun callback, which dispatches the full cli
// close pipeline by the run's own workflow with --no-edit semantics.
// One route, one guard set, regardless of run kind — a stale or
// replayed POST hits the same refusals.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("close %s: load: %v", id, err)
		http.Error(w, "close: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if dash.IsCapture(md.Workflow) {
		s.closeCaptureRun(w, r, projectID, slug, id, md.Workflow)
		return
	}
	s.closeWorkflowRun(w, r, projectID, slug, id)
}

func (s *Server) closeCaptureRun(w http.ResponseWriter, r *http.Request, projectID, slug, id, workflow string) {
	if err := runopen.CloseCapture(s.opts.Root, projectID, slug, s.syncWriter(), s.syncWriter()); err != nil {
		switch {
		case errors.Is(err, run.ErrRunNotFound):
			http.Error(w, "no such run: "+id, http.StatusNotFound)
		case errors.Is(err, runopen.ErrNotCapture):
			http.Error(w, "run "+id+" is not a closable "+workflow, http.StatusConflict)
		default:
			s.logf("close %s %s: %v", workflow, id, err)
			http.Error(w, "close "+workflow+": "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// closeWorkflowRun closes an in-progress non-capture run through the
// CloseRun callback, which dispatches the registered close pipeline by
// the run's workflow. serve owns the PTY children it spawned, so the
// one guard it applies itself is the live-child refusal: closing while
// the agent is mid-turn would yank the sandbox clone out from under
// it. Every other guard (pushed, terminal, canvas-empty, no registered
// close) lives on the cli side and surfaces through the callback's
// error.
func (s *Server) closeWorkflowRun(w http.ResponseWriter, r *http.Request, projectID, slug, id string) {
	if s.opts.CloseRun == nil {
		http.Error(w, "close not configured (Options.CloseRun is nil)", http.StatusInternalServerError)
		return
	}
	if c, ok := s.children.get(id); ok {
		if exited, _ := c.snapshot(); !exited {
			http.Error(w,
				"run "+id+" has a live agent mid-turn — wait for it to finish, then close",
				http.StatusConflict)
			return
		}
	}
	if err := s.opts.CloseRun(projectID, slug); err != nil {
		var notClosable *runopen.NotClosableError
		if errors.As(err, &notClosable) {
			http.Error(w, "close: "+err.Error(), http.StatusConflict)
			return
		}
		s.logf("close run %s: %v", id, err)
		http.Error(w, "close: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The run is gone; drop any lingering exited-child entry so the dash
	// and run page stop marking it parented.
	s.children.remove(id)
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

func (s *Server) handleIdeaReopen(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := runopen.ReopenIdea(s.opts.Root, projectID, slug, s.syncWriter(), s.syncWriter()); err != nil {
		switch {
		case errors.Is(err, run.ErrRunNotFound):
			http.Error(w, "no such run: "+id, http.StatusNotFound)
		case errors.Is(err, runopen.ErrNotReopenableIdea):
			http.Error(w, "reopen idea: "+err.Error(), http.StatusConflict)
		default:
			s.logf("reopen idea %s: %v", id, err)
			http.Error(w, "reopen idea: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// spawnMode selects which cascade flag (if any) spawnNextStage appends
// to `moe <workflow> <stage> <id>`. The three web chips map one-to-one onto
// the modes, and each mode onto the bang vocabulary: advance (= `!`,
// no flag), ship (= `!!`, --ship, ship this run), chain (= `!!!`,
// --chain, ship + ride the whole chain).
type spawnMode int

const (
	spawnAdvance spawnMode = iota
	spawnShip
	spawnChain
)

// verb is the human-facing label spawnNextStage uses in log lines and
// error bodies for each mode.
func (m spawnMode) verb() string {
	switch m {
	case spawnShip:
		return "ship"
	case spawnChain:
		return "chain"
	default:
		return "advance"
	}
}

// flag is the cascade flag spawnNextStage appends for each mode, or ""
// for advance (a single headless step, no cascade flag).
func (m spawnMode) flag() string {
	switch m {
	case spawnShip:
		return "--ship"
	case spawnChain:
		return "--chain"
	default:
		return ""
	}
}

// handleAdvance spawns the run's next stage interactively with no
// cascade flag: the child runs one stage under the MOE_SERVE_AGENT
// handshake (the operator drives the session through Claude Code on
// the web) and exits at the chain prompt it never reaches. The
// "→ <stage>" chip on the per-run page posts here.
func (s *Server) handleAdvance(w http.ResponseWriter, r *http.Request) {
	s.spawnNextStage(w, r, spawnAdvance)
}

// handleShip spawns the run's next stage under --ship: the
// headless cascade that drives every remaining stage through push and
// ships this run, then stops. The "ship" chip posts here. Bigger lever
// than advance — one click can open/merge a PR — but still operator-
// triggered, and guarded downstream by the test-stage anti-theater gate
// and the pre-push hooks.
func (s *Server) handleShip(w http.ResponseWriter, r *http.Request) {
	s.spawnNextStage(w, r, spawnShip)
}

// handleChain spawns the run's next stage under --chain: the same
// headless cascade as ship, but after this run ships it rides the chain
// into the next live child. The "chain" chip posts here — the biggest
// lever on the page, and like ship it stays operator-triggered.
func (s *Server) handleChain(w http.ResponseWriter, r *http.Request) {
	s.spawnNextStage(w, r, spawnChain)
}

// handleKick rides a chain head headlessly from the browser by spawning
// `moe chain kick <id>` — the same verb, unwrapped. The "kick" chip on
// a parked head's page posts here, finally giving the dash's `parked ·
// kick?` hint a web surface to name: before this the hint pointed at an
// action only a terminal could take.
//
// A spawnNextStage sibling in posture, not in body: a chain head has no
// stage ladder, so there is no next stage to re-derive server-side and
// no spawnable-set check to make. What remains are the stage-free
// guards — insecure gate, workflow, in-progress, no live child.
//
// Deliberately *not* re-checked here: whether the head is itself
// chained under a live parent. That refusal needs the journal index,
// `moe chain kick` already makes it (fail-closed, index errors
// propagate), and the chip doesn't render when it would fire. Copying
// it into serve would be a second authority on the same question that
// could disagree with the first.
func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	if !s.spawnAllowed(w) {
		return
	}
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("kick %s: load: %v", id, err)
		http.Error(w, "kick: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if md.Workflow != dash.ChainWorkflow {
		http.Error(w, "run "+id+" is a "+md.Workflow+" run, not a chain head", http.StatusConflict)
		return
	}
	if md.Status != run.StatusInProgress {
		http.Error(w, "run "+id+" is not in progress (status="+md.Status+")", http.StatusConflict)
		return
	}
	if c, ok := s.children.get(id); ok {
		if exited, _ := c.snapshot(); !exited {
			http.Error(w,
				"run "+id+" has a live agent mid-turn — wait for it to finish, then kick",
				http.StatusConflict)
			return
		}
	}
	if _, err := s.children.spawn(id, s.opts.MoeBin, []string{"chain", "kick", id}, s.opts.Root, s.opts.Logger); err != nil {
		s.logf("kick %s: spawn: %v", id, err)
		http.Error(w, "kick: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// spawnNextStage is the shared body behind /advance, /ship, and /chain.
// It re-derives the next stage server-side (never trusting a possibly-
// stale page) and applies the same guard set the close route uses,
// then spawns `moe <workflow> <stage> <id>` — appending the mode's
// cascade flag (--ship / --chain, or none for advance). Only workflows
// whose declaration carries Cascade qualify (the operator-cascade set —
// their stage verbs accept the flags), and the next stage must
// be in the declared spawnable set (push stays terminal/CLI-only via
// sdlc's exclusion). The server-side re-derivation plus spawn's own
// dup-guard mean a double-click or a stale button can't double-spawn
// or skip a stage.
//
// A direct spawn deliberately bypasses the design-stage cascade's
// tracked-change refusal (EnforceSandboxBoundary): the explicit click
// is the consent that guard asks for at the chain prompt.
func (s *Server) spawnNextStage(w http.ResponseWriter, r *http.Request, mode spawnMode) {
	if !s.spawnAllowed(w) {
		return
	}
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug
	verb := mode.verb()

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("%s %s: load: %v", verb, id, err)
		http.Error(w, verb+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.opts.WorkflowUI == nil {
		http.Error(w, verb+" not configured (Options.WorkflowUI is nil)", http.StatusInternalServerError)
		return
	}
	ui, ok := s.opts.WorkflowUI(md.Workflow)
	if !ok || !ui.Cascade {
		http.Error(w, "workflow "+md.Workflow+" does not "+verb+" from serve", http.StatusConflict)
		return
	}
	if md.Status != run.StatusInProgress {
		http.Error(w, "run "+id+" is not in progress (status="+md.Status+")", http.StatusConflict)
		return
	}
	// A live agent mid-turn owns the sandbox clone; spawning the next
	// stage now would race it. Mirror closeWorkflowRun's live-child
	// refusal.
	if c, ok := s.children.get(id); ok {
		if exited, _ := c.snapshot(); !exited {
			http.Error(w,
				"run "+id+" has a live agent mid-turn — wait for it to finish, then "+verb,
				http.StatusConflict)
			return
		}
	}
	// Re-derive the next stage rather than trusting the button. row.Stage
	// is the bare next-stage name with all satisfaction logic baked in.
	stage, err := s.nextStage(projectID, slug)
	if err != nil {
		s.logf("%s %s: next stage: %v", verb, id, err)
		http.Error(w, verb+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !slices.Contains(ui.Stages, stage) {
		// "" (no next stage) or an excluded stage (sdlc's push) — push
		// stays terminal/CLI-only.
		http.Error(w,
			"run "+id+" has no advanceable next stage (next="+strconv.Quote(stage)+")",
			http.StatusConflict)
		return
	}

	args := []string{md.Workflow, stage, id}
	if flag := mode.flag(); flag != "" {
		args = append(args, flag)
	}
	if _, err := s.children.spawn(id, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.logf("%s %s: spawn: %v", verb, id, err)
		http.Error(w, verb+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// nextStage re-derives a run's bare next-stage name through the
// GatherRunRow callback (the same lookup fillRunRow uses for the
// dash-row meta). Returns "" when no callback is wired or the row
// gathered as not-found / filtered — callers treat "" as "no
// advanceable stage" and refuse.
func (s *Server) nextStage(projectID, slug string) (string, error) {
	if s.opts.GatherRunRow == nil {
		return "", nil
	}
	row, ok, err := s.opts.GatherRunRow(projectID, slug)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return row.Stage, nil
}

// buildReadOnlyRunVM constructs a runVM from on-disk state for a run
// not currently parented by this serve. For in-progress idea runs,
// the page surfaces edit/promote affordances; for sdlc runs, just the
// dash-row meta and canvas links.
func (s *Server) buildReadOnlyRunVM(projectID, slug, id string) (runVM, error) {
	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		return runVM{}, err
	}
	now := time.Now()
	vm := runVM{
		ID:          id,
		Project:     projectID,
		Slug:        slug,
		Status:      md.Status,
		CanvasLinks: s.canvasLinks(projectID, slug, now),
	}
	s.fillRunRow(&vm, projectID, slug, now)
	chain := s.gatherChainState(md, projectID, slug, now)
	vm.ChainMembers = chain.Members
	// No live child on the read-only path (this serve isn't parenting the
	// run), so the spawn chips gate on live=false. fillRunRow ran first
	// so vm.NextStage is populated.
	vm.Actions = s.composeRunActions(projectID, slug, vm.NextStage, md, false, chain)
	return vm, nil
}

// composeRunActions returns the peer-affordances list for the per-run
// page. Idea runs keep their bespoke chips (edit / promote / close /
// reopen — idea has no stage verbs to derive). Every other workflow's
// chips are composed from its registration-time serve declaration
// (Options.WorkflowUI): cascade workflows get the "→ <stage>" /
// "ship" / "chain" trio keyed off the re-derived next stage. Workflows
// with a close pipeline get a close-run chip when close is the routine
// idle-page next move; perpetual workflows keep close off the idle page
// but still expose it while a child is live. A workflow that declared
// nothing — or declared without cascade — renders no spawn chips: the
// read-only page plus, where applicable, the close chip.
//
// nextStage is the bare next-stage name re-derived from the dash row;
// live is true when an agent is mid-turn. Spawn chips drop while live
// (spawning past a stage whose agent is still running would race it
// for the sandbox clone). Close chips stay for non-perpetual
// workflows, and for live perpetual pages; the close route's own
// live-child refusal guards the click.
func (s *Server) composeRunActions(projectID, slug, nextStage string, md *run.Metadata, live bool, chain chainState) []runAction {
	base := "/run/" + projectID + "/" + slug
	if md.Workflow == dash.ChainWorkflow {
		// A chain head declares no serve workflow UI — it has no stages,
		// so there is nothing for the cascade trio to spawn — which is why
		// its one chip is bespoke like idea's rather than derived. It
		// spawns an agent ride all the same, so it keeps the trio's gates:
		// insecure mode, and not while a child is mid-turn.
		if !live && s.opts.Insecure && chain.Kickable {
			return []runAction{{Label: "kick", Href: base + "/kick", Method: "POST"}}
		}
		return nil
	}
	if dash.IsCapture(md.Workflow) {
		switch md.Status {
		case run.StatusInProgress:
			// edit / close are journal-only, so both captures get them
			// in safe mode. promote spawns the destination run's agent,
			// so it's gated to insecure mode — and to ideas, which are
			// the only capture that promotes.
			out := []runAction{{Label: "edit " + md.Workflow, Href: base + "/edit"}}
			if s.opts.Insecure && md.Workflow == dash.IdeaWorkflow {
				out = append(out, runAction{Label: "promote", Href: base + "/promote"})
			}
			return append(out, runAction{Label: "close " + md.Workflow, Href: base + "/close", Method: "POST"})
		case run.StatusClosed:
			// Reopen stays idea-only: the intent verb set has no reopen
			// (cli/intent.go), and the web must not exceed the CLI's.
			if md.Workflow == dash.IdeaWorkflow {
				return []runAction{
					{Label: "reopen idea", Href: base + "/reopen", Method: "POST"},
				}
			}
		}
		return nil
	}
	if s.opts.WorkflowUI == nil {
		return nil
	}
	ui, ok := s.opts.WorkflowUI(md.Workflow)
	if !ok || md.Status != run.StatusInProgress {
		return nil
	}
	var out []runAction
	// The cascade trio chips spawn an agent, so they render only in
	// insecure mode. The close-run chip below is journal-only (CloseRun
	// runs in-process, no spawn) and stays in safe mode.
	if !live && s.opts.Insecure && ui.Cascade {
		// A "" or excluded next stage (sdlc's push) yields no trio:
		// push stays terminal/CLI-only — the bang vocabulary
		// collapses there — so a run parked right before push shows
		// only the close chip.
		if slices.Contains(ui.Stages, nextStage) {
			out = append(out,
				runAction{Label: "→ " + nextStage, Href: base + "/advance", Method: "POST"},
				runAction{Label: "ship", Href: base + "/ship", Method: "POST"},
				runAction{Label: "chain", Href: base + "/chain", Method: "POST"})
		}
	}
	if ui.Close && (!ui.Perpetual || live) {
		out = append(out, runAction{Label: "close run", Href: base + "/close", Method: "POST"})
	}
	return out
}

// fillRunRow populates RowNote / RowWhen from the dash-row lookup.
// Errors are swallowed (logged) so a row-gather hiccup never breaks
// the per-run page; the template falls back to the Started / Status
// meta line when the row note is empty.
func (s *Server) fillRunRow(vm *runVM, projectID, slug string, now time.Time) {
	if s.opts.GatherRunRow == nil {
		return
	}
	row, ok, err := s.opts.GatherRunRow(projectID, slug)
	if err != nil {
		s.logf("run row gather %s/%s: %v", projectID, slug, err)
		return
	}
	if !ok {
		return
	}
	vm.RowNote = noteHTML(row.Project, row.Note)
	vm.RowWhen = dash.HumanAgo(now, row.When)
	vm.NextStage = row.Stage
}

// isPromotableIdea reports whether the loaded run is an in-progress
// idea — the gate for offering the promote-to-sdlc affordance.
func isPromotableIdea(md *run.Metadata) bool {
	return md.Workflow == dash.IdeaWorkflow && md.Status == run.StatusInProgress
}

// buildRunVM assembles the per-run page from the live child's state
// and the on-disk canvas listing.
func (s *Server) buildRunVM(c *child, projectID, slug, id string) runVM {
	exited, exitErr := c.snapshot()
	now := time.Now()
	vm := runVM{
		ID:      id,
		Project: projectID,
		Slug:    slug,
		Started: dash.HumanAgo(now, c.started),
		Live:    !exited,
	}
	switch {
	case !exited:
		vm.Status = "live"
	case exitErr != nil:
		vm.Status = "exited: " + exitErr.Error()
	default:
		vm.Status = "exited cleanly"
	}
	vm.CanvasLinks = s.canvasLinks(projectID, slug, now)
	s.fillRunRow(&vm, projectID, slug, now)
	// A live-parented run is usually sdlc, but opening a chore can spawn
	// any configured workflow (e.g. a chore whose `workflow` is `twin`), so don't
	// assume the workflow here — gate the action chips on the on-disk
	// metadata (composeRunActions keys off the workflow's declaration).
	// A load failure just drops the chips.
	if md, err := run.Load(s.opts.Root, projectID, slug); err != nil {
		s.logf("run page %s: load for actions: %v", id, err)
	} else {
		chain := s.gatherChainState(md, projectID, slug, now)
		vm.ChainMembers = chain.Members
		// !exited == an agent mid-turn; composeRunActions drops the spawn
		// chips in that case. fillRunRow above populated vm.NextStage.
		vm.Actions = s.composeRunActions(projectID, slug, vm.NextStage, md, !exited, chain)
	}
	return vm
}

// canvasLinks enumerates the run's stage canvas files (rendered in
// workflow ladder order) with their mtimes. Only stages whose
// content.md actually exists are surfaced.
//
// Resolution routes through Options.ResolveCanvas — the same callback
// the canvas route and `moe sdlc cat` use — so an in-progress run
// whose canonical-root documents/ is empty still surfaces links to
// the live session's worktree copy. Before this, canvasLinks did its
// own `ReadDir` on the canonical docs dir; for in-progress runs that
// directory is empty (the agent edits live under .moe/worktrees/…),
// so no links were emitted and the canvas was effectively invisible
// on the run page.
//
// A nil ResolveCanvas or RunStages yields no links. `moe serve` wires
// both in cli/serve.go; tests that want canvas links must too. Note
// that the session-vs-canonical decision baked into ResolveCanvas
// depends on session.List finding worktrees under <Root>/.moe — i.e.
// serve must run from the bureaucracy root, not from inside a session
// worktree, or the live-edit branch silently falls back to canonical.
func (s *Server) canvasLinks(projectID, slug string, now time.Time) []canvasLink {
	if s.opts.ResolveCanvas == nil || s.opts.RunStages == nil {
		return nil
	}
	ladder, err := s.opts.RunStages(projectID, slug)
	if err != nil {
		return nil
	}
	out := make([]canvasLink, 0, len(ladder))
	for _, stage := range ladder {
		path, err := s.opts.ResolveCanvas(projectID, slug, stage)
		if err != nil {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		out = append(out, canvasLink{
			Stage:       stage,
			URL:         "/run/" + projectID + "/" + slug + "/canvas/" + stage,
			ModTime:     dash.HumanAgo(now, st.ModTime()),
			Transcripts: s.transcriptLinks(projectID, slug, stage),
		})
	}
	return out
}

// transcriptLinks returns the per-agent transcript links for a stage —
// one per backend thread present on the canonical path. Threads mirror
// there when a turn closes, so an in-progress stage mid-first-turn has
// none yet (accurate as of the last closed turn, same posture as the
// canvas route's live view). Chat runs are where this pays most: the
// chat canvas is only session markers, the transcript is the content.
func (s *Server) transcriptLinks(projectID, slug, stage string) []transcriptLink {
	var links []transcriptLink
	for _, agent := range []string{"claude", "codex"} {
		path := filepath.Join(s.opts.Root, run.ThreadPathFor(agent, projectID, slug, stage))
		if !fileExists(path) {
			continue
		}
		links = append(links, transcriptLink{
			Agent: agent,
			URL:   "/run/" + projectID + "/" + slug + "/transcript/" + stage + "?agent=" + agent,
		})
	}
	return links
}

// promoteVM backs the per-idea promote page (GET /run/{p}/{s}/promote).
// Workspaces is every named workspace this host knows about (cross-
// project, mirroring /run/new); Agents includes "" for "use default";
// Workflows mirrors the new-run form's destination selector (sdlc is
// the only entry today; `moe sdlc new --from-idea` is the CLI face of
// the same move). ErrorBanner is populated on POST validation failure
// so the re-render keeps the operator's correction surface in one
// place.
type promoteVM struct {
	Project     string
	Slug        string
	Workspaces  []workspaceOption
	Agents      []string
	Workflows   []NewRunWorkflow
	Workflow    string // selected entry, echoed on error re-render
	ErrorBanner string
}

// gatherPromoteVM returns the dropdown content the promote page needs.
// Pulled from disk per request, same as gatherNewRunVM.
func (s *Server) gatherPromoteVM(projectID, slug string) (promoteVM, error) {
	infos, err := workspace.List(s.opts.Root, "")
	if err != nil {
		return promoteVM{}, err
	}
	wsOpts := make([]workspaceOption, 0, len(infos))
	for _, info := range infos {
		wsOpts = append(wsOpts, workspaceOption{
			Project: info.Project,
			Name:    info.Name,
			Label:   info.Project + "/" + info.Name,
		})
	}
	vm := promoteVM{
		Project:    projectID,
		Slug:       slug,
		Workspaces: wsOpts,
		Agents:     agentOptions,
		Workflows:  s.opts.NewRunWorkflows,
	}
	if len(vm.Workflows) > 0 {
		vm.Workflow = vm.Workflows[0].Name
	}
	return vm, nil
}

// handlePromoteForm renders the per-idea promote page (GET). 404 when
// the slug doesn't exist, 409 when the run is not a promotable idea —
// same gates POST applies, so a stale bookmark fails the same way at
// either method.
func (s *Server) handlePromoteForm(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("promote form: load %s: %v", id, err)
		http.Error(w, "promote form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !isPromotableIdea(md) {
		http.Error(w,
			"run "+id+" is not a promotable idea (workflow="+md.Workflow+", status="+md.Status+")",
			http.StatusConflict)
		return
	}

	vm, err := s.gatherPromoteVM(projectID, slug)
	if err != nil {
		s.logf("promote form gather: %v", err)
		http.Error(w, "promote form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "promote.html", vm)
}

// handlePromote opens the destination run in-process by calling
// runopen.Promote with the chosen workflow (sdlc default; the web face
// of `moe <workflow> new --from-idea`), then spawns
// `moe <workflow> <first-stage> <p>/<newslug>` as a PTY-backed agent
// session and redirects to the new run's page. Opening synchronously
// means the destination's slug is known before the spawn — no
// placeholder id, no stdout regex, no rename race. Validation failures
// re-render the promote page with an inline error banner.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	if !s.spawnAllowed(w) {
		return
	}
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	wsName := strings.TrimSpace(r.FormValue("workspace"))
	agentName := strings.TrimSpace(r.FormValue("agent"))
	wfName := strings.TrimSpace(r.FormValue("workflow"))
	fail := func(msg string) { s.renderPromoteError(w, r, projectID, slug, wfName, msg) }

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("promote: load %s: %v", id, err)
		http.Error(w, "promote: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !isPromotableIdea(md) {
		http.Error(w,
			"run "+id+" is not a promotable idea (workflow="+md.Workflow+", status="+md.Status+")",
			http.StatusConflict)
		return
	}
	wf, ok := s.newRunWorkflow(wfName)
	if !ok {
		fail("workflow: unknown workflow " + strconv.Quote(wfName))
		return
	}
	if wsName != "" {
		if err := workspace.ValidateName(wsName); err != nil {
			fail("workspace: " + err.Error())
			return
		}
		if !wf.Workspace {
			fail("workspace: only sdlc and hooks accept a workspace binding")
			return
		}
	}
	// Agent membership rides the hardcoded dropdown set.

	promoted, err := runopen.Promote(s.opts.Root, projectID, slug, runopen.PromoteOptions{
		Workflow:   wf.Name,
		FirstStage: wf.FirstStage,
		Workspace:  wsName,
		Agent:      agentName,
	}, s.syncWriter(), s.syncWriter())
	if err != nil {
		fail("promote: " + err.Error())
		return
	}
	if promoted.MarkErr != nil {
		// The destination run is already open; surface the bookkeeping
		// failure in the log so the operator can re-mark the idea by
		// hand if needed. The destination's MoE-Idea trailer still
		// records the source.
		s.logf("promote: mark idea %s/%s promoted: %v", projectID, slug, promoted.MarkErr)
	}

	destID := promoted.Run.Project + "/" + promoted.Run.ID
	args := []string{wf.Name, wf.FirstStage, destID}
	if _, err := s.children.spawn(destID, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		fail("spawn: " + err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+promoted.Run.Project+"/"+promoted.Run.ID, http.StatusSeeOther)
}

func (s *Server) renderPromoteError(w http.ResponseWriter, r *http.Request, projectID, slug, wfName, msg string) {
	vm, err := s.gatherPromoteVM(projectID, slug)
	if err != nil {
		http.Error(w, msg+" (and promote form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	if wfName != "" {
		vm.Workflow = wfName
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "promote.html", vm)
}

func (s *Server) gatherNewRunVM() (newRunVM, error) {
	projectIDs, err := s.listProjectIDs()
	if err != nil {
		return newRunVM{}, err
	}

	infos, err := workspace.List(s.opts.Root, "")
	if err != nil {
		return newRunVM{}, err
	}
	wsOpts := make([]workspaceOption, 0, len(infos))
	for _, info := range infos {
		wsOpts = append(wsOpts, workspaceOption{
			Project: info.Project,
			Name:    info.Name,
			Label:   info.Project + "/" + info.Name,
		})
	}

	vm := newRunVM{
		Projects:   projectIDs,
		Workspaces: wsOpts,
		Agents:     agentOptions,
		Workflows:  s.opts.NewRunWorkflows,
	}
	if len(vm.Workflows) > 0 {
		vm.Workflow = vm.Workflows[0].Name
	}
	return vm, nil
}

// listProjectIDs returns the sorted set of registered project IDs.
// Shared by the new-run and new-idea forms; the idea form needs
// nothing else from gatherNewRunVM, so this stays a small helper
// rather than dragging a workspace listing through the idea path.
func (s *Server) listProjectIDs() ([]string, error) {
	mds, warns, err := project.List(s.opts.Root)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		s.logf("project list: skipping %s: %v", w.ID, w.Err)
	}
	projectIDs := make([]string, 0, len(mds))
	for _, md := range mds {
		projectIDs = append(projectIDs, md.ID)
	}
	sort.Strings(projectIDs)
	return projectIDs, nil
}

// requireKnownProject rejects a project id that isn't in the registered
// set, mirroring the CLI's requireProject (internal/cli/idea.go) so the
// web forms fail the same way the CLI does. The dropdown the forms used
// to carry made an unknown project unreachable; a free-text field
// doesn't, so the check moves server-side — catching it here yields a
// clean "unknown project" banner instead of leaking a downstream
// runopen.Open error.
func (s *Server) requireKnownProject(projectID string) error {
	ids, err := s.listProjectIDs()
	if err != nil {
		return err
	}
	if !slices.Contains(ids, projectID) {
		return errors.New("unknown project: " + projectID)
	}
	return nil
}

// newIdeaVM backs the new-idea form. Projects are gathered from disk
// at request time; there are no workspace / agent dropdowns because
// idea runs don't host a PTY session and have no workspace binding.
type newIdeaVM struct {
	Projects    []string
	ErrorBanner string
	// ID, Body echo the operator's submitted values back on an error
	// re-render so a validation failure doesn't wipe a typed-out idea.
	// ID is the raw `project/slug` text, echoed verbatim.
	ID   string
	Body string
}

func (s *Server) handleNewIdeaForm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.gatherNewIdeaVM()
	if err != nil {
		s.logf("new-idea form gather: %v", err)
		http.Error(w, "new-idea form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "new_idea.html", vm)
}

// handleNewIdeaSubmit validates the form and opens an idea run
// in-process via runopen.Open. No PTY spawn — idea runs are a
// single-stage doc with no live agent — so the handler redirects
// straight to the per-run page once the open commit lands.
//
// Body is taken verbatim with CRLF normalised to LF (browsers send
// \r\n in textarea bodies; canvases live on disk as LF). An empty
// body falls back to "# {slug}\n", matching the CLI stub
// (internal/cli/idea.go:185).
func (s *Server) handleNewIdeaSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("id"))
	body := strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")
	// Echo the raw typed id and body on every error path so the operator
	// never loses a multi-line idea to a validation slip.
	fail := func(msg string) { s.renderIdeaFormError(w, r, id, body, msg) }

	projectID, slug, err := splitID(id)
	if err != nil {
		fail(err.Error())
		return
	}
	if !slugPattern.MatchString(slug) {
		fail("slug: must be kebab-case (lowercase, digits, hyphens; start with letter/digit)")
		return
	}
	if err := s.requireKnownProject(projectID); err != nil {
		fail(err.Error())
		return
	}

	seed := body
	if seed == "" {
		seed = "# " + slug + "\n"
	}
	md, err := runopen.Open(s.opts.Root, projectID, run.Options{
		ID:       slug,
		Workflow: dash.IdeaWorkflow,
		SeedDocs: map[string]string{dash.IdeaDocID: seed},
	}, s.syncWriter(), s.syncWriter())
	if err != nil {
		fail("open: " + err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+md.Project+"/"+md.ID, http.StatusSeeOther)
}

func (s *Server) renderIdeaFormError(w http.ResponseWriter, r *http.Request, id, body, msg string) {
	vm, err := s.gatherNewIdeaVM()
	if err != nil {
		http.Error(w, msg+" (and form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	vm.ID = id
	vm.Body = body
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "new_idea.html", vm)
}

func (s *Server) gatherNewIdeaVM() (newIdeaVM, error) {
	projectIDs, err := s.listProjectIDs()
	if err != nil {
		return newIdeaVM{}, err
	}
	return newIdeaVM{Projects: projectIDs}, nil
}

// editCaptureVM backs the per-capture edit page (GET
// /run/{p}/{s}/edit) for both ideas and intents. Body is the current
// canvas content (seeded into the textarea); a missing file falls back
// to empty and the operator can save into it. Kind is the capture's
// workflow, so the page's chrome says "edit intent" on an intent.
// ErrorBanner is populated on POST validation failure.
type editCaptureVM struct {
	Project     string
	Slug        string
	Kind        string
	Body        string
	ErrorBanner string
}

// handleCaptureEditForm renders the textarea seeded with the capture's
// canvas. 404 / 409 mirror handlePromoteForm's gates.
func (s *Server) handleCaptureEditForm(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			http.Error(w, "no such run: "+id, http.StatusNotFound)
			return
		}
		s.logf("edit form: load %s: %v", id, err)
		http.Error(w, "edit form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	docID, ok := dash.CaptureDocID(md.Workflow)
	if !ok || md.Status != run.StatusInProgress {
		http.Error(w,
			"run "+id+" is not an editable capture (workflow="+md.Workflow+", status="+md.Status+")",
			http.StatusConflict)
		return
	}

	body, err := os.ReadFile(filepath.Join(s.opts.Root, run.ContentPath(projectID, slug, docID)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logf("edit form: read canvas %s/%s: %v", projectID, slug, err)
		http.Error(w, "edit form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "edit_idea.html", editCaptureVM{
		Project: projectID,
		Slug:    slug,
		Kind:    md.Workflow,
		Body:    string(body),
	})
}

// handleCaptureEditSubmit writes the textarea body to the capture's
// canvas and commits with the trailers the matching CLI edit verb
// produces. CRLF is normalised to LF (mirrors handleNewIdeaSubmit).
// Defends against a replayed POST landing on a now-terminal capture by
// re-checking the gate inside runopen.EditCapture.
func (s *Server) handleCaptureEditSubmit(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")

	err := runopen.EditCapture(s.opts.Root, projectID, slug, body, s.syncWriter(), s.syncWriter())
	switch {
	case errors.Is(err, run.ErrRunNotFound):
		http.Error(w, "no such run: "+id, http.StatusNotFound)
		return
	case errors.Is(err, runopen.ErrNotCapture):
		http.Error(w,
			"run "+id+" is not an editable capture",
			http.StatusConflict)
		return
	case errors.Is(err, run.ErrNothingToCommit):
		// No-op edit — body matched on-disk content. Treat as success;
		// the operator wanted to land their text and it's there.
	case err != nil:
		s.renderCaptureEditError(w, r, projectID, slug, body, "edit: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// renderCaptureEditError re-renders the edit page with the operator's
// unsaved body and a banner. It re-loads the run only for the Kind
// label — the happy path never pays for this — and leaves Kind empty
// if the load fails, which the template falls back on.
func (s *Server) renderCaptureEditError(w http.ResponseWriter, r *http.Request, projectID, slug, body, msg string) {
	kind := ""
	if md, err := run.Load(s.opts.Root, projectID, slug); err == nil {
		kind = md.Workflow
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "edit_idea.html", editCaptureVM{
		Project:     projectID,
		Slug:        slug,
		Kind:        kind,
		Body:        body,
		ErrorBanner: msg,
	})
}
