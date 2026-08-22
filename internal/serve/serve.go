// Package serve provides moe's HTTP front-end: a small server the
// operator can reach from a phone over the Tailnet to glance at the
// dash, read canvases and transcripts, and hand the machine consent.
//
// The web starts nothing. Every POST it serves writes a journal commit
// and stops: capture or edit an idea, tag it for a workflow, close or
// reopen a run, mark the current stage advanced. Starting agents is the
// resident heartbeat's job alone (heartbeat.go), which is what makes
// those two halves compose — the web writes licences, the clock spends
// them. It also means there is no path from the listener to code
// execution, armed or not.
//
// The server is still the parent process of every sweep the heartbeat
// starts, so it owns each child's TTY (via a PTY) and can show its
// output tail. Nothing else in the process spawns.
//
// Auth is network reach. The listener binds to 127.0.0.1 by default,
// so nothing off-box can reach it directly. Exposing it to the tailnet
// is the job of whatever sits in front — on the cloud-box that's a
// `tailscale serve` proxy at tailnet:443 → 127.0.0.1:4242, which is
// the thing that enforces "tailnet peers only." There is no token, no
// login form. Override with --addr to bind elsewhere (for example,
// --addr <tailnet-ip> on a kernel-mode tailscale host).
//
// Because reach is the only gate, motion is opt-in. Options.Dynamic
// (the --dynamic flag or a non-empty MOE_SERVE_DYNAMIC) arms the
// process, and it licenses exactly one thing: the resident heartbeat, a
// per-project ticker that sweeps a project on its own clock when the
// board warrants it. Running the armed process *is* the consent act —
// the legible replacement for installing a crontab. Stopping it
// retracts. Unarmed, every route still works and nothing ever acts on
// what they wrote.
//
// Under that switch, each project carries its own cap on the clock —
// `moe project mode <id> paused|safe|auto`, stored in project.json and
// read by the gate and the sweep child. Arming stays above it: an
// unarmed serve automates nothing anywhere, whatever any project's mode
// says.
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
	"os"
	"strconv"
	"sync"
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
	// would actually ride.
	//
	// The head's canvas is the operator's purpose note and says nothing
	// about membership; this is where the head page gets the batch. The
	// page offers no kick of its own — a hand-staged head is a deliberate
	// staging fence, and staging is a terminal activity — so what it owes
	// the operator is an honest picture of what `moe chain kick` would
	// ride, nothing more.
	//
	// Only called for chain-workflow runs. Absent — or erroring — leaves
	// the head page as it was: no members section.
	ChainMembers func(project, run string) (members []dash.Row, err error)

	// RunProvenance returns the per-run page's provenance section: how
	// the run was opened and, walking up the MoE-Spawned-By chain, how
	// each spawner came to be. It lives on the cli side because the walk
	// reads the journal index *and* the spawning pulse's canvas gate for
	// the recorded reason — both cli-owned.
	//
	// Absent or erroring leaves the section off the page; provenance is
	// enrichment, not the reason the page exists.
	RunProvenance func(project, run string) (hops []ProvHop, err error)

	// GatherRunTraces returns what a run left behind outside its
	// canvases: its followups.md and feedback/lore.md checklist entries
	// (each harvested one resolved to the idea run or lore entry it
	// became) and its feedback/twin.md note with the reflect pass that
	// folded it in. The checklist grammar is unexported cli state and
	// serve can't import cli, so it crosses the seam as a callback
	// rather than by moving the parser.
	//
	// Absent — or erroring — leaves the run page as it was: no traces
	// sections. A broken trace file degrades its section, not the page.
	GatherRunTraces func(project, run string) (RunTraces, error)

	// Dynamic arms the heartbeat ticker (heartbeat.go). It is the whole
	// of what the dynamic consent rung gates here, because the heartbeat
	// is the only thing in this process that starts an agent: every route
	// the web serves either reads, or writes a journal commit a later
	// tick may act on.
	//
	// Off by default (unarmed): no tick ever fires, and the process is a
	// reader with a capture door. The cli wrapper sets this from the
	// --dynamic flag or a non-empty $MOE_SERVE_DYNAMIC.
	Dynamic bool

	// Heartbeat is the cli-side gate the resident ticker consults each
	// tick: which projects warrant a sweep, plus the reap of dead machine
	// sessions that runs ahead of it. Every question it answers needs the
	// journal, the chain graph or the workflow registry, so it crosses
	// the seam as a callback like everything else serve can't know.
	//
	// Absent — or a serve that isn't armed (see Dynamic) — means no
	// ticker at all: the process serves HTTP and nothing looks at the
	// board on its own.
	Heartbeat Heartbeat

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

	// WorkflowUI returns the serve declaration a workflow made at
	// registration time — which stages the web may mark advanced, and
	// whether a close pipeline exists. ok=false means the workflow
	// declared nothing:
	// its runs render read-only (canvas links, no chips) and the
	// stage/advance routes refuse. cli/serve.go wires this to the
	// cli-side registry so serve carries no per-workflow UI policy of
	// its own. Absent means the advance route returns 500; idea chips
	// (bespoke, not stage-derived) still render.
	WorkflowUI func(workflow string) (ui WorkflowUI, ok bool)

	// TagWorkflows lists the workflows an idea may be tagged for, in
	// display order; the first entry is what an untagged POST falls back
	// to. One "tag <workflow>" chip renders per entry, and the tag route
	// resolves against the same list, so the two surfaces can't disagree
	// on what a valid destination is. Computed once from the cli-side
	// registry at serve start (the registry is init-time static). Empty
	// leaves ideas untaggable from the web.
	TagWorkflows []string
}

// WorkflowUI is one workflow's declared web affordances, composed
// cli-side from the workflow registries (see Options.WorkflowUI).
type WorkflowUI struct {
	// Stages are the stage verbs this workflow's runs step through, in
	// ladder order, minus any the workflow excludes from the web. A run's
	// next stage must be in this set for the advance mark to render or
	// land — which is what keeps sdlc's push out: a marker on push would
	// satisfy the ladder without anything ever being pushed.
	Stages []string
	// Perpetual reports that satisfying every stage does not make close
	// the routine next move; the run stays open for repeat sittings.
	Perpetual bool
	// Close reports that the workflow registered the shared close
	// pipeline. Per-run pages use Perpetual to decide whether close is
	// a routine idle-page chip or only a live-child lifecycle affordance.
	Close bool
}

// Server owns the HTTP listener and the registry of live PTY
// children.
type Server struct {
	opts      Options
	addr      string
	tmpl      *template.Template
	router    *http.ServeMux
	children  *children
	heartbeat *heartbeat
	// activity is the runtime record both dashes read: process facts, the
	// heartbeat's last verdicts, and a bounded ring of what it has been
	// doing. See activity.go.
	activity *activity
	// stateMu serialises the state file. Held across snapshot-and-write in
	// saveActivity, and taken by dropActivity to set stopped. See
	// saveActivity for why both.
	stateMu sync.Mutex
	stopped bool
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
		opts:      opts,
		addr:      addr,
		tmpl:      tmpl,
		router:    http.NewServeMux(),
		children:  newChildren(),
		heartbeat: newHeartbeat(),
		activity:  newActivity(opts.Root, os.Getpid(), addr, opts.Dynamic, time.Now()),
	}
	if opts.NotifyURL != "" {
		s.children.notify = makeNotifier(opts.NotifyURL, opts.Logger)
	}
	s.children.onSpawn = func(id string, at time.Time) {
		s.activity.recordChildSpawn(id, at)
		s.saveActivity()
	}
	s.children.onExit = func(id string, at time.Time, exitErr error, tail string) {
		// A heartbeat sweep names the run it minted in its emit file. This
		// is where both are known at once, so it is where the exit event and
		// the project row learn it — and where the file goes. Recorded even
		// when empty: panel() may have lazily cached the slug mid-sweep
		// without checking the run was ever committed, and this read is the
		// check — a sweep whose run never landed takes its link back off
		// the row here.
		runID := ""
		if project := heartbeatProject(id); project != "" {
			runID = takeSweepRun(s.opts.Root, project)
			s.activity.recordSweepRun(project, runID)
		}
		s.activity.recordChildExit(id, at, exitErr, cleanTail(tail), runID)
		s.saveActivity()
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
	// The state file goes down as soon as the listener is up, so `moe
	// dash` can see an armed serve before its first tick — "up, nothing
	// swept yet" is a different answer from "no serve at all", and both
	// are worth telling apart.
	s.saveActivity()
	if s.opts.Dynamic {
		s.logf("DYNAMIC: armed — the heartbeat may start settled work, and anything that can reach http://%s/ can execute code", s.addr)
	}
	// The heartbeat lives inside the listener's lifetime, not beside it:
	// running the armed process *is* the standing consent, so the clock
	// starts when the process starts serving and stops when ctx cancels.
	// Its children ride the same registry, so shutdown winds them down
	// with everything else.
	// It runs off a derived context so both return paths below can stop
	// it and join it. The srv.Serve-error path is why the context is
	// derived rather than the caller's: there the caller's ctx is not
	// done, so nothing else would ever tell the ticker to stop.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	var heartbeatDone sync.WaitGroup
	if s.opts.Dynamic && s.opts.Heartbeat != nil {
		heartbeatDone.Go(func() { s.runHeartbeat(hbCtx) })
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
		// Ahead of children.shutdown, not after: heartbeatTick spawns
		// into the same registry the wind-down drains, so a tick still
		// in flight could land a child behind the drain and leak it past
		// shutdown. A tick is a gate call plus non-blocking spawns, so
		// the wait is short.
		hbCancel()
		heartbeatDone.Wait()
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
		// A clean exit takes the state file with it. That is what makes a
		// file left behind mean something: it names a pid, and a pid that
		// is gone is a serve that crashed rather than one that stopped.
		if err := s.dropActivity(); err != nil {
			s.logf("serve: remove state file: %v", err)
		}
		return <-errCh
	case err := <-errCh:
		hbCancel()
		heartbeatDone.Wait()
		return err
	}
}

func (s *Server) registerRoutes() {
	s.router.HandleFunc("/", s.handleDash)
	s.router.HandleFunc("/idea/new", s.handleNewIdea)
	// Per-run page. Uses Go 1.22+ pattern wildcards so the project
	// and slug fall out of the URL without manual splitting.
	s.router.HandleFunc("GET /run/{project}/{slug}", s.handleRunPage)
	s.router.HandleFunc("GET /run/{project}/{slug}/canvas/{stage}", s.handleCanvas)
	// Read-only agent-transcript viewer for a stage. Same unarmed-serve
	// bucket as the canvas route; ?agent / ?before / ?fragment are the
	// backend pick, the paging cursor, and the load-earlier fetch form.
	s.router.HandleFunc("GET /run/{project}/{slug}/transcript/{stage}", s.handleTranscript)
	s.router.HandleFunc("GET /run/{project}/{slug}/edit", s.handleCaptureEditForm)
	s.router.HandleFunc("POST /run/{project}/{slug}/edit", s.handleCaptureEditSubmit)
	s.router.HandleFunc("POST /run/{project}/{slug}/close", s.handleClose)
	s.router.HandleFunc("POST /run/{project}/{slug}/reopen", s.handleIdeaReopen)
	s.router.HandleFunc("POST /run/{project}/{slug}/tag", s.handleIdeaTag)
	s.router.HandleFunc("POST /run/{project}/{slug}/untag", s.handleIdeaUntag)
	// The advance mark: the operator read the stage's canvas and approves,
	// so the run's next pickup starts at the successor. A journal commit,
	// not a spawn — the heartbeat is what rides it.
	s.router.HandleFunc("POST /run/{project}/{slug}/advance", s.handleAdvance)
	// Chore detail page. A chore isn't a run, so it has its own /chore
	// namespace; read-only, because serve opens due chores on its own
	// cadence now and `moe chore open --now` is the operator's override.
	s.router.HandleFunc("GET /chore/{project}/{name}", s.handleChorePage)

	// The heartbeat's own page: what it decided, what it spawned, what
	// died. The boards carry a brief status cluster in their header and
	// link here for the trace.
	s.router.HandleFunc("GET /serve", s.handleServePage)

	// Read-only browsing of the bureaucracy's durable content: lore,
	// projects, per-project knowledge and digital-twin docs. All render
	// from os.ReadFile + the internal/md renderer.
	s.router.HandleFunc("GET /lore", s.handleLoreIndex)
	s.router.HandleFunc("GET /lore/{name}", s.handleLoreEntry)
	s.router.HandleFunc("GET /projects", s.handleProjectsIndex)
	s.router.HandleFunc("GET /projects/{project}", s.handleProjectHub)
	// The heartbeat's per-project brake: it writes config and starts
	// nothing, like every other route here.
	s.router.HandleFunc("POST /projects/{project}/mode", s.handleProjectMode)
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

// completedCap reads the ?completed=N page size a "show more" click
// asks for. Absent, unparseable or below one falls back to the default
// cap — a mangled URL should still render a page rather than an error.
// There is no upper clamp: a cap past the row count just shows every
// row, which is the same page ?all=1 used to serve.
func completedCap(r *http.Request) int {
	q := r.URL.Query().Get("completed")
	if q == "" {
		return dash.CompletedCap
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 {
		return dash.CompletedCap
	}
	return n
}

// handleDash renders the home page. Pulls dash data through the
// Options.GatherDash callback so this package stays workflow-
// registry-free. ?completed=N sets how many top-level completed runs
// to show; ?completed=N&fragment=1 returns just those rows, for the
// show-more JS to swap in place.
func (s *Server) handleDash(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.opts.GatherDash == nil {
		http.Error(w, "dash not configured (Options.GatherDash is nil)", http.StatusInternalServerError)
		return
	}
	rows, projectCount, activeProjects, histogram, err := s.opts.GatherDash("")
	if err != nil {
		s.logf("dash gather: %v", err)
		http.Error(w, "dash error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	vm := newDashVM(now, rows, projectCount, activeProjects, histogram, completedCap(r))
	if r.URL.Query().Get("fragment") != "" {
		// A show-more fetch wants COMPLETED rows and nothing else.
		s.render(w, r, "completed_chunk.html", vm.Completed)
		return
	}
	// From memory, not from the state file: serve holds the record, and a
	// round trip through its own snapshot would only add a beat of lag.
	vm.Serve = s.activity.panel(now)
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

// handleServePage renders the heartbeat's trace: the process status, a
// line per project it has a verdict for, and the whole activity ring
// with each failed child's output tail behind a details. Board-wide —
// single operator, at most 50 events, so one page and one scan beat a
// per-project filter.
func (s *Server) handleServePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "serve.html", s.activity.panel(time.Now().UTC()))
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

// saveActivity rewrites the state file from the current record.
// Warn-only: the file is a convenience for `moe dash`, and a serve that
// can't write it should still serve.
//
// The lock is what makes the file honest. Callers arrive from three
// goroutines at once — the ticker, each sweep watcher, and the registry
// hooks — and a child that ends a sweep wakes two of them on the same
// instant. writeSnapshot is atomic per write, but atomicity says nothing
// about *order*: unserialised, a caller can snapshot, be descheduled
// while a second caller records and writes something newer, and then
// rename its stale copy over the top. Nothing rewrites the file until
// the next event, which is minutes away, so the dash spends that whole
// window showing a sweep that already finished. Holding stateMu across
// snapshot-and-write makes rename order match record order: every
// mutation is followed by a write whose snapshot is taken after it.
//
// Once stopped is set the save is a no-op — see dropActivity.
func (s *Server) saveActivity() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stopped {
		return
	}
	if err := writeSnapshot(s.opts.Root, s.activity.snapshot(time.Now())); err != nil {
		s.logf("serve: write state file: %v", err)
	}
}

// dropActivity closes the state file for good: no further save writes it,
// and the file goes away.
//
// The flag is the point. children.shutdown returns as soon as every
// child's done channel closes, but each reader goroutine runs its exit
// hook *after* closing it, and the sweep watchers aren't waited on at
// all — so on the ordinary Ctrl-C with a live child, a straggling save
// lands after the remove and re-creates the file. Serve then exits
// leaving a record that names a dead pid, and every later `moe dash`
// reports a crash that never happened, permanently, until the next serve
// start. Setting stopped under the same mutex saveActivity takes lets a
// save already in flight finish before the remove and turns every later
// one into a no-op.
func (s *Server) dropActivity() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.stopped = true
	return removeSnapshot(s.opts.Root)
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
