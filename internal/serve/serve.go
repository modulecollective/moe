// Package serve provides moe's HTTP front-end: a small server the
// operator can reach from a phone over the Tailnet to launch new runs,
// glance at the dash, and feed chain-prompt keys into in-flight runs.
//
// The server is the parent process of every run it launches, so it
// owns each run's TTY (via a PTY) and writes chain-prompt keys
// directly into the child's stdin without going through
// `tmux send-keys`. Runs started outside `moe serve` are read-only in
// the UI (they show on the dash but expose no buttons).
//
// Auth is network reach. The listener binds to 127.0.0.1 by default,
// so nothing off-box can reach it directly. Exposing it to the tailnet
// is the job of whatever sits in front — on the cloud-box that's a
// `tailscale serve` proxy at tailnet:443 → 127.0.0.1:4242, which is
// the thing that enforces "tailnet peers only." There is no token, no
// login form. Override with --addr to bind elsewhere (for example,
// --addr <tailnet-ip> on a kernel-mode tailscale host).
//
// Because reach is the only gate, the spawn bucket is opt-in. Several
// POST routes run `moe <wf> <stage>` agent subprocesses (i.e. arbitrary
// code), so by default — safe mode — they refuse with 403 and the UI
// doesn't offer them: only idea capture, run close/edit/reopen, and the
// read-only views work. Options.Insecure (the --insecure flag or a
// non-empty MOE_SERVE_INSECURE) re-enables the whole spawn bucket,
// trading the safe default for serve's phone-facing "launch a run"
// feature. That's an acknowledged choice: anything that can reach the
// listener can then execute code.
package serve

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
)

// DefaultPort is the listener port when --port isn't set.
const DefaultPort = 4242

// Options configures a Server.
type Options struct {
	// Addr overrides the listener address. Accepts "host" or
	// "host:port". Empty means 127.0.0.1 with Port.
	Addr string

	// Port is the listener port. Ignored when Addr already includes
	// one. Defaults to DefaultPort.
	Port int

	// Root is the bureaucracy root (the directory containing
	// projects/, sessions.json, etc.). Required.
	Root string

	// MoeBin is the path to the `moe` executable invoked to spawn
	// runs. Defaults to "moe" (PATH lookup).
	MoeBin string

	// Logger receives one line per HTTP request and lifecycle events.
	// nil discards.
	Logger io.Writer

	// GatherDash returns the dash data a route renders, scoped to a
	// project: an empty projectID gathers every run (the home dash); a
	// non-empty one scopes the rows, factory art, and histogram to that
	// project (the project hub). The ?all= query flag is a render-time
	// concern (it lifts the COMPLETED cap in newDashVM), not a gather
	// input. The cli/serve.go entry point wires this to
	// cli.GatherDashSnapshot so serve itself doesn't depend on the
	// workflow registry.
	//
	// Required by the dash route; absent means GET / returns 500.
	// histogram is the trailing-HistDays daily run-activity window
	// (oldest→newest) the dash charts above the factory art.
	GatherDash func(projectID string) (rows []dash.Row, projectCount, activeProjects int, histogram []int, err error)

	// ResolveCanvas returns the absolute filesystem path serve should
	// open for (project, run, stage). The cli wrapper closes over the
	// bureaucracy root and looks up the run's workflow internally so
	// serve stays workflow-registry-free. Required by the canvas
	// route; absent means GET .../canvas/... returns 500. Any error it
	// returns maps to 404 — resolution is a path computation, not a
	// file stat. A missing canvas file is detected later by the
	// handler's ReadFile (ErrNotExist renders the 200 empty-state).
	ResolveCanvas func(project, run, stage string) (path string, err error)

	// RunStages returns the workflow ladder order for an on-disk run.
	// canvasLinks walks this ladder and asks ResolveCanvas for each
	// stage in order — so this drives both ordering *and* which
	// stages get a link at all. Absent means no canvas links on the
	// per-run page.
	RunStages func(project, run string) (stages []string, err error)

	// GatherRunRow returns the dash.Row for one run — the same shape
	// the dash renders. The per-run page uses it to surface the
	// dash-row note (workflow:stage, workspace marker, open-session
	// marker, "· close?" hint, etc.) and the When timestamp that
	// matches what the operator just saw on the dash. ok=false means
	// the row was filtered out (or no such run); the per-run page
	// falls back to its older started/status line in that case.
	//
	// Absent means the per-run page renders the fallback meta line on
	// every hit — no row data is fatal, just less informative.
	GatherRunRow func(project, run string) (row dash.Row, ok bool, err error)

	// ChainMembers returns the live batch hanging off a chain head: one
	// dash.Row per member in head→tail order — the runs `moe chain kick`
	// would actually ride — plus the qualified key of a live parent the
	// head is itself chained under ("" when it heads its own chain).
	//
	// The head's canvas is the operator's purpose note and says nothing
	// about membership; this is where the head page gets the batch. The
	// second return is the kick chips' gate: a head chained under a live
	// parent is one `moe chain kick` refuses ("kick the head"), so the
	// page must not offer it.
	//
	// Only called for chain-workflow runs. Absent — or erroring — leaves
	// the head page as it was: no members section, no kick chips.
	ChainMembers func(project, run string) (members []dash.Row, chainedUnder string, err error)

	// Insecure enables the spawn bucket — the POST routes that run
	// `moe <wf> <stage>` agent subprocesses (new run, promote,
	// advance/ship/chain, chain kick, chore open). Off by default (safe
	// mode): those routes refuse with 403 and their UI entry points
	// don't render; the journal-write and read-only routes are
	// unaffected. The cli wrapper sets this from the --insecure flag or
	// a non-empty $MOE_SERVE_INSECURE.
	Insecure bool

	// NotifyURL is the webhook URL we POST a small JSON payload to
	// when a serve-parented run exits. Empty disables notifications.
	// The cli wrapper populates this from $MOE_SERVE_NOTIFY_URL.
	NotifyURL string

	// CloseRun closes an in-progress non-idea run in-process:
	// the full cli close pipeline — workspace release, follow-up/lore
	// harvest, status flip, trailered commit — run with --no-edit
	// semantics. cli/serve.go wires this to the cli close core so the
	// serve package stays free of the workflow registry and the
	// cli-resident teardown helpers.
	//
	// Returns *runopen.NotClosableError when the run's state forbids a
	// close (pushed, already terminal, wrong workflow); the close route
	// maps that to 409 and anything else to 500. Absent means the close
	// route returns 500 for non-idea runs (idea closes don't need it).
	CloseRun func(project, run string) error

	// GatherChore returns the computed state of one chore for the chore
	// detail page. ok=false means no chore by that project/name. The cli
	// wrapper closes this over gatherChoreStates so serve stays free of
	// the workflow registry. Absent means the chore page returns 500.
	GatherChore func(project, name string) (state chore.State, ok bool, err error)

	// OpenChore opens the chore's configured-workflow run in-process
	// (mirroring `moe chore open`) and returns the destination run's
	// identity plus the workflow + first stage serve must spawn to host
	// it. Guard failures come back wrapping ErrChoreNotFound (→ 404) or
	// ErrChoreNotOpenable (→ 409); cli/serve.go translates its internal
	// guard errors into those so serve needn't import the cli package.
	// Absent means the open route returns 500.
	OpenChore func(project, name string) (dest ChoreOpen, err error)

	// WorkflowUI returns the serve declaration a workflow made at
	// registration time — which stage verbs serve may spawn, whether
	// the cascade trio (advance/ship/chain) applies, whether a close
	// pipeline exists. ok=false means the workflow declared nothing:
	// its runs render read-only (canvas links, no chips) and the
	// stage/advance routes refuse. cli/serve.go wires this to the
	// cli-side registry so serve carries no per-workflow UI policy of
	// its own. Absent means the advance/ship/chain and stage-spawn
	// routes return 500; idea chips (bespoke, not stage-derived) still
	// render.
	WorkflowUI func(workflow string) (ui WorkflowUI, ok bool)

	// NewRunWorkflows lists the workflows the /run/new and promote
	// forms offer, in display order; the first entry is the default
	// selection. Computed once from the cli-side registry at serve
	// start (the registry is init-time static). Empty hides the
	// workflow selector and fails any new-run/promote POST.
	NewRunWorkflows []NewRunWorkflow
}

// WorkflowUI is one workflow's declared web affordances, composed
// cli-side from the workflow registries (see Options.WorkflowUI).
type WorkflowUI struct {
	// Stages are the stage verbs serve may spawn for this workflow, in
	// ladder order. For cascade workflows the run's next stage must be
	// in this set for the advance trio to render/spawn.
	Stages []string
	// Cascade reports that the workflow's stage verbs accept --ship /
	// --chain — the advance/ship/chain routes and chips apply.
	Cascade bool
	// Perpetual reports that satisfying every stage does not make close
	// the routine next move; the run stays open for repeat sittings.
	Perpetual bool
	// Close reports that the workflow registered the shared close
	// pipeline. Per-run pages use Perpetual to decide whether close is
	// a routine idle-page chip or only a live-child lifecycle affordance.
	Close bool
}

// NewRunWorkflow is one entry in the new-run/promote forms' workflow
// selector.
type NewRunWorkflow struct {
	Name string
	// FirstStage is the stage verb serve spawns right after opening a
	// run in this workflow (the registry's first ladder stage).
	FirstStage string
	// Workspace reports whether the workflow accepts a workspace
	// binding (the CLI's "only sdlc and hooks accept --workspace"
	// rule); the form rejects a workspace selection otherwise.
	Workspace bool
}

// Server owns the HTTP listener and the registry of live PTY
// children.
type Server struct {
	opts     Options
	addr     string
	tmpl     *template.Template
	router   *http.ServeMux
	children *children
}

//go:embed templates/*.html static/*
var assets embed.FS

// New parses templates, resolves the listener address, and registers
// routes. It does not start listening; call ListenAndServe.
func New(opts Options) (*Server, error) {
	if opts.Root == "" {
		return nil, errors.New("serve: Root is required")
	}
	if opts.MoeBin == "" {
		opts.MoeBin = "moe"
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}

	addr := resolveAddr(opts.Addr, opts.Port)

	tmpl, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("serve: parse templates: %w", err)
	}

	s := &Server{
		opts:     opts,
		addr:     addr,
		tmpl:     tmpl,
		router:   http.NewServeMux(),
		children: newChildren(),
	}
	if opts.NotifyURL != "" {
		s.children.notify = makeNotifier(opts.NotifyURL, opts.Logger)
	}
	s.registerRoutes()
	return s, nil
}

// Addr returns the resolved "ip:port" the server binds to.
func (s *Server) Addr() string { return s.addr }

// Handler returns the wired-up http.Handler. Exposed so tests can
// drive routes through httptest without binding a real listener.
func (s *Server) Handler() http.Handler { return s.router }

// ListenAndServe binds, serves, and blocks until ctx is cancelled. On
// shutdown it drains in-flight requests for up to 5 seconds. Children
// spawned by handlers die with the server (SIGHUP on PTY close —
// wired in the per-run slice).
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s.router,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("serve: listen %s: %w", s.addr, err)
	}
	s.logf("listening on http://%s/", s.addr)
	if s.opts.Insecure {
		s.logf("INSECURE: run-spawning enabled; anything that can reach http://%s/ can execute code", s.addr)
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer httpCancel()
		if err := srv.Shutdown(httpCtx); err != nil {
			s.logf("shutdown: %v", err)
		}
		// Children get their own budget — the four-phase wind-down
		// in children.shutdown can run up to shutdownSoftGrace +
		// shutdownHangupGrace + shutdownIntrGap. Add a small buffer
		// so the inner phases see the deadline as theirs, not the
		// context's.
		childCtx, childCancel := context.WithTimeout(context.Background(),
			shutdownSoftGrace+shutdownHangupGrace+2*time.Second)
		defer childCancel()
		s.children.shutdown(childCtx, s.opts.Logger)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func (s *Server) registerRoutes() {
	s.router.HandleFunc("/", s.handleDash)
	s.router.HandleFunc("/run/new", s.handleNewRun)
	s.router.HandleFunc("/idea/new", s.handleNewIdea)
	// Per-run page. Uses Go 1.22+ pattern wildcards so the project
	// and slug fall out of the URL without manual splitting.
	s.router.HandleFunc("GET /run/{project}/{slug}", s.handleRunPage)
	s.router.HandleFunc("GET /run/{project}/{slug}/canvas/{stage}", s.handleCanvas)
	// Read-only agent-transcript viewer for a stage. Same safe-mode
	// bucket as the canvas route; ?agent / ?before / ?fragment are the
	// backend pick, the paging cursor, and the load-earlier fetch form.
	s.router.HandleFunc("GET /run/{project}/{slug}/transcript/{stage}", s.handleTranscript)
	s.router.HandleFunc("GET /run/{project}/{slug}/promote", s.handlePromoteForm)
	s.router.HandleFunc("POST /run/{project}/{slug}/promote", s.handlePromote)
	s.router.HandleFunc("GET /run/{project}/{slug}/edit", s.handleCaptureEditForm)
	s.router.HandleFunc("POST /run/{project}/{slug}/edit", s.handleCaptureEditSubmit)
	s.router.HandleFunc("POST /run/{project}/{slug}/close", s.handleClose)
	s.router.HandleFunc("POST /run/{project}/{slug}/reopen", s.handleIdeaReopen)
	// Stage advancement for in-progress cascade-workflow runs:
	// /advance spawns the next stage interactively under the serve
	// handshake; /ship spawns it under --ship (headless cascade through
	// push, ship this run); /chain spawns it under --chain (ship this
	// run, then ride the whole chain).
	s.router.HandleFunc("POST /run/{project}/{slug}/advance", s.handleAdvance)
	s.router.HandleFunc("POST /run/{project}/{slug}/ship", s.handleShip)
	s.router.HandleFunc("POST /run/{project}/{slug}/chain", s.handleChain)
	s.router.HandleFunc("POST /run/{project}/{slug}/kick", s.handleKick)
	s.router.HandleFunc("POST /run/{project}/{slug}/kick-dynamic", s.handleKickDynamic)
	// Chore detail page + open action. A chore isn't a run, so it has
	// its own /chore namespace; "open" mints a fresh run of the chore's
	// configured workflow (the analog of promoting an idea).
	s.router.HandleFunc("GET /chore/{project}/{name}", s.handleChorePage)
	s.router.HandleFunc("POST /chore/{project}/{name}/open", s.handleChoreOpen)

	// Read-only browsing of the bureaucracy's durable content: lore,
	// projects, per-project knowledge and digital-twin docs. All render
	// from os.ReadFile + the internal/md renderer; none touch the spawn
	// bucket, so they work in safe mode exactly like the dash and canvas.
	s.router.HandleFunc("GET /lore", s.handleLoreIndex)
	s.router.HandleFunc("GET /lore/{name}", s.handleLoreEntry)
	s.router.HandleFunc("GET /projects", s.handleProjectsIndex)
	s.router.HandleFunc("GET /projects/{project}", s.handleProjectHub)
	s.router.HandleFunc("GET /projects/{project}/knowledge", s.handleKnowledge)
	s.router.HandleFunc("GET /projects/{project}/knowledge/{topic}", s.handleKnowledgeTopic)
	s.router.HandleFunc("GET /projects/{project}/twin/{doc}", s.handleTwinDoc)

	// Static assets are embedded under static/; strip the URL prefix
	// so /static/style.css maps to embedded static/style.css.
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		// Embedded path is a constant of the build; a Sub failure
		// here would mean the //go:embed directive went wrong.
		panic("serve: sub static FS: " + err.Error())
	}
	s.router.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}

// handleNewRun dispatches GET (form render) vs POST (spawn child)
// on the single /run/new path.
func (s *Server) handleNewRun(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleNewRunForm(w, r)
	case http.MethodPost:
		s.handleNewRunSubmit(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleNewIdea dispatches GET (form render) vs POST (open idea run)
// on the single /idea/new path. No PTY spawn — idea runs are a single
// canvas with no live agent.
func (s *Server) handleNewIdea(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleNewIdeaForm(w, r)
	case http.MethodPost:
		s.handleNewIdeaSubmit(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDash renders the home page. Pulls dash data through the
// Options.GatherDash callback so this package stays workflow-
// registry-free.
func (s *Server) handleDash(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.opts.GatherDash == nil {
		http.Error(w, "dash not configured (Options.GatherDash is nil)", http.StatusInternalServerError)
		return
	}
	showAll := r.URL.Query().Get("all") != ""
	rows, projectCount, activeProjects, histogram, err := s.opts.GatherDash("")
	if err != nil {
		s.logf("dash gather: %v", err)
		http.Error(w, "dash error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	vm := newDashVM(time.Now().UTC(), rows, projectCount, activeProjects, histogram, showAll)
	// Mark which active rows are currently parented by serve so the
	// dash can render a "live" badge. Registry presence isn't enough:
	// natural exit leaves *child in cs.all (only the respawn path
	// deletes), so c.snapshot() gates on the exited flag the same way
	// buildRunVM does for the per-run page.
	for i := range vm.Active {
		id := vm.Active[i].Project + "/" + vm.Active[i].Run
		if c, ok := s.children.get(id); ok {
			if exited, _ := c.snapshot(); !exited {
				vm.Active[i].Live = true
			}
		}
	}
	s.render(w, r, "dash.html", vm)
}

// render runs a named template with data and surfaces template
// errors via the logger. Template errors return 500 only when the
// header hasn't been written yet; once bytes are on the wire a
// partial render is the lesser evil.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logf("template %s: %v", name, err)
	}
	s.logf("%s %s", r.Method, r.URL.Path)
}

// spawnAllowed gates the spawn bucket — the POST routes that run agent
// subprocesses. In safe mode (the default) it writes a 403 and returns
// false; with Insecure set it returns true and the handler proceeds.
// The four spawn handlers call it first thing. The matching UI gating
// (so safe mode never offers a route it would refuse) lives in the view
// models, not here.
func (s *Server) spawnAllowed(w http.ResponseWriter) bool {
	if s.opts.Insecure {
		return true
	}
	http.Error(w,
		"serve is in safe mode; restart with --insecure (or set MOE_SERVE_INSECURE) to enable run-spawning actions",
		http.StatusForbidden)
	return false
}

func (s *Server) logf(format string, a ...any) {
	if s.opts.Logger == nil {
		return
	}
	fmt.Fprintf(s.opts.Logger, format+"\n", a...)
}

// syncWriter is the io.Writer handed to runopen's journal write-edge
// (auto-push progress and warnings). Same sink as logf; never nil
// because the push helpers write unconditionally.
func (s *Server) syncWriter() io.Writer {
	if s.opts.Logger == nil {
		return io.Discard
	}
	return s.opts.Logger
}

// resolveAddr returns "ip:port". When override is empty the listener
// binds to loopback; the proxy in front is what enforces reach. When
// override includes a port, that port wins.
func resolveAddr(override string, port int) string {
	if override == "" {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if _, _, err := net.SplitHostPort(override); err == nil {
		return override
	}
	return net.JoinHostPort(override, strconv.Itoa(port))
}
