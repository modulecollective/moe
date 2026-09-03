package cli

import (
	"context"
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
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "listen address override (host or host:port); default 127.0.0.1:4242")
	port := fs.Int("port", serve.DefaultPort, "listen port (ignored when --addr already includes one)")
	dynamic := fs.Bool("dynamic", false, "stand this process at the dynamic consent rung: the resident heartbeat may start settled work; off by default")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the moe web UI as an HTTP server. Binds 127.0.0.1:4242 by")
		moePrintln(stderr, "default; put a `tailscale serve` proxy (or similar) in front to expose")
		moePrintln(stderr, "it to peers. Pass --addr 0.0.0.0 or --addr <tailnet-ip> to bind wider.")
		moePrintln(stderr, "Ctrl-C to stop.")
		moePrintln(stderr, "")
		moePrintln(stderr, "The web starts nothing. Every action it offers writes a journal")
		moePrintln(stderr, "commit and stops there: capture or edit an idea, tag it for a")
		moePrintln(stderr, "workflow, close or reopen a run, mark the current stage advanced,")
		moePrintln(stderr, "answer a run's open question. No request executes code — but agents")
		moePrintln(stderr, "read those writes in their prompts, and an armed serve's heartbeat")
		moePrintln(stderr, "starts agents because of them: whoever can reach the listener steers")
		moePrintln(stderr, "the machine. Reach is the only auth, so whatever sits in front of the")
		moePrintln(stderr, "listener is the security boundary.")
		moePrintln(stderr, "")
		moePrintln(stderr, "--dynamic, or a non-empty MOE_SERVE_DYNAMIC, is the standing consent")
		moePrintln(stderr, "rung: it starts the resident heartbeat, the only thing in this")
		moePrintln(stderr, "process that starts an agent. Unarmed the ticker never fires and")
		moePrintln(stderr, "what the web wrote simply waits. Stopping the process is the whole")
		moePrintln(stderr, "retraction.")
		moePrintln(stderr, "")
		moePrintln(stderr, "`moe project mode <id> paused|safe` holds one project back without")
		moePrintln(stderr, "stopping anything: paused is never swept, safe is swept and groomed")
		moePrintln(stderr, "but starts only what you marked. Both bind the clock, not you.")
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

	opts := serveOptions(root, stderr)
	opts.Addr = *addr
	opts.Port = *port
	// Flag or a non-empty env var arms the process; the env var lets a
	// daemonized cloud-box `moe serve` opt in without threading a flag
	// through its unit/launcher. Non-empty enables, mirroring how
	// MOE_SERVE_NOTIFY_URL is read just below.
	//
	// One switch, one consumer: the heartbeat is the only thing in the
	// process that starts an agent, so armed means the clock may act.
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
func serveOptions(root string, stderr io.Writer) serve.Options {
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
		ChainMembers: func(project, runID string) ([]dash.Row, error) {
			return chainMembers(root, project, runID, time.Now().UTC())
		},
		// Provenance crosses the seam already resolved to display strings:
		// the walk reads the journal index *and* the spawning pulse's
		// canvas gate, and neither belongs on serve's side.
		RunProvenance: func(project, runID string) ([]serve.ProvHop, error) {
			return runProvenance(root, project, runID)
		},
		// The checklist grammar all three harvest pipelines share lives
		// in cli; the run page needs to read it the way harvest does, so
		// it crosses as one gather rather than as a re-implementation on
		// the serve side.
		GatherRunTraces: func(project, runID string) (serve.RunTraces, error) {
			return GatherRunTraces(root, project, runID)
		},
		// serve can't host $EDITOR inside an HTTP POST, so close runs
		// with --no-edit semantics (skipEdit=true): harvest the
		// followups/lore files as they sit on disk. Dispatch is by the
		// run's own workflow through the close registry — the same
		// (subject, cleanup) pair `moe <workflow> close` registered —
		// so the in-process path and the CLI verb stay one pipeline.
		// The close's stderr is serve's own log: nothing left on that
		// path prompts, and the one thing it writes — the advisory for
		// a canvas that claimed a followup it never filed — is a
		// dropped thread. A close from the web must not be the quiet
		// one, so the writer goes to the log, not to io.Discard.
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
				reg.cleanup, project, runID, true, stderr)
		},
		// The workflow registries are init-time static, so the serve UI
		// declarations cross the seam as a lookup plus a precomputed
		// tag-destination list.
		WorkflowUI:   lookupServeWorkflowUI,
		TagWorkflows: serveTagWorkflows(),
		// The stage gates live on the workflow, so the verdict crosses
		// the seam per-run rather than riding the static WorkflowUI
		// declaration. Both the advance chip and the advance route ask,
		// so the web can't offer a mark the ladder would ignore.
		CheckStageGate: func(md *run.Metadata, stage string) (bool, error) {
			wf, err := LookupWorkflow(md.Workflow)
			if err != nil {
				return false, err
			}
			return wf.CheckStageGate(root, md, stage)
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
	}
}
