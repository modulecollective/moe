package serve

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/workspace"
)

// promotedWorkflow names the workflow both the new-run form and the
// promote-from-idea form open into. Hardcoded to sdlc: the operator-
// facing surface only fronts sdlc runs (the other workflows have
// their own entry points elsewhere), and serve only knows how to
// host one kind of agent session.
const promotedWorkflow = "sdlc"

// promotedFirstStage is the destination workflow's first-stage doc
// id (where a promote seeds the source idea's canvas) and the verb
// serve spawns to host the agent session. Sdlc's first stage is
// design; if a second workflow ever fronts here, this would split.
const promotedFirstStage = "design"

// slugPattern is the kebab-case shape `moe sdlc new` accepts. Mirrors
// the validation moe does itself so a bad slug fails at the form
// rather than after the child has spawned. Lowercase letters, digits,
// and hyphens; must start with a letter or digit.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// projectIDPattern matches the project IDs `project.List` returns.
// Same character class as slugs (project ids are derived from repo
// names, also kebab-case).
var projectIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

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
// gathered from disk at request time; the agent list is static.
type newRunVM struct {
	Projects    []string          // project IDs
	Workspaces  []workspaceOption // every named workspace this host has on disk, across all projects
	Agents      []string          // includes "" for "use default"
	ErrorBanner string            // populated on a POST validation failure (slice #4)
}

func (s *Server) handleNewRunForm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.gatherNewRunVM()
	if err != nil {
		s.logf("new-run form gather: %v", err)
		http.Error(w, "new-run form: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "new.html", vm)
}

// handleNewRunSubmit validates the form, opens the run in-process,
// then spawns `moe sdlc design <p>/<slug>` as a PTY-backed agent
// session and redirects to the per-run page. Opening synchronously
// means an open failure surfaces in the HTTP response (instead of
// the prior spawn-succeeded-but-open-failed half-state), and the
// child has no slug-discovery to do on its way to the agent.
//
// Validation failures re-render the form with an ErrorBanner so the
// operator can correct without retyping.
func (s *Server) handleNewRunSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	slug := strings.TrimSpace(r.FormValue("slug"))
	wsName := strings.TrimSpace(r.FormValue("workspace"))
	agentName := strings.TrimSpace(r.FormValue("agent"))

	if !projectIDPattern.MatchString(projectID) {
		s.renderFormError(w, r, "project: invalid id")
		return
	}
	if !slugPattern.MatchString(slug) {
		s.renderFormError(w, r, "slug: must be kebab-case (lowercase, digits, hyphens; start with letter/digit)")
		return
	}
	if wsName != "" {
		if err := workspace.ValidateName(wsName); err != nil {
			s.renderFormError(w, r, "workspace: "+err.Error())
			return
		}
	}
	// Agent validity is checked by runopen.Open via run.New; we trust
	// the hardcoded dropdown set here.

	md, err := runopen.Open(s.opts.Root, projectID, run.Options{
		ID:        slug,
		Workflow:  promotedWorkflow,
		Workspace: wsName,
		Agent:     agentName,
	})
	if err != nil {
		s.renderFormError(w, r, "open: "+err.Error())
		return
	}

	id := md.Project + "/" + md.ID
	args := []string{"sdlc", promotedFirstStage, id}
	if _, err := s.children.spawn(id, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.renderFormError(w, r, "spawn: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+md.Project+"/"+md.ID, http.StatusSeeOther)
}

func (s *Server) renderFormError(w http.ResponseWriter, r *http.Request, msg string) {
	vm, err := s.gatherNewRunVM()
	if err != nil {
		http.Error(w, msg+" (and form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "new.html", vm)
}

// runVM backs the per-run page (GET /run/{project}/{slug}). It is a
// static panel — no PTY tail, no chain-prompt buttons, no
// remote-controlled end-agent affordance — so the same shape covers
// both the live-parented and read-only render paths.
type runVM struct {
	ID          string
	Project     string
	Slug        string
	Started     string // human "Xm ago"; empty on the read-only path
	Status      string // "live" | "exited: …" | "exited cleanly" | run.Status
	Live        bool
	CanvasLinks []canvasLink
	// PromoteEnabled signals the page to render the promote-to-sdlc
	// form. Set when the loaded run is an in-progress idea.
	// Workspaces / Agents back the form's dropdowns.
	PromoteEnabled bool
	Workspaces     []workspaceOption
	Agents         []string
	// ErrorBanner is the form-error path for /promote re-renders.
	// Empty on the happy GET path.
	ErrorBanner string
}

type canvasLink struct {
	Stage   string
	URL     string // /run/<p>/<r>/canvas/<stage>
	ModTime string // human "Xm ago"
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

// buildReadOnlyRunVM constructs a runVM from on-disk state for a run
// not currently parented by this serve. For in-progress idea runs,
// the promote form fields are populated so the operator can launch
// the sdlc run inline.
func (s *Server) buildReadOnlyRunVM(projectID, slug, id string) (runVM, error) {
	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		return runVM{}, err
	}
	vm := runVM{
		ID:          id,
		Project:     projectID,
		Slug:        slug,
		Status:      md.Status,
		CanvasLinks: s.canvasLinks(projectID, slug, time.Now()),
	}
	if isPromotableIdea(md) {
		wsOpts, agents, err := s.gatherIdeaPromoteVM(projectID)
		if err != nil {
			// Don't fail the whole page over a workspace-listing
			// hiccup; just don't render the form. The operator
			// still has the SSH fallback.
			s.logf("promote form gather: %v", err)
		} else {
			vm.PromoteEnabled = true
			vm.Workspaces = wsOpts
			vm.Agents = agents
		}
	}
	return vm, nil
}

// isPromotableIdea reports whether the loaded run is an in-progress
// idea — the gate for offering the promote-to-sdlc affordance.
func isPromotableIdea(md *run.Metadata) bool {
	return md.Workflow == dash.IdeaWorkflow && md.Status == run.StatusInProgress
}

// buildRunVM assembles the per-run page from the live child's state
// and the on-disk canvas listing.
func (s *Server) buildRunVM(c *child, projectID, slug, id string) runVM {
	exited, exitErr, _ := c.snapshot()
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
			Stage:   stage,
			URL:     "/run/" + projectID + "/" + slug + "/canvas/" + stage,
			ModTime: dash.HumanAgo(now, st.ModTime()),
		})
	}
	return out
}

// gatherIdeaPromoteVM returns just the dropdown content the per-idea
// promote form needs: every named workspace this host knows about
// (cross-project, mirroring /run/new) and the agent option set.
// Pulled from disk per request, same as gatherNewRunVM.
func (s *Server) gatherIdeaPromoteVM(_ string) ([]workspaceOption, []string, error) {
	infos, err := workspace.List(s.opts.Root, "")
	if err != nil {
		return nil, nil, err
	}
	wsOpts := make([]workspaceOption, 0, len(infos))
	for _, info := range infos {
		wsOpts = append(wsOpts, workspaceOption{
			Project: info.Project,
			Name:    info.Name,
			Label:   info.Project + "/" + info.Name,
		})
	}
	return wsOpts, agentOptions, nil
}

// handlePromote opens the destination sdlc run in-process by calling
// runopen.Promote, then spawns `moe sdlc design <p>/<newslug>` as a
// PTY-backed agent session and redirects to the new run's page.
// Opening synchronously means the destination's slug is known before
// the spawn — no placeholder id, no stdout regex, no rename race.
// Validation failures re-render the idea page with an inline error
// banner — same shape as the new-run form's renderFormError.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	wsName := strings.TrimSpace(r.FormValue("workspace"))
	agentName := strings.TrimSpace(r.FormValue("agent"))

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
	if wsName != "" {
		if err := workspace.ValidateName(wsName); err != nil {
			s.renderPromoteError(w, r, projectID, slug, id, "workspace: "+err.Error())
			return
		}
	}
	// Agent membership rides the hardcoded dropdown set.

	promoted, err := runopen.Promote(s.opts.Root, projectID, slug, runopen.PromoteOptions{
		Workflow:   promotedWorkflow,
		FirstStage: promotedFirstStage,
		Workspace:  wsName,
		Agent:      agentName,
	})
	if err != nil {
		s.renderPromoteError(w, r, projectID, slug, id, "promote: "+err.Error())
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
	args := []string{"sdlc", promotedFirstStage, destID}
	if _, err := s.children.spawn(destID, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.renderPromoteError(w, r, projectID, slug, id, "spawn: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+promoted.Run.Project+"/"+promoted.Run.ID, http.StatusSeeOther)
}

func (s *Server) renderPromoteError(w http.ResponseWriter, r *http.Request, projectID, slug, id, msg string) {
	vm, err := s.buildReadOnlyRunVM(projectID, slug, id)
	if err != nil {
		http.Error(w, msg+" (and run-page gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.render(w, r, "run.html", vm)
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

	return newRunVM{
		Projects:   projectIDs,
		Workspaces: wsOpts,
		Agents:     agentOptions,
	}, nil
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

// newIdeaVM backs the new-idea form. Projects are gathered from disk
// at request time; there are no workspace / agent dropdowns because
// idea runs don't host a PTY session and have no workspace binding.
type newIdeaVM struct {
	Projects    []string
	ErrorBanner string
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
	projectID := strings.TrimSpace(r.FormValue("project"))
	slug := strings.TrimSpace(r.FormValue("slug"))
	body := strings.ReplaceAll(r.FormValue("body"), "\r\n", "\n")

	if !projectIDPattern.MatchString(projectID) {
		s.renderIdeaFormError(w, r, "project: invalid id")
		return
	}
	if !slugPattern.MatchString(slug) {
		s.renderIdeaFormError(w, r, "slug: must be kebab-case (lowercase, digits, hyphens; start with letter/digit)")
		return
	}
	if body == "" {
		body = "# " + slug + "\n"
	}

	md, err := runopen.Open(s.opts.Root, projectID, run.Options{
		ID:       slug,
		Workflow: dash.IdeaWorkflow,
		SeedDocs: map[string]string{"idea": body},
	})
	if err != nil {
		s.renderIdeaFormError(w, r, "open: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+md.Project+"/"+md.ID, http.StatusSeeOther)
}

func (s *Server) renderIdeaFormError(w http.ResponseWriter, r *http.Request, msg string) {
	vm, err := s.gatherNewIdeaVM()
	if err != nil {
		http.Error(w, msg+" (and form gather failed: "+err.Error()+")", http.StatusInternalServerError)
		return
	}
	vm.ErrorBanner = msg
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
