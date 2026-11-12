package cli

import (
	"fmt"
	"io"
	"sync"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
)

// This invocation's held run guards (see internal/runowner), keyed by
// root and run. The table is how ownership survives the stage → prompt →
// continuation walk: those continuations re-enter through Command.Run
// (`next.Run`, `scuttle.Run`, the cascade dispatchers), which carries no
// handle, so a same-run nested entry borrows the outer guard from here
// instead of contending with it. A different run is a different key and
// acquires its own guard.
//
// Only the CLI's own verb entry points read the table. Callers that can
// run concurrently inside one long-lived process — serve's HTTP writers,
// sync's finalisation — call runowner.Acquire directly and never borrow,
// so two goroutines in serve still conflict like two processes.
var (
	invocationRunsMu sync.Mutex
	invocationRuns   = map[string]*runowner.Handle{}
	// invocationEndpoints is each execution-owned run's live endpoint
	// (internal/runsock), keyed like invocationRuns. Mutation-only guard
	// holders serve none.
	invocationEndpoints = map[string]*runsock.Server{}
)

func invocationRunKey(root, projectID, runID string) string {
	return root + "\x00" + projectID + "/" + runID
}

// ownRun takes (or, for a same-run nested entry, borrows) this
// invocation's guard on one run. The returned release is a no-op for a
// borrow; the outermost holder's release drops the lock. Contention
// comes back as a *runowner.BusyError.
//
// execute marks an owner that may run workflow work (a stage, a walk, a
// ship): it must serve the run's live endpoint, and an endpoint that
// cannot be bound refuses ownership — an owner nobody can observe or
// address is not allowed to run. A short mutation-only holder (close,
// resume) serves none.
func ownRun(root, projectID, runID string, execute bool) (release func() error, err error) {
	key := invocationRunKey(root, projectID, runID)
	invocationRunsMu.Lock()
	defer invocationRunsMu.Unlock()
	if invocationRuns[key] != nil {
		return func() error { return nil }, nil
	}
	h, err := runowner.Acquire(root, projectID, runID)
	if err != nil {
		return nil, err
	}
	var srv *runsock.Server
	if execute {
		if srv, err = startRunEndpoint(root, projectID, runID); err != nil {
			h.Release()
			return nil, err
		}
	}
	invocationRuns[key] = h
	invocationEndpoints[key] = srv
	return func() error {
		invocationRunsMu.Lock()
		defer invocationRunsMu.Unlock()
		// Stop serving, and join the handlers, before giving up the guard:
		// once it is free a successor may bind the same path.
		if s := invocationEndpoints[key]; s != nil {
			s.Close()
		}
		delete(invocationEndpoints, key)
		if invocationRuns[key] == h {
			delete(invocationRuns, key)
		}
		return h.Release()
	}, nil
}

// heldRun returns this invocation's live guard on a run, or nil. It is
// what an owning flow passes explicitly into shared writers
// (runopen.MarkAdvanced) so they skip reacquiring their own run.
func heldRun(root, projectID, runID string) *runowner.Handle {
	invocationRunsMu.Lock()
	defer invocationRunsMu.Unlock()
	return invocationRuns[invocationRunKey(root, projectID, runID)]
}

// ownRunForVerb is ownRun for a verb entry point: a refusal prints the
// reason and the release reports its own close error, so callers are a
// two-liner.
func ownRunForVerb(root, projectID, runID string, stderr io.Writer) (release func(), ok bool) {
	return ownRunVerb(root, projectID, runID, true, stderr)
}

// ownRunForMutation is ownRunForVerb for a short mutation-only holder,
// which serves no endpoint.
func ownRunForMutation(root, projectID, runID string, stderr io.Writer) (release func(), ok bool) {
	return ownRunVerb(root, projectID, runID, false, stderr)
}

func ownRunVerb(root, projectID, runID string, execute bool, stderr io.Writer) (release func(), ok bool) {
	rel, err := ownRun(root, projectID, runID, execute)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return nil, false
	}
	return func() {
		if err := rel(); err != nil {
			moePrintf(stderr, "%v\n", err)
		}
	}, true
}

// runOwnerHold is the planner-side reading of the guard: the skip
// phrase for a run another invocation owns, or "" when it is free. A
// probe failure holds rather than reading as free. A guard this
// invocation holds itself is not a hold: the CI repair re-asks
// eligibility after kickCIRepair has taken the run.
func runOwnerHold(root string, md *run.Metadata) string {
	if heldRun(root, md.Project, md.ID) != nil {
		return ""
	}
	busy, err := runowner.Busy(root, md.Project, md.ID)
	switch {
	case err != nil:
		return fmt.Sprintf("has an unreadable owner guard (%v) — skipping", err)
	case busy:
		return "is owned by an active moe invocation (open stage or unanswered prompt) — skipping"
	}
	return ""
}
