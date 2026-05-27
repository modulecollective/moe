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
	"github.com/modulecollective/moe/internal/run"
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
	addr := fs.String("addr", "", "listen address override (host or host:port); default 127.0.0.1:4242")
	port := fs.Int("port", serve.DefaultPort, "listen port (ignored when --addr already includes one)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe serve [--addr <host[:port]>] [--port <n>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the moe web UI as an HTTP server. Binds 127.0.0.1:4242 by")
		moePrintln(stderr, "default; put a `tailscale serve` proxy (or similar) in front to expose")
		moePrintln(stderr, "it to peers. Pass --addr 0.0.0.0 or --addr <tailnet-ip> to bind wider.")
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
		ResolveCanvas: func(project, runID, stage string) (string, error) {
			md, err := run.Load(root, project, runID)
			if err != nil {
				return "", err
			}
			return resolveCanvasPath(root, md.Workflow, project, runID, stage)
		},
		RunStages: func(project, runID string) ([]string, error) {
			md, err := run.Load(root, project, runID)
			if err != nil {
				return nil, err
			}
			wf, err := LookupWorkflow(md.Workflow)
			if err != nil {
				return nil, err
			}
			return wf.Stages(), nil
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
