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

// runVM backs the per-run page (GET /run/{project}/{slug}).
type runVM struct {
	ID              string
	Project         string
	Slug            string
	Started         string // human "Xm ago"
	Status          string // "live" | "exited (err)" | "exited (ok)"
	Live            bool
	Parented        bool   // serve has a live PTY child for this run (governs the activity section)
	Tail            string // PTY stdout, stripped to plain text
	CanvasLinks     []canvasLink
	Buttons         []promptButton // chain-prompt buttons when one is active
	EndAgentEnabled bool           // render the "end agent" button (true while live)
}

// promptButton is one renderable button for the per-run page. Key
// is what gets POSTed to /key; Label is what the operator sees;
// Class lets the CSS color-code by intent (benign / accent / warn).
type promptButton struct {
	Key   string // "Y", "n", "!", "!!", ...
	Label string // typically same as Key but readable
	Class string // "benign" | "accent" | "warn"
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
// not currently parented by this serve. The result has no Tail /
// Buttons / EndAgentEnabled — the template's {{if}} gates collapse
// those sections, so the page renders as a static snapshot.
func (s *Server) buildReadOnlyRunVM(projectID, slug, id string) (runVM, error) {
	md, err := run.Load(s.opts.Root, projectID, slug)
	if err != nil {
		return runVM{}, err
	}
	return runVM{
		ID:          id,
		Project:     projectID,
		Slug:        slug,
		Status:      md.Status,
		CanvasLinks: s.canvasLinks(projectID, slug, time.Now()),
	}, nil
}

// handleEndAgent writes two \x04 (EOT) bytes ~100ms apart to the
// child's PTY. Soft EOFs: claude / codex see EOF on stdin, flush,
// and exit cleanly. Always sends two — claude needs them, codex
// no-ops the second. The 303 redirect lands back on the run page,
// which re-renders with the chain prompt the agent emitted on exit.
func (s *Server) handleEndAgent(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	c, ok := s.children.get(id)
	if !ok {
		http.Error(w, "run "+id+" not live in this serve", http.StatusNotFound)
		return
	}
	if _, _, exited, _, _ := c.snapshot(); exited {
		http.Error(w, "run already exited", http.StatusConflict)
		return
	}
	if err := c.writeRaw([]byte{0x04}); err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	time.Sleep(endAgentEotGap)
	if err := c.writeRaw([]byte{0x04}); err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// buildRunVM assembles the per-run page from the live child's state
// and the on-disk canvas listing.
func (s *Server) buildRunVM(c *child, projectID, slug, id string) runVM {
	tail, prompt, exited, exitErr, _ := c.snapshot()
	now := time.Now()
	vm := runVM{
		ID:              id,
		Project:         projectID,
		Slug:            slug,
		Started:         dash.HumanAgo(now, c.started),
		Tail:            sanitizePTYTail(string(tail)),
		Live:            !exited,
		Parented:        true,
		EndAgentEnabled: !exited,
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
	if prompt.Active {
		vm.Buttons = buttonsFor(prompt.Options)
	}
	return vm
}

// handleRunKey writes one chain-prompt answer (single byte or "!!")
// to the child's PTY stdin, then 303-redirects back to the run page.
// Validates that the requested key is in the currently-active prompt
// option set so a stale POST can't push an unsolicited byte into the
// child.
func (s *Server) handleRunKey(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	c, ok := s.children.get(id)
	if !ok {
		http.Error(w, "run "+id+" not live in this serve", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	_, prompt, exited, _, _ := c.snapshot()
	if exited {
		http.Error(w, "run already exited", http.StatusConflict)
		return
	}
	if !prompt.Active {
		http.Error(w, "no active chain prompt; refresh", http.StatusConflict)
		return
	}
	if !keyAllowed(key, prompt.Options) {
		http.Error(w, "key "+key+" not in current option set "+prompt.Options, http.StatusBadRequest)
		return
	}

	if err := c.writeKeys(key); err != nil {
		http.Error(w, "write: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/run/"+projectID+"/"+slug, http.StatusSeeOther)
}

// keyAllowed checks that key is admissible given the live prompt's
// option set. Single-char keys must appear verbatim in options; the
// "!!" multi-char cascade is permitted whenever "!" is in options,
// since they ride the same dispatcher.
func keyAllowed(key, options string) bool {
	if key == "!!" {
		return strings.Contains(options, "!")
	}
	if len(key) != 1 {
		return false
	}
	return strings.IndexByte(options, key[0]) >= 0
}

// buttonsFor maps an option string to renderable buttons. Keeps the
// always-visible cascade extra (!!) right after the single ! so the
// row reads left-to-right as "more aggressive". Class assignments
// follow the design's color rule: Y/!/N benign, !! accent, x warn.
func buttonsFor(options string) []promptButton {
	out := make([]promptButton, 0, len(options)+1)
	for i := 0; i < len(options); i++ {
		k := string(options[i])
		out = append(out, promptButton{
			Key:   k,
			Label: k,
			Class: buttonClass(k),
		})
		if k == "!" {
			out = append(out, promptButton{
				Key:   "!!",
				Label: "!!",
				Class: "accent",
			})
		}
	}
	return out
}

func buttonClass(key string) string {
	switch key {
	case "x":
		return "warn"
	case "!":
		return "benign"
	default:
		return "benign"
	}
}

// canvasLinks enumerates the run's stage canvas files (under
// projects/<p>/runs/<r>/documents/*/content.md) with their mtimes.
// Only stages whose content.md actually exists are surfaced.
//
// Ordering: when Options.RunStages is wired, the result follows the
// workflow's ladder order so `design → code → test → push` reads
// left-to-right; otherwise the result is alphabetical. Any disk
// stages not in the ladder are appended alphabetically — a stale
// stage directory shouldn't disappear from the page just because
// the workflow has moved on.
func (s *Server) canvasLinks(projectID, slug string, now time.Time) []canvasLink {
	docsDir := filepath.Join(s.opts.Root, "projects", projectID, "runs", slug, "documents")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		return nil
	}
	onDisk := map[string]time.Time{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := os.Stat(filepath.Join(docsDir, e.Name(), "content.md"))
		if err != nil {
			continue
		}
		onDisk[e.Name()] = st.ModTime()
	}

	stageOrder := s.stageOrder(projectID, slug, onDisk)
	out := make([]canvasLink, 0, len(stageOrder))
	for _, stage := range stageOrder {
		out = append(out, canvasLink{
			Stage:   stage,
			URL:     "/run/" + projectID + "/" + slug + "/canvas/" + stage,
			ModTime: dash.HumanAgo(now, onDisk[stage]),
		})
	}
	return out
}

// stageOrder returns the order in which canvas links should render.
// Ladder order first (filtered to stages that exist on disk), then
// any unknown stages alphabetically.
func (s *Server) stageOrder(projectID, slug string, onDisk map[string]time.Time) []string {
	var ladder []string
	if s.opts.RunStages != nil {
		if l, err := s.opts.RunStages(projectID, slug); err == nil {
			ladder = l
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(onDisk))
	for _, stage := range ladder {
		if _, ok := onDisk[stage]; ok {
			out = append(out, stage)
			seen[stage] = true
		}
	}
	var extras []string
	for stage := range onDisk {
		if !seen[stage] {
			extras = append(extras, stage)
		}
	}
	sort.Strings(extras)
	return append(out, extras...)
}

// CSI: ESC [ <params> <intermediates> <final>. Params are
// 0x30-0x3f (digits, ; : < = > ?), intermediates are 0x20-0x2f
// (space ! " # $ % & ' ( ) * + , - . /), finals are 0x40-0x7e.
// Covers SGR (m), cursor moves (A-H), erase (J/K), private modes
// (?25l/h), and anything else following the ECMA-48 CSI grammar.
var ansiCSI = regexp.MustCompile(`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`)

// OSC: ESC ] <text> (BEL | ESC \). Matches both terminators so
// title-set sequences emitted by either flavor of terminal are
// stripped.
var ansiOSC = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// Simple ESC: ESC <intermediates>? <final>. Charset switches,
// cursor save/restore, DEC line-drawing ops. Applied *after* CSI
// and OSC so we don't accidentally consume their `[` / `]`
// introducers.
var ansiSimpleEsc = regexp.MustCompile(`\x1b[\x20-\x2f]*[\x30-\x7e]`)

// sanitizePTYTail turns the captured PTY ring buffer into something
// readable inside a <pre>. This is not a terminal emulator — any
// agent that drives the screen with cursor-up-and-rewrite multi-line
// regions will still leak orphan fragments. The goal is to flatten
// the single-line-spinner / clear-line / carriage-return overwrite
// patterns claude and codex actually produce, so the activity log
// stops showing "174m*oz / * / Ii" empty-line junk.
//
// Order matters:
//
//  1. CSI and OSC (they share an introducer with the simple-ESC pass).
//  2. The simple-ESC pass — what remains is non-CSI / non-OSC ESC.
//  3. BEL and BS stripped wholesale.
//  4. \r semantics applied per line: split on \n, and for each line
//     keep only the substring after the *last* \r. Collapses
//     spinner-overwrite trails like "Loading...\rDone." → "Done."
//  5. Runs of blank lines collapsed to at most one. The redraws
//     agents do leave behind a lot of vertical whitespace.
func sanitizePTYTail(s string) string {
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiSimpleEsc.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x07", "")
	s = strings.ReplaceAll(s, "\x08", "")

	lines := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s))
	blank := false
	for i, line := range lines {
		if idx := strings.LastIndexByte(line, '\r'); idx >= 0 {
			line = line[idx+1:]
		}
		isBlank := strings.TrimSpace(line) == ""
		if isBlank && blank {
			continue
		}
		blank = isBlank
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
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
