package serve

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
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
	Tail            string // PTY stdout, stripped to plain text
	CanvasLinks     []canvasLink
	Buttons         []promptButton // chain-prompt buttons when one is active
	EndAgentEnabled bool           // render the "end agent" button (true while live)
	PollStop        bool           // tell the client-side poller to stop (exited + grace elapsed)
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
	ModTime string // human "Xm ago"
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	c, ok := s.children.get(id)
	if !ok {
		http.Error(w, "run "+id+" is not parented by this serve process", http.StatusNotFound)
		return
	}
	vm := s.buildRunVM(c, projectID, slug, id)
	s.render(w, r, "run.html", vm)
}

// handleRunFragment renders just the swapping subtree of the run
// page. The setInterval poller on run.html hits this endpoint every
// 2s and swaps its innerHTML into <div id="poll-target">. Returns
// 404 if the run isn't parented (the poller stops on 404).
func (s *Server) handleRunFragment(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	slug := r.PathValue("slug")
	id := projectID + "/" + slug

	c, ok := s.children.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	vm := s.buildRunVM(c, projectID, slug, id)
	s.render(w, r, "run_fragment.html", vm)
}

// handleEndAgent writes two \x04 (EOT) bytes ~100ms apart to the
// child's PTY. Soft EOFs: claude / codex see EOF on stdin, flush,
// and exit cleanly. Always sends two — claude needs them, codex
// no-ops the second. The chain prompt that follows surfaces on the
// next poller tick via the broadened chainPromptRegex.
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

// pollStopGrace is how long after a child exits the client-side
// poller keeps fetching. Long enough for the operator to read the
// final tail; short enough not to keep an abandoned tab polling
// forever.
const pollStopGrace = 30 * time.Second

// buildRunVM is shared between the full-page and fragment renderers
// so they're guaranteed to agree on what's visible.
func (s *Server) buildRunVM(c *child, projectID, slug, id string) runVM {
	tail, prompt, exited, exitErr, exitedAt := c.snapshot()
	now := time.Now()
	vm := runVM{
		ID:              id,
		Project:         projectID,
		Slug:            slug,
		Started:         dash.HumanAgo(now, c.started),
		Tail:            sanitizePTYTail(string(tail)),
		Live:            !exited,
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
	vm.CanvasLinks = canvasLinksFor(s.opts.Root, projectID, slug, now)
	if prompt.Active {
		vm.Buttons = buttonsFor(prompt.Options)
	}
	if exited && now.Sub(exitedAt) > pollStopGrace {
		vm.PollStop = true
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

// handleResume spawns `moe sdlc resume <project>/<slug>` as a PTY
// child so a run started outside serve (e.g. in the operator's
// laptop tmux) becomes button-controllable from the phone. The
// route is workflow-agnostic at this layer — moe surfaces a clear
// error in the activity log if the run isn't sdlc-shaped.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project"))
	slug := strings.TrimSpace(r.FormValue("slug"))
	if !projectIDPattern.MatchString(projectID) {
		http.Error(w, "project: invalid id", http.StatusBadRequest)
		return
	}
	if !slugPattern.MatchString(slug) {
		http.Error(w, "slug: invalid", http.StatusBadRequest)
		return
	}

	id := projectID + "/" + slug
	args := []string{"sdlc", "resume", id}
	if _, err := s.children.spawn(id, s.opts.MoeBin, args, s.opts.Root, s.opts.Logger); err != nil {
		http.Error(w, "spawn: "+err.Error(), http.StatusConflict)
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

// canvasLinksFor enumerates the run's stage canvas files (under
// projects/<p>/runs/<r>/documents/*/content.md) with their mtimes.
// Used by the run page so the operator can see at a glance which
// stages have content and how fresh it is.
func canvasLinksFor(root, projectID, slug string, now time.Time) []canvasLink {
	docsDir := filepath.Join(root, "projects", projectID, "runs", slug, "documents")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		return nil
	}
	var out []canvasLink
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		canvas := filepath.Join(docsDir, e.Name(), "content.md")
		st, err := os.Stat(canvas)
		if err != nil {
			continue
		}
		out = append(out, canvasLink{
			Stage:   e.Name(),
			ModTime: dash.HumanAgo(now, st.ModTime()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
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
