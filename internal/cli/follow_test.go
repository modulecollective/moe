package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
)

// TestPickFollowTargetEmpty: no runs registered → no candidate, no
// "last:" entry in the idle summary. The empty-bureaucracy state must
// be a clean idle screen.
func TestPickFollowTargetEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	target, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected no candidate, got %+v", target)
	}
	if sum.activeCount != 0 || sum.last != nil {
		t.Fatalf("expected empty summary, got %+v", sum)
	}
}

// TestPickFollowTargetParkedNotACandidate: a fresh sdlc run is parked
// at design under the parking rule, but with no open stage session it
// is *not* a follow auto-pick candidate. Parked-only runs are work-to-
// do, not work-being-done — `dash` is the surface for those. Auto-pick
// returns no path; the operator sees the idle screen.
func TestPickFollowTargetParkedNotACandidate(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	target, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected idle (parked-only is not a candidate), got %+v", target)
	}
	if sum.activeCount != 1 {
		t.Fatalf("expected the parked run to count as active, got %d", sum.activeCount)
	}
}

// TestPickFollowTargetLiveDesignSession: a run with an open design
// session resolves to (worktree, "main"). Bureaucracy worktree is
// where the agent's edits live until the session rebases onto main at
// close.
func TestPickFollowTargetLiveDesignSession(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sess.WorktreePath {
		t.Fatalf("dir = %q, want session worktree %q", target.Dir, sess.WorktreePath)
	}
	if target.Base != "main" {
		t.Fatalf("base = %q, want main", target.Base)
	}
}

// TestPickFollowTargetLiveCodeSession: a run with an open code session
// resolves to (sandbox dir, project default branch). Code sessions
// edit files inside the sandbox clone, not the bureaucracy worktree —
// the bureaucracy worktree's only artifact during code is the canvas
// summary written near the end, which the merge-decision prompt
// surfaces separately.
func TestPickFollowTargetLiveCodeSession(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeProjectJSONWithDefaults(t, root, "tele", "develop")
	// session.Open opens a code session worktree under bureaucracy;
	// the sandbox clone itself is created by attachRunWorkspace at
	// stage time, but pickFollowTarget only stat's the dir, so
	// faking the dir here is enough.
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })
	sandboxDir := sandbox.Path(root, "tele", "fix-it")
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		t.Fatal(err)
	}

	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sandboxDir {
		t.Fatalf("dir = %q, want sandbox %q", target.Dir, sandboxDir)
	}
	if target.Base != "develop" {
		t.Fatalf("base = %q, want project default branch %q", target.Base, "develop")
	}
}

// TestPickFollowTargetLiveCodeSessionWithoutSandboxIdles: an open code
// session whose sandbox clone hasn't materialised yet must idle rather
// than hand hunk a non-existent cwd. Stat-skip is defense-in-depth: in
// production the sandbox always exists by the time the agent is
// committing, but a botched code-stage open could leave a session
// without a clone.
func TestPickFollowTargetLiveCodeSessionWithoutSandboxIdles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeProjectJSONWithDefaults(t, root, "tele", "main")
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	// No sandbox dir.
	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected idle (sandbox missing), got %+v", target)
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
	// Parked at code; open a design session — design's worktree wins
	// over the parked-at-code state.
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sess.WorktreePath {
		t.Fatalf("dir = %q, want design worktree %q", target.Dir, sess.WorktreePath)
	}
}

// TestPickFollowTargetLiveOnly: two runs — one with an open design
// session, one merely parked at design with newer activity. The live
// run is the only candidate; parked-only runs are invisible to
// auto-pick regardless of recency.
func TestPickFollowTargetLiveOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	// Live: design committed (parked at code), open session on design.
	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitWorkTurnAt(t, root, "tele", "alpha", "sdlc", "design", t0)
	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })

	// Parked-only: fresh run, more recent activity than alpha's design
	// commit but no open session — must not surface.
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	commitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
		t0.Add(2*time.Hour))

	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sessA.WorktreePath {
		t.Fatalf("dir = %q, want alpha worktree %q", target.Dir, sessA.WorktreePath)
	}
}

// TestPickFollowTargetMostRecentLiveWins: two runs each with an open
// design session — the more-recently-active one wins. We backdate
// both runs' latest MoE-Run commit so the journal index's
// topological-walk "first encountered" picks up the controlled
// timestamps rather than the seedRun open commits at real-time-now
// (which would otherwise tie within the same second of wall-clock).
func TestPickFollowTargetMostRecentLiveWins(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	commitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele", t0)
	commitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
		t0.Add(time.Hour))

	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })
	sessB, err := session.Open(root, "tele", "beta", "design")
	if err != nil {
		t.Fatalf("session.Open beta: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessB) })

	target, _, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sessB.WorktreePath {
		t.Fatalf("dir = %q, want beta worktree %q", target.Dir, sessB.WorktreePath)
	}
}

// TestPickFollowTargetRunFilterPinsSpecificRun: --run locks to the
// named run, even when another run would otherwise outrank it under
// recency. Pin behaviour is the design's stated escape hatch for
// "I know which run I want to watch."
func TestPickFollowTargetRunFilterPinsSpecificRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	seedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// alpha is the natural recency winner …
	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })
	commitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele",
		t0.Add(time.Hour))
	// … but pin to beta. Beta needs an open session for the pin to
	// land — pin overrides recency, not liveness.
	sessB, err := session.Open(root, "tele", "beta", "design")
	if err != nil {
		t.Fatalf("session.Open beta: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessB) })

	target, _, err := pickFollowTarget(root, "beta")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sessB.WorktreePath {
		t.Fatalf("dir = %q, want beta worktree %q", target.Dir, sessB.WorktreePath)
	}
}

// TestPickFollowTargetRunFilterWithoutLiveSessionIdles: pinning to a
// run with no open session falls through to the idle screen — pin
// overrides recency but not liveness, so the operator can pin pre-
// emptively and have hunk spawn the moment a session opens.
func TestPickFollowTargetRunFilterWithoutLiveSessionIdles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)

	target, _, err := pickFollowTarget(root, "alpha")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected idle (no live session), got %+v", target)
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

	target, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected no candidate, got %+v", target)
	}
	if sum.activeCount != 1 {
		t.Fatalf("expected 1 active (the pushed run), got %d", sum.activeCount)
	}
	if sum.last == nil || sum.last.run != "shipped" || sum.last.state != "awaiting merge" {
		t.Fatalf("unexpected last: %+v", sum.last)
	}
}

// TestPickFollowTargetIdeaRunsExcluded: idea runs (workflow=idea) are
// backlog, not active. They neither surface as candidates nor inflate
// the activeCount.
func TestPickFollowTargetIdeaRunsExcluded(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	seedRun(t, root, "tele", "captured", "idea", run.StatusInProgress)

	target, sum, err := pickFollowTarget(root, "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != "" {
		t.Fatalf("expected no candidate from idea workflow, got %+v", target)
	}
	if sum.activeCount != 0 {
		t.Fatalf("expected idea runs to skip activeCount, got %d", sum.activeCount)
	}
}

// TestStageRankPrefersCodeOverDesign pins the per-run
// session-disambiguation rule. A run normally has at most one open
// session at a time, but if a botched close leaves an orphan, code
// should win over design (closer to the run's likely live workspace),
// and both should win over an arbitrary other doc.
func TestStageRankPrefersCodeOverDesign(t *testing.T) {
	if stageRank("code") >= stageRank("design") {
		t.Errorf("code rank %d should be < design rank %d", stageRank("code"), stageRank("design"))
	}
	if stageRank("design") >= stageRank("kb") {
		t.Errorf("design rank %d should be < other-stage rank %d", stageRank("design"), stageRank("kb"))
	}
}

// TestIdleLineEmpty: with nothing active, the idle line drops the
// trailing "last:" segment so the operator sees an honest "0 active"
// without a phantom pointer.
func TestIdleLineEmpty(t *testing.T) {
	got := idleLine(followSummary{})
	want := "(no run in play · 0 active)"
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
	want := "(no run in play · 2 active · last: tele/fix-it awaiting merge)"
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

// writeProjectJSONWithDefaults rewrites the minimal seedRun project
// stub with the fields project.Load requires (Remote non-empty) plus
// a chosen default branch. The code-stage routing reads
// DefaultBranch off project.json, so tests that exercise that path
// need a populated record.
func writeProjectJSONWithDefaults(t *testing.T, root, projectID, defaultBranch string) {
	t.Helper()
	body := `{"id":"` + projectID + `","status":"incubating",` +
		`"submodule":"projects/` + projectID + `/src",` +
		`"remote":"https://example.invalid/` + projectID + `.git",` +
		`"default_branch":"` + defaultBranch + `","created":"2026-04-01"}`
	path := filepath.Join(root, "projects", projectID, "project.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
