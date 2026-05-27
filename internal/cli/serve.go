package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/serve"
)

func init() {
	Register(&Command{
		Name:    "serve",
		Summary: "run the moe web UI",
		Run:     runServe,
	})
}

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "listen address override (host or host:port); default auto-detects a local Tailscale IPv4 if available")
	port := fs.Int("port", serve.DefaultPort, "listen port (ignored when --addr already includes one)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe serve [--addr <host[:port]>] [--port <n>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the moe web UI as an HTTP server. The default bind address")
		moePrintln(stderr, "auto-detects a local Tailscale IPv4 if one is available; use --addr")
		moePrintln(stderr, "to listen somewhere else (for example, 127.0.0.1:8080 for local-only).")
		moePrintln(stderr, "Ctrl-C to stop; live runs spawned by serve die with it (PTY teardown).")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	srv, err := serve.New(serve.Options{
		Addr:      *addr,
		Port:      *port,
		Root:      root,
		Logger:    stderr,
		NotifyURL: os.Getenv("MOE_SERVE_NOTIFY_URL"),
		GatherDash: func(showAll bool) ([]dash.Row, int, int, error) {
			snap, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{All: showAll})
			if err != nil {
				return nil, 0, 0, err
			}
			return snap.Rows, snap.ProjectCount, snap.ActiveProjects, nil
		},
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	moePrintf(stdout, "moe serve: http://%s/\n", srv.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
