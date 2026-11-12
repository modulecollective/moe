package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
)

func readLive(t *testing.T, root string) (runsock.Status, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return runsock.ReadStatus(ctx, runsock.RuntimeDir(), runsock.Identity{Root: root, Project: "moe", Run: "change-auth"})
}

// An execution owner serves the run's endpoint for exactly as long as it
// holds the guard, carrying the origin its launcher stated; a
// mutation-only holder serves none, and no owner never reads as idle.
func TestEndpointLivesWithExecutionOwnership(t *testing.T) {
	root, _ := controlFixture(t)
	t.Setenv(envLaunchHost, "local-pty")
	t.Setenv(envServeLaunch, "heartbeat:moe/1@t")
	currentSweep = "moe/pulse-one"
	t.Cleanup(func() { currentSweep = "" })

	rel, err := ownRun(root, "moe", "change-auth", true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := readLive(t, root)
	if err != nil {
		t.Fatal(err)
	}
	want := runsock.Origin{Sweep: "moe/pulse-one", Host: "local-pty", ServeLaunch: "heartbeat:moe/1@t"}
	if st.Origin != want || st.Workflow != "sdlc" || st.Phase != runsock.PhaseAdmission {
		t.Fatalf("status = %+v", st)
	}
	if err := rel(); err != nil {
		t.Fatal(err)
	}
	if _, err := readLive(t, root); !errors.Is(err, runsock.ErrNoOwner) {
		t.Fatalf("after release: %v, want ErrNoOwner", err)
	}

	rel, err = ownRun(root, "moe", "change-auth", false)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if _, err := readLive(t, root); !errors.Is(err, runsock.ErrNoOwner) {
		t.Fatalf("mutation-only holder served an endpoint: %v", err)
	}
}

// An owner that cannot bind its endpoint does not run: ownership is
// refused and the guard released, so nothing runs unobservable.
func TestEndpointBindFailureRefusesExecution(t *testing.T) {
	root, _ := controlFixture(t)
	restore := runsock.SetTransport(func(string) (net.Listener, error) {
		return nil, errors.New("socket: operation not permitted")
	}, func(context.Context, string) (net.Conn, error) { return nil, errors.New("unreachable") })
	t.Cleanup(restore)

	if _, err := ownRun(root, "moe", "change-auth", true); err == nil || !strings.Contains(err.Error(), "cannot serve its live endpoint") {
		t.Fatalf("ownRun = %v, want an endpoint refusal", err)
	}
	if busy, err := runowner.Busy(root, "moe", "change-auth"); err != nil || busy {
		t.Fatalf("guard after a refused bind: busy=%v err=%v", busy, err)
	}
	if heldRun(root, "moe", "change-auth") != nil {
		t.Fatal("refused owner left a guard in the invocation table")
	}
}

// `moe control park` reaches the live owner through its endpoint and is
// bound to that owner's generation; a successor never honours it.
func TestControlParkThroughEndpointBindsGeneration(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	gen := ownerGeneration(root, "moe", "change-auth")

	code, out, errs := runControl(t, "park", "moe/change-auth", "--id", "k1")
	if code != 0 || strings.Contains(errs, "unavailable") {
		t.Fatalf("park = %d:\n%s%s", code, out, errs)
	}
	k1, _ := controlRecord(t, root).Find("k1")
	if k1.Owner != gen || gen == "" {
		t.Fatalf("park owner = %q, want the live generation %q", k1.Owner, gen)
	}

	// The same request reaching a different owner is retired, not obeyed.
	if err := runcontrol.Settle(root, "moe", "change-auth", "k1", runcontrol.Settlement{State: runcontrol.StateCompleted}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := runcontrol.Submit(root, "moe", "change-auth", runcontrol.Request{ID: "k2", Kind: runcontrol.KindPark, Owner: "old-owner"}); err != nil {
		t.Fatal(err)
	}
	if c := controlCheckpoint(root, md, "stage test", io.Discard); c != 0 {
		t.Fatalf("checkpoint = %d; a park for another owner must not stop this one", c)
	}
	if k2, _ := controlRecord(t, root).Find("k2"); !strings.Contains(k2.Outcome, "retired") {
		t.Fatalf("stale park = %+v", k2)
	}
}

// A checkpoint publishes the boundary it admitted, so a reader sees
// what the owner is doing; a refused one leaves the phase alone.
func TestCheckpointPublishesAdmittedBoundary(t *testing.T) {
	root, md := controlFixture(t)
	ownForTest(t, root)
	if c := controlCheckpoint(root, md, "push", io.Discard); c != 0 {
		t.Fatal(c)
	}
	st, err := readLive(t, root)
	if err != nil || st.Phase != runsock.PhaseShipping {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if c := controlCheckpoint(root, md, "stage review", io.Discard); c != 0 {
		t.Fatal(c)
	}
	if st, _ := readLive(t, root); st.Phase != runsock.PhaseBetween || st.Stage != "review" {
		t.Fatalf("status = %+v", st)
	}
}

// With no live owner answering, a hold is committed directly and an
// owner, if one exists, is told nothing new: it reads it at its next
// boundary.
func TestControlWithoutEndpointCommitsDirectly(t *testing.T) {
	root, _ := controlFixture(t)
	h, err := runowner.Acquire(root, "moe", "change-auth") // an owner with no endpoint
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	code, _, errs := runControl(t, "pause", "moe/change-auth")
	if code != 0 || !strings.Contains(errs, "owner acknowledgement unavailable") {
		t.Fatalf("pause = %d: %s", code, errs)
	}
	if !controlRecord(t, root).Held() {
		t.Fatal("pause not recorded")
	}
}
