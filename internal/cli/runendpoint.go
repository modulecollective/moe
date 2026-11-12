package cli

// An execution owner's live endpoint (internal/runsock): bound when the
// owner takes the run, closed before it lets go, and fed by the code
// that makes each decision — admission, quota, the agent turn, the
// decision prompt, the walk's boundaries.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runsock"
)

// Launch-origin handshake. serve sets both on every child it spawns;
// they ride the child's environment, so a Rex job adopted by a later
// serve still names the launch that started it.
const (
	envLaunchHost  = "MOE_LAUNCH_HOST"
	envServeLaunch = "MOE_SERVE_LAUNCH"
)

// currentSweep is the pulse run whose kick loop this process is
// running, "<project>/<run>"; empty outside a sweep.
var currentSweep string

func launchOrigin() runsock.Origin {
	o := runsock.Origin{Sweep: currentSweep, Host: os.Getenv(envLaunchHost), ServeLaunch: os.Getenv(envServeLaunch)}
	if o.Host == "" {
		o.Host = "terminal"
		if rexTerminal() != nil {
			o.Host = "rex"
		}
	}
	return o
}

// rexTerminal is the Rex block this process runs in, from its own
// environment. Full IDs only: a label would let a reader resolve some
// other block.
func rexTerminal() *runsock.Terminal {
	s, b := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	if s == "" || b == "" || strings.ContainsAny(s+b, " \t\n") {
		return nil
	}
	return &runsock.Terminal{Session: s, Block: b}
}

// startRunEndpoint binds the run's endpoint for a fresh execution owner.
func startRunEndpoint(root, projectID, runID string) (*runsock.Server, error) {
	initial := runsock.Status{Origin: launchOrigin(), Terminal: rexTerminal()}
	if md, err := run.Load(root, projectID, runID); err == nil {
		initial.Workflow = md.Workflow
	}
	srv, err := runsock.Listen(runsock.RuntimeDir(), runsock.Identity{Root: root, Project: projectID, Run: runID}, initial)
	if err != nil {
		return nil, fmt.Errorf("run %s/%s: cannot serve its live endpoint, so it cannot run here: %w", projectID, runID, err)
	}
	srv.Submit = func(r runcontrol.Request) (runcontrol.Request, error) {
		return submitFromEndpoint(root, projectID, runID, r)
	}
	return srv, nil
}

// submitNestedBudget bounds how long an endpoint submit waits for this
// process's own repository-lock holder (a turn commit) to finish.
var submitNestedBudget = 2 * time.Minute

// submitFromEndpoint persists a request arriving on the owner's socket.
// The handler runs inside the owner, where the main path may hold the
// repository lock; repolock refuses an in-process acquire rather than
// waiting, so wait here. The lock order is fixed: the endpoint's
// dispatch lock (held by the caller), then the repository lock.
func submitFromEndpoint(root, projectID, runID string, r runcontrol.Request) (runcontrol.Request, error) {
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return runcontrol.Request{}, err
	}
	if err := controlPolicy(md); err != nil {
		return runcontrol.Request{}, err
	}
	deadline := time.Now().Add(submitNestedBudget)
	for {
		got, err := runcontrol.Submit(root, projectID, runID, r)
		if _, nested := errors.AsType[*repolock.NestedError](err); !nested || time.Now().After(deadline) {
			return got, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// controlPolicy is which runs take controls at all: operator-paced stage
// workflows. Shared by every door so they refuse alike.
func controlPolicy(md *run.Metadata) error {
	if !operatorCascades(md.Workflow) {
		return fmt.Errorf("%s/%s is a %s run; controls apply to operator-paced stage workflows", md.Project, md.ID, md.Workflow)
	}
	return nil
}

// runEndpoint is this invocation's endpoint for a run, or nil.
func runEndpoint(root, projectID, runID string) *runsock.Server {
	invocationRunsMu.Lock()
	defer invocationRunsMu.Unlock()
	return invocationEndpoints[invocationRunKey(root, projectID, runID)]
}

// publishRun applies change to the run's live status, when this
// invocation serves its endpoint.
func publishRun(root, projectID, runID string, change func(*runsock.Status)) {
	if srv := runEndpoint(root, projectID, runID); srv != nil {
		srv.Update(change)
	}
}

// publishPhase is publishRun for the common case: a phase, its stage
// when known, and why it waits (empty when it does not).
func publishPhase(root string, md *run.Metadata, phase, stage, waiting string) {
	publishRun(root, md.Project, md.ID, func(s *runsock.Status) {
		s.Phase, s.Waiting = phase, waiting
		if stage != "" {
			s.Stage = stage
		}
	})
}

// cascadePublish is publishPhase for the cascade seams, which carry no
// root.
func cascadePublish(md *run.Metadata, change func(*runsock.Status)) {
	if root, err := findRoot(nopWriter{}); err == nil {
		publishRun(root, md.Project, md.ID, change)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// boundaryPhase is the phase an admitted boundary moves the owner to.
func boundaryPhase(boundary string) (phase, stage string) {
	switch {
	case boundary == "push":
		return runsock.PhaseShipping, ""
	case boundary == "the next-stage decision":
		return runsock.PhaseDeciding, ""
	case strings.HasPrefix(boundary, "stage "):
		return runsock.PhaseBetween, strings.Fields(strings.TrimPrefix(boundary, "stage "))[0]
	}
	return runsock.PhaseBetween, ""
}
