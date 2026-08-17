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
	"github.com/modulecollective/moe/internal/runopen"
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
	// Two verbs ride inside the serve command rather than turning it into
	// a CommandGroup: a group dispatches on the first argument, and
	// `moe serve --dynamic` has to keep working. Peeking for exactly two
	// words costs less than the conversion and breaks nothing — neither
	// is a flag, so a leading `-` still falls through to the parser.
	if len(args) > 0 {
		switch args[0] {
		case "snooze":
			return runServeSnooze(args[1:], stdout, stderr)
		case "wake":
			return runServeWake(args[1:], stdout, stderr)
		}
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "listen address override (host or host:port); default 127.0.0.1:4242")
	port := fs.Int("port", serve.DefaultPort, "listen port (ignored when --addr already includes one)")
	dynamic := fs.Bool("dynamic", false, "stand this process at the dynamic consent rung: run-spawning actions and the resident heartbeat; off by default")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]")
		moePrintln(stderr, "       moe serve snooze [duration]   hold the heartbeat (no arg: report)")
		moePrintln(stderr, "       moe serve wake                resume sweeps now")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the moe web UI as an HTTP server. Binds 127.0.0.1:4242 by")
		moePrintln(stderr, "default; put a `tailscale serve` proxy (or similar) in front to expose")
		moePrintln(stderr, "it to peers. Pass --addr 0.0.0.0 or --addr <tailnet-ip> to bind wider.")
		moePrintln(stderr, "Ctrl-C to stop; live runs spawned by serve die with it (PTY teardown).")
		moePrintln(stderr, "")
		moePrintln(stderr, "Safe by default: idea capture, run close/edit/reopen, and all views")
		moePrintln(stderr, "work; the run-spawning actions (which run agent subprocesses, i.e.")
		moePrintln(stderr, "arbitrary code) refuse with 403, and the heartbeat never ticks.")
		moePrintln(stderr, "")
		moePrintln(stderr, "--dynamic, or a non-empty MOE_SERVE_DYNAMIC, is the standing consent")
		moePrintln(stderr, "rung: this process may look at the board on its own clock and start")
		moePrintln(stderr, "settled work, and anything that can reach the listener can spawn a")
		moePrintln(stderr, "run — i.e. execute code. Stopping the process is the whole retraction.")
		moePrintln(stderr, "")
		moePrintln(stderr, "`moe serve snooze 2h` holds the clock without stopping anything:")
		moePrintln(stderr, "no sweeps until it expires, while clicks and hand-run")
		moePrintln(stderr, "pulses stay live. It survives a restart, and it always expires.")
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

	opts := serveOptions(root, stdout, stderr)
	opts.Addr = *addr
	opts.Port = *port
	// Flag or a non-empty env var arms the process; the env var lets a
	// daemonized cloud-box `moe serve` opt in without threading a flag
	// through its unit/launcher. Non-empty enables, mirroring how
	// MOE_SERVE_NOTIFY_URL is read just below.
	//
	// One switch, two consumers: the spawn bucket and the heartbeat. A
	// separate heartbeat flag would be a consent matrix nobody needs —
	// armed means the machine may act, clicks and clock alike.
	opts.Dynamic = *dynamic || os.Getenv("MOE_SERVE_DYNAMIC") != ""
	opts.NotifyURL = os.Getenv("MOE_SERVE_NOTIFY_URL")

	srv, err := serve.New(opts)
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

// serveOptions wires the cli side of the serve seam: every callback
// serve.Options carries, closed over the bureaucracy root. Listener and
// security fields (Addr, Port, Dynamic, NotifyURL) are the caller's to
// set — they come from flags and env in runServe, and from the test
// directly in tests.
func serveOptions(root string, stdout, stderr io.Writer) serve.Options {
	return serve.Options{
		Root:   root,
		Logger: stderr,
		// The resident heartbeat's gate. Serve owns the clock, the
		// backoff and the child; every question that needs the journal,
		// the chain graph or the workflow registry is answered on this
		// side. It only ticks on an armed serve — see Options.Dynamic.
		Heartbeat: newHeartbeatGate(root),
		GatherDash: func(projectID string) ([]dash.Row, int, int, []int, error) {
			snap, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{ProjectFilter: projectID})
			if err != nil {
				return nil, 0, 0, nil, err
			}
			return snap.Rows, snap.ProjectCount, snap.ActiveProjects, snap.Histogram, nil
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
			return docsForRun(root, md), nil
		},
		GatherRunRow: func(project, runID string) (dash.Row, bool, error) {
			return GatherRunRow(root, project, runID, time.Now().UTC())
		},
		// The chain head's own page is where the dash's `parked · kick?`
		// hint sends the operator, so it's where the batch has to be
		// legible. Membership is journal state, so it crosses the seam as
		// a callback like every other journal-shaped fact.
		ChainMembers: func(project, runID string) ([]dash.Row, string, error) {
			return chainMembers(root, project, runID, time.Now().UTC())
		},
		// Provenance crosses the seam already resolved to display strings:
		// the walk reads the journal index *and* the spawning pulse's
		// canvas gate, and neither belongs on serve's side.
		RunProvenance: func(project, runID string) ([]serve.ProvHop, error) {
			return runProvenance(root, project, runID)
		},
		// The followup/lore checklist grammar and the reflect ingestion
		// rule both live in cli; the run page needs to read them the way
		// harvest and reflect do, so they cross as one gather rather than
		// as a re-implementation on the serve side.
		GatherRunTraces: func(project, runID string) (serve.RunTraces, error) {
			return GatherRunTraces(root, project, runID)
		},
		// serve can't host $EDITOR inside an HTTP POST, so close runs
		// with --no-edit semantics (skipEdit=true): harvest the
		// followups/lore files as they sit on disk. Dispatch is by the
		// run's own workflow through the close registry — the same
		// (subject, cleanup) pair `moe <workflow> close` registered —
		// so the in-process path and the CLI verb stay one pipeline.
		CloseRun: func(project, runID string) error {
			md, err := run.Load(root, project, runID)
			if err != nil {
				return err
			}
			reg, ok := lookupCloseRegistration(md.Workflow)
			if !ok {
				return &runopen.NotClosableError{Reason: fmt.Sprintf(
					"workflow %s has no close pipeline", md.Workflow)}
			}
			return closeRunInProcess(root, md.Workflow, reg.subject,
				reg.cleanup, project, runID, true, io.Discard, io.Discard)
		},
		// The workflow registries are init-time static, so the serve UI
		// declarations cross the seam as a lookup plus a precomputed
		// new-run list.
		WorkflowUI:      lookupServeWorkflowUI,
		NewRunWorkflows: serveNewRunWorkflows(),
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
			res, err := openChoreInProcess(root, project, name, choreOpenNormal, stdout, stderr)
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
	}
}
