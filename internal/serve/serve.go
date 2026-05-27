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

	// GatherDash returns the dash data the home route renders. Its
	// bool argument is the ?all= query flag (mirrors `moe dash
	// --all`). The cli/serve.go entry point wires this to
	// cli.GatherDashSnapshot so serve itself doesn't depend on the
	// workflow registry.
	//
	// Required by the dash route; absent means GET / returns 500.
	GatherDash func(showAll bool) (rows []dash.Row, projectCount, activeProjects int, err error)

	// NotifyURL is the webhook URL we POST a small JSON payload to
	// when a serve-parented run exits. Empty disables notifications.
	// The cli wrapper populates this from $MOE_SERVE_NOTIFY_URL.
	NotifyURL string
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
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			s.logf("shutdown: %v", err)
		}
		// Tear down PTY children alongside HTTP graceful shutdown.
		// The kernel SIGHUPs them via controlling-terminal teardown
		// when their master fd closes.
		s.children.shutdown(shutCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func (s *Server) registerRoutes() {
	s.router.HandleFunc("/", s.handleDash)
	s.router.HandleFunc("/run/new", s.handleNewRun)
	// Per-run page. Uses Go 1.22+ pattern wildcards so the project
	// and slug fall out of the URL without manual splitting.
	s.router.HandleFunc("GET /run/{project}/{slug}", s.handleRunPage)
	s.router.HandleFunc("POST /run/{project}/{slug}/key", s.handleRunKey)
	s.router.HandleFunc("POST /run/resume", s.handleResume)

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
	rows, projectCount, activeProjects, err := s.opts.GatherDash(showAll)
	if err != nil {
		s.logf("dash gather: %v", err)
		http.Error(w, "dash error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	vm := newDashVM(time.Now().UTC(), rows, projectCount, activeProjects, showAll)
	// Mark which active rows are currently parented by serve so the
	// template can pick between "open" (live) and "take it over"
	// (resumable) affordances.
	for i := range vm.Active {
		id := vm.Active[i].Project + "/" + vm.Active[i].Run
		if _, ok := s.children.get(id); ok {
			vm.Active[i].Live = true
		} else {
			vm.Active[i].Resumable = true
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

func (s *Server) logf(format string, a ...any) {
	if s.opts.Logger == nil {
		return
	}
	fmt.Fprintf(s.opts.Logger, format+"\n", a...)
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
