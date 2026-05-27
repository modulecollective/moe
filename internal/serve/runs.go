package serve

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// promotedWorkflow names the workflow a Promote action opens the
// idea into. Hardcoded to sdlc per design: the seed request says
// "sdlc run", and `--from-idea` is workflow-agnostic at the CLI
// but the dominant case here is sdlc.
const promotedWorkflow = "sdlc"

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

// handleNewRunSubmit validates the form, builds the `moe sdlc new`
// argv, spawns the child as a PTY-backed run, and redirects to the
// per-run page. Validation failures re-render the form with an
// ErrorBanner so the operator can correct without retyping.
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
	// Agent validity is checked by `moe sdlc new`; we trust the
	// hardcoded dropdown set here.

	args := []string{"sdlc", "new"}
	if wsName != "" {
		args = append(args, "--workspace", wsName)
	}
	if agentName != "" {
		args = append(args, "--agent", agentName)
	}
	args = append(args, projectID+"/"+slug)

	id := projectID + "/" + slug
	if _, err := s.children.spawn(id, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.renderFormError(w, r, "spawn: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
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
	Stage         string
	URL           string // /run/<p>/<r>/canvas/<stage>
	ModTime       string // human "Xm ago"
	TranscriptURL string // /static/transcripts/... when a snagged jsonl exists; empty otherwise
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
	_, exited, exitErr, _ := c.snapshot()
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
//
// Each link also picks up a TranscriptURL pointing at the most-recent
// snagged claude-code session JSONL under the canonical
// `documents/<stage>/transcripts/` dir (left to point at the on-disk
// path the operator can pull via SSH; serve doesn't host the JSONL
// itself).
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
		link := canvasLink{
			Stage:   stage,
			URL:     "/run/" + projectID + "/" + slug + "/canvas/" + stage,
			ModTime: dash.HumanAgo(now, st.ModTime()),
		}
		if hasSnaggedTranscript(s.opts.Root, projectID, slug, stage) {
			link.TranscriptURL = transcriptOnDiskPath(s.opts.Root, projectID, slug, stage)
		}
		out = append(out, link)
	}
	return out
}

// hasSnaggedTranscript reports whether at least one *.jsonl lives
// under the canonical `documents/<stage>/transcripts/` dir for this
// run. Used to decide whether the per-run page renders a transcript
// affordance next to the stage's canvas link.
func hasSnaggedTranscript(root, projectID, slug, stage string) bool {
	dir := filepath.Join(root, "projects", projectID, "runs", slug,
		"documents", stage, "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// transcriptOnDiskPath returns the canonical on-disk transcripts
// dir for (project, run, stage). The per-run page surfaces this as
// a path string, not a download URL: serve doesn't host the JSONL
// itself, and the operator has shell access to pull it.
func transcriptOnDiskPath(root, projectID, slug, stage string) string {
	return filepath.Join(root, "projects", projectID, "runs", slug,
		"documents", stage, "transcripts")
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

// handlePromote spawns `moe sdlc new --from-idea <p>/<s>` as a PTY
// child and redirects back to the idea page. The child opens under
// a `<p>/<s>:promoting` placeholder id; the read-loop watcher
// renames it to `<p>/<newslug>` on the first `opened run …` line.
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

	args := []string{"sdlc", "new"}
	if wsName != "" {
		args = append(args, "--workspace", wsName)
	}
	if agentName != "" {
		args = append(args, "--agent", agentName)
	}
	args = append(args, "--from-idea", id)

	placeholder := id + promotingSuffix
	if _, err := s.children.spawn(placeholder, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		s.renderPromoteError(w, r, projectID, slug, id, "spawn: "+err.Error())
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
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
	mds, warns, err := project.List(s.opts.Root)
	if err != nil {
		return newRunVM{}, err
	}
	for _, w := range warns {
		s.logf("project list: skipping %s: %v", w.ID, w.Err)
	}
	projectIDs := make([]string, 0, len(mds))
	for _, md := range mds {
		projectIDs = append(projectIDs, md.ID)
	}
	sort.Strings(projectIDs)

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
