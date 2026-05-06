package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// TestPickFollowTargetEmpty: no runs registered → no candidate, no
// "last:" entry in the idle summary. The empty-bureaucracy state must
// be a clean idle screen.
func TestPickFollowTargetEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	path, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if path != "" {
		t.Fatalf("expected no candidate, got %q", path)
	}
	if sum.activeCount != 0 || sum.last != nil {
		t.Fatalf("expected empty summary, got %+v", sum)
	}
}

// TestPickFollowTargetParkedAtDesign: a fresh sdlc run is parked at
// design under the parking rule, so its design canvas is the natural
// candidate. The candidate path is the run's design content.md.
func TestPickFollowTargetParkedAtDesign(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	path, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if !strings.HasSuffix(path, "tele/runs/fix-it/documents/design/content.md") {
		t.Fatalf("unexpected path %q", path)
	}
}

// TestPickFollowTargetIgnoresParkedAtCode: a run parked at code shouldn't
// surface as a follow candidate — code stages are deliberately excluded
// from `moe follow`. Under the forward-walking parking rule a run is
// only parked at code once both design and code work turns are in,
// with code's no older than design's.
func TestPickFollowTargetIgnoresParkedAtCode(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))

	path, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if path != "" {
		t.Fatalf("expected no candidate (parked at code), got %q", path)
	}
}

// TestPickFollowTargetSessionOnDesignBeatsParkedElsewhere: a run
// parked at code (design and code both committed, code last) but with
// an open session on the design doc still surfaces. The session
// signal — the operator is mid-edit on design right now — overrides
// the parking rule's choice.
func TestPickFollowTargetSessionOnDesignBeatsParkedElsewhere(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	commitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
	// Parked at code now; open a session on the design doc to flip
	// the pick back.
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	path, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if !strings.HasSuffix(path, "tele/runs/fix-it/documents/design/content.md") {
		t.Fatalf("expected design path, got %q", path)
	}
}

// TestPickFollowTargetSessionTierBeatsParkedTier: two runs are
// candidates — one with an open design session, one merely parked at
// design with newer activity. The session-open one wins; the more-
// recent parked-only run does not get to outrank a live session.
func TestPickFollowTargetSessionTierBeatsParkedTier(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	// Live tier: design committed (parked at code), open session on design.
	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "tele", "alpha", "sdlc", "design", t0)
	sess, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	// Parked-only tier: fresh run, more recent activity than alpha's
	// design commit but no open session.
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
		t0.Add(2*time.Hour))

	path, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if !strings.Contains(path, "/runs/alpha/") {
		t.Fatalf("expected alpha (live tier) to win, got %q", path)
	}
}

// TestPickFollowTargetMostRecentParked: two parked-at-design runs in
// the same tier — the more-recently-active one wins. We backdate both
// runs' latest MoE-Run commit so the journal index's topological-walk
// "first encountered" picks up the controlled timestamps rather than
// the seedRun open commits at real-time-now (which would otherwise
// tie within the same second of wall-clock).
func TestPickFollowTargetMostRecentParked(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele", t0)
	commitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
		t0.Add(time.Hour))

	path, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if !strings.Contains(path, "/runs/beta/") {
		t.Fatalf("expected beta (more recent) to win, got %q", path)
	}
}

// TestPickFollowTargetRunFilterPinsSpecificRun: --run locks to the
// named run, even when another run would otherwise outrank it under
// the tier rules. Pin behaviour is the design's stated escape hatch
// for "I know which design I want to watch."
func TestPickFollowTargetRunFilterPinsSpecificRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// Make alpha the natural winner under tier rules…
	commitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele",
		t0.Add(time.Hour))
	// …but pin to beta. Seed a content.md so the os.Stat check passes;
	// the pin is for an existing-on-disk canvas.
	writeContent(t, root, "tele", "beta", "design", "# beta design\n")

	path, _, err := pickFollowTarget(root, "beta")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if !strings.Contains(path, "/runs/beta/") {
		t.Fatalf("expected pinned beta, got %q", path)
	}
}

// TestPickFollowTargetRunFilterMissingCanvasIdles: pinning to a run
// whose design canvas isn't on disk yet falls through to the idle
// screen — follow keeps polling so the operator can pin pre-emptively
// and have the pager spawn the moment the canvas materialises.
func TestPickFollowTargetRunFilterMissingCanvasIdles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)

	// No content.md was ever written for alpha's design doc.
	path, _, err := pickFollowTarget(root, "alpha")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if path != "" {
		t.Fatalf("expected idle (no canvas yet), got %q", path)
	}
}

// TestPickFollowTargetSkipsTerminalAndPushed: terminal statuses
// (merged, closed, promoted) and StatusPushed runs aren't candidates —
// pushed runs have nothing left for moe to drive. The active-count
// in the idle summary still includes pushed runs (they're "awaiting
// merge"), and that's the row the "last:" pointer reports.
func TestPickFollowTargetSkipsTerminalAndPushed(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "merged-one", "sdlc", run.StatusMerged)
	seedRun(t, root, "tele", "shipped", "sdlc", run.StatusPushed)

	path, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if path != "" {
		t.Fatalf("expected no candidate, got %q", path)
	}
	if sum.activeCount != 1 {
		t.Fatalf("expected 1 active (the pushed run), got %d", sum.activeCount)
	}
	if sum.last == nil || sum.last.run != "shipped" || sum.last.state != "awaiting merge" {
		t.Fatalf("unexpected last: %+v", sum.last)
	}
}

// TestPickFollowTargetIdeaRunsExcluded: idea runs (workflow=idea) are
// backlog, not active. They neither surface as design candidates nor
// inflate the activeCount.
func TestPickFollowTargetIdeaRunsExcluded(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "captured", "idea", run.StatusInProgress)

	path, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if path != "" {
		t.Fatalf("expected no candidate from idea workflow, got %q", path)
	}
	if sum.activeCount != 0 {
		t.Fatalf("expected idea runs to skip activeCount, got %d", sum.activeCount)
	}
}

// TestIdleLineEmpty: with nothing active, the idle line drops the
// trailing "last:" segment so the operator sees an honest "0 active"
// without a phantom pointer.
func TestIdleLineEmpty(t *testing.T) {
	got := idleLine(followSummary{})
	want := "(no design in play · 0 active)"
	if got != want {
		t.Fatalf("idleLine(): got %q want %q", got, want)
	}
}

// TestIdleLineWithLast: with one active run, the idle line points at
// it. The state cell carries either "awaiting merge" or
// "<workflow>:<stage>", whichever stateForActive returned.
func TestIdleLineWithLast(t *testing.T) {
	got := idleLine(followSummary{
		activeCount: 2,
		last:        &followLast{project: "tele", run: "fix-it", state: "awaiting merge"},
	})
	want := "(no design in play · 2 active · last: tele/fix-it awaiting merge)"
	if got != want {
		t.Fatalf("idleLine(): got %q want %q", got, want)
	}
}

// TestFollowRegistered: smoke check that `moe follow` is dispatchable
// — reaching this point at all means the init() registration didn't
// duplicate-panic against another command's name.
func TestFollowRegistered(t *testing.T) {
	if _, ok := commands["follow"]; !ok {
		t.Fatal("follow command not registered")
	}
}
