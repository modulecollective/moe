package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/chore"
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
			snap, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{All: showAll},
				newGatherTimer(stderr, "dash"))
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
		GatherRunRow: func(project, runID string) (dash.Row, bool, error) {
			return GatherRunRow(root, project, runID, time.Now().UTC(), stderr)
		},
		// serve can't host $EDITOR inside an HTTP POST, so close runs
		// with --no-edit semantics (skipEdit=true): harvest the
		// followups/lore files as they sit on disk. Mirrors `moe sdlc
		// close`'s subject + workspace-release cleanup so the in-process
		// path and the CLI verb stay one pipeline.
		CloseRun: func(project, runID string) error {
			return closeRunInProcess(root, "sdlc", sdlcCloseSubject,
				releaseWorkspaceCleanup, project, runID, true, io.Discard, io.Discard)
		},
		// GatherChore picks the named chore out of the per-project state
		// gather. Keeps the workflow registry on the cli side of the seam.
		GatherChore: func(project, name string) (chore.State, bool, error) {
			states, err := gatherChoreStates(root, project)
			if err != nil {
				return chore.State{}, false, err
			}
			for _, st := range states {
				if st.Definition.Name == name {
					return st, true, nil
				}
			}
			return chore.State{}, false, nil
		},
		// OpenChore runs the shared chore-open pipeline and translates its
		// internal guard errors into the sentinels serve maps to 404/409.
		OpenChore: func(project, name string) (serve.ChoreOpen, error) {
			res, err := openChoreInProcess(root, project, name, false)
			if err != nil {
				var notFound *choreNotFoundError
				var notOpenable *choreNotOpenableError
				switch {
				case errors.As(err, &notFound):
					return serve.ChoreOpen{}, fmt.Errorf("%w: %v", serve.ErrChoreNotFound, err)
				case errors.As(err, &notOpenable):
					return serve.ChoreOpen{}, fmt.Errorf("%w: %v", serve.ErrChoreNotOpenable, err)
				}
				return serve.ChoreOpen{}, err
			}
			return serve.ChoreOpen{
				Project:    res.Metadata.Project,
				Slug:       res.Metadata.ID,
				Workflow:   res.Workflow,
				FirstStage: res.FirstStage,
			}, nil
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
