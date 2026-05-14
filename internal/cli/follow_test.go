package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// TestPickFollowTargetEmpty: no runs registered → no candidate, no
// "last:" entry in the idle summary. The empty-bureaucracy state must
// be a clean idle screen.
func TestPickFollowTargetEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	target, sum, err := pickFollowTarget(root, "", "")
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

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)

	target, sum, err := pickFollowTarget(root, "", "")
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
// session resolves to (worktree, merge-base(HEAD, main)). Bureaucracy
// worktree is where the agent's edits live until the session rebases
// onto main at close; the merge-base anchors the diff at the
// divergence point so unrelated runs that landed on main between
// turns stay below the base.
func TestPickFollowTargetLiveDesignSession(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	wantBase, err := git.RevParse(sess.WorktreePath, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse session HEAD: %v", err)
	}

	target, _, err := pickFollowTarget(root, "", "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sess.WorktreePath {
		t.Fatalf("dir = %q, want session worktree %q", target.Dir, sess.WorktreePath)
	}
	if target.Base != wantBase {
		t.Fatalf("base = %q, want merge-base %q", target.Base, wantBase)
	}
}

// TestPickFollowTargetLiveCodeSession: a run with an open code session
// resolves to (sandbox dir, merge-base(HEAD, default-branch)). Code
// sessions edit files inside the sandbox worktree, not the
// bureaucracy worktree — the bureaucracy worktree's only artifact
// during code is the canvas summary written near the end, which the
// merge-decision prompt surfaces separately. The merge-base anchors
// the diff at the session-open commit so a moving default branch
// (under the worktree primitive the sandbox shares the canonical's
// ref DB) doesn't drag unrelated commits into the diff.
func TestPickFollowTargetLiveCodeSession(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeProjectJSONWithDefaults(t, root, "tele", "develop")
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })
	// Fake a real git repo at the sandbox path so merge-base resolves
	// — production attaches a worktree here, but the follow path only
	// runs git ops, not sandbox ops.
	sandboxDir := sandbox.Path(root, "tele", "fix-it")
	if err := os.MkdirAll(sandboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, sandboxDir, "init", "-b", "develop")
	gittest.Run(t, sandboxDir, "commit", "--allow-empty", "-m", "seed")
	wantBase, err := git.RevParse(sandboxDir, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse sandbox HEAD: %v", err)
	}

	target, _, err := pickFollowTarget(root, "", "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sandboxDir {
		t.Fatalf("dir = %q, want sandbox %q", target.Dir, sandboxDir)
	}
	if target.Base != wantBase {
		t.Fatalf("base = %q, want merge-base %q", target.Base, wantBase)
	}
	wantCanvas := filepath.Join(sess.WorktreePath, run.ContentPath("tele", "fix-it", "code"))
	if target.Canvas != wantCanvas {
		t.Fatalf("canvas = %q, want %q", target.Canvas, wantCanvas)
	}
	if strings.HasPrefix(target.Canvas, sandboxDir) {
		t.Fatalf("canvas %q must not sit under sandbox %q", target.Canvas, sandboxDir)
	}
}

// TestPickFollowTargetLiveCodeSessionWithoutSandboxIdles: an open code
// session whose sandbox clone hasn't materialised yet must idle rather
// than resolve to a non-existent cwd. Stat-skip is defense-in-depth: in
// production the sandbox always exists by the time the agent is
// committing, but a botched code-stage open could leave a session
// without a clone.
func TestPickFollowTargetLiveCodeSessionWithoutSandboxIdles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	writeProjectJSONWithDefaults(t, root, "tele", "main")
	sess, err := session.Open(root, "tele", "fix-it", "code")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	// No sandbox dir.
	target, _, err := pickFollowTarget(root, "", "")
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

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "design", t0)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "fix-it", "sdlc", "code", t0.Add(time.Hour))
	// Parked at code; open a design session — design's worktree wins
	// over the parked-at-code state.
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	target, _, err := pickFollowTarget(root, "", "")
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
	trailerstest.SeedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "alpha", "sdlc", "design", t0)
	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })

	// Parked-only: fresh run, more recent activity than alpha's design
	// commit but no open session — must not surface.
	trailerstest.SeedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	trailerstest.CommitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
		t0.Add(2*time.Hour))

	target, _, err := pickFollowTarget(root, "", "")
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

	trailerstest.SeedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	trailerstest.CommitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele", t0)
	trailerstest.CommitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: tele",
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

	target, _, err := pickFollowTarget(root, "", "")
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

	trailerstest.SeedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "tele", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// alpha is the natural recency winner …
	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })
	trailerstest.CommitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele",
		t0.Add(time.Hour))
	// … but pin to beta. Beta needs an open session for the pin to
	// land — pin overrides recency, not liveness.
	sessB, err := session.Open(root, "tele", "beta", "design")
	if err != nil {
		t.Fatalf("session.Open beta: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessB) })

	target, _, err := pickFollowTarget(root, "", "beta")
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
// emptively and the wrapper loop will resolve once a session opens.
func TestPickFollowTargetRunFilterWithoutLiveSessionIdles(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)

	target, _, err := pickFollowTarget(root, "", "alpha")
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

	trailerstest.SeedRun(t, root, "tele", "merged-one", "sdlc", run.StatusMerged)
	trailerstest.SeedRun(t, root, "tele", "shipped", "sdlc", run.StatusPushed)

	target, sum, err := pickFollowTarget(root, "", "")
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

	trailerstest.SeedRun(t, root, "tele", "captured", "idea", run.StatusInProgress)

	target, sum, err := pickFollowTarget(root, "", "")
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

// TestPickFollowTargetProjectFilterNarrowsCandidates: --project drops
// candidates from other projects so the operator can pin attention to
// one project's runs without naming an individual run id. The natural
// recency winner is in project "other" here; with --project tele the
// resolver must skip it and pick alpha (tele's only live candidate).
func TestPickFollowTargetProjectFilterNarrowsCandidates(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "alpha", "sdlc", run.StatusInProgress)
	trailerstest.SeedRun(t, root, "other", "beta", "sdlc", run.StatusInProgress)
	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// alpha first, beta later → beta is the natural recency winner.
	trailerstest.CommitTrailer(t, root, "touch alpha", "MoE-Run: alpha\nMoE-Project: tele", t0)
	trailerstest.CommitTrailer(t, root, "touch beta", "MoE-Run: beta\nMoE-Project: other",
		t0.Add(time.Hour))

	sessA, err := session.Open(root, "tele", "alpha", "design")
	if err != nil {
		t.Fatalf("session.Open alpha: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessA) })
	sessB, err := session.Open(root, "other", "beta", "design")
	if err != nil {
		t.Fatalf("session.Open beta: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sessB) })

	target, _, err := pickFollowTarget(root, "tele", "")
	if err != nil {
		t.Fatalf("pickFollowTarget: %v", err)
	}
	if target.Dir != sessA.WorktreePath {
		t.Fatalf("dir = %q, want tele/alpha worktree %q", target.Dir, sessA.WorktreePath)
	}
}

// TestResolveFollowTargetUsesMergeBaseForNonCode: hunk's base for a
// non-code session is `merge-base(HEAD, main)` resolved inside the
// session worktree — the commit at which the session branch diverged
// from main. That anchor is stable across the close→reopen boundary
// resume relies on (the worktree is re-created off main HEAD on
// resume, and the merge base moves with it), so unrelated runs that
// landed on main between turns stay below the base and out of the
// diff. The followTarget also carries identity (workflow, stage,
// project, run) for the human/shell printers — a live target without
// those fields would render anonymous output.
func TestResolveFollowTargetUsesMergeBaseForNonCode(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	// Capture the divergence point — main's HEAD at the moment the
	// session worktree was created off it. resolveFollowTarget must
	// report this SHA, not main itself.
	wantBase, err := git.RevParse(sess.WorktreePath, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse session HEAD: %v", err)
	}

	// Land an unrelated commit on main *after* the session opened.
	// merge-base must stay anchored at the divergence point (not
	// advance to the new main HEAD), so the unrelated commit doesn't
	// retroactively appear in the diff base.
	trailerstest.CommitTrailer(t, root, "unrelated work on main", "MoE-Run: other\n", time.Time{})

	md := &run.Metadata{
		ID: "fix-it", Project: "tele", Workflow: "sdlc",
	}
	target, err := resolveFollowTarget(root, md, sess)
	if err != nil {
		t.Fatalf("resolveFollowTarget: %v", err)
	}
	if target.Dir != sess.WorktreePath {
		t.Fatalf("dir = %q, want %q", target.Dir, sess.WorktreePath)
	}
	if target.Base != wantBase {
		t.Fatalf("base = %q, want merge-base %q", target.Base, wantBase)
	}
	if target.Workflow != "sdlc" || target.Stage != "design" ||
		target.Project != "tele" || target.Run != "fix-it" {
		t.Fatalf("identity = %+v, want sdlc/design/tele/fix-it", target)
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
	want := "(no run in play · 2 active · last: tele fix-it awaiting merge)"
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

// TestRunFollowDefaultHumanForm: the no-flags form prints the
// multi-line summary on stdout and exits 0. Pins the seven-line
// labeled shape — one fact per row, all colons aligned to the longest
// label (`workflow:`). The line-by-line ordering is the stable
// contract, not byte-for-byte equality, so we assert on substrings.
func TestRunFollowDefaultHumanForm(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var stdout, stderr bytes.Buffer
	code := runFollow(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	wantContent := filepath.Join(sess.WorktreePath, run.ContentPath("tele", "fix-it", "design"))
	for _, want := range []string{
		"project:  tele",
		"run:      fix-it",
		"workflow: sdlc",
		"stage:    design",
		"canvas:   " + wantContent,
		"base:     ",
		"dir:      " + sess.WorktreePath,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestRunFollowPathOnly: --path prints exactly the absolute canvas
// path on stdout, no other lines. The output must be safe inside
// $(moe follow --path) — a single \n-terminated line.
func TestRunFollowPathOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--path"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	want := filepath.Join(sess.WorktreePath, run.ContentPath("tele", "fix-it", "design")) + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("--path stdout = %q, want %q", got, want)
	}
	if !filepath.IsAbs(strings.TrimSpace(stdout.String())) {
		t.Fatalf("--path must be absolute, got %q", stdout.String())
	}
}

// TestRunFollowBaseOnly: --base prints exactly the diff base SHA, one
// line. For a non-code stage that's the worktree's
// merge-base(HEAD, main).
func TestRunFollowBaseOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	wantBase, err := git.RevParse(sess.WorktreePath, "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--base"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != wantBase+"\n" {
		t.Fatalf("--base stdout = %q, want %q", got, wantBase+"\n")
	}
}

// TestRunFollowDirOnly: --dir prints exactly the absolute workspace
// dir, one line. For a design session that's the bureaucracy worktree
// path.
func TestRunFollowDirOnly(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--dir"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != sess.WorktreePath+"\n" {
		t.Fatalf("--dir stdout = %q, want %q", got, sess.WorktreePath+"\n")
	}
}

// TestRunFollowShellEvalsCleanly: --shell output must round-trip
// through `bash -c 'eval "$1"; echo "$MOE_FOLLOW_PATH"' _ "$out"`
// against a fixture path containing both a space and an apostrophe.
// The escape regression we're guarding against is the standard POSIX
// dance for an apostrophe inside a single-quoted string — close,
// backslash-quote, reopen — without which a naive double-quote wrap
// would leak the apostrophe and break eval. This test is the
// wire-format guarantee that wrappers can rely on.
//
// We can't easily inject an apostrophe into a real run id (slug
// characters are constrained), so we exercise shellQuote directly
// against the awkward fixture and assert eval-roundtrip semantics.
// The end-to-end --shell test below covers the full pipe.
func TestRunFollowShellEvalsCleanly(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	awkward := "/tmp/has space/and 'quote'/file.md"
	script := `eval "$1"; printf '%s' "$VAR"`
	got, err := exec.Command("bash", "-c", script, "_", "VAR="+shellQuote(awkward)).Output()
	if err != nil {
		t.Fatalf("bash eval: %v", err)
	}
	if string(got) != awkward {
		t.Fatalf("eval round-trip: got %q want %q", string(got), awkward)
	}
}

// TestRunFollowShell: --shell emits all seven MOE_FOLLOW_*
// assignments, one per line, and the values are eval-safe. Pipe the
// output through bash -c 'eval' and assert MOE_FOLLOW_PATH comes back
// matching the canvas path.
func TestRunFollowShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	trailerstest.SeedRun(t, root, "tele", "fix-it", "sdlc", run.StatusInProgress)
	sess, err := session.Open(root, "tele", "fix-it", "design")
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--shell"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{
		"MOE_FOLLOW_PATH=",
		"MOE_FOLLOW_BASE=",
		"MOE_FOLLOW_DIR=",
		"MOE_FOLLOW_PROJECT=",
		"MOE_FOLLOW_RUN=",
		"MOE_FOLLOW_STAGE=",
		"MOE_FOLLOW_WORKFLOW=",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q in --shell output:\n%s", want, stdout.String())
		}
	}

	// End-to-end: pipe through eval and confirm MOE_FOLLOW_PATH.
	script := `eval "$1"; printf '%s' "$MOE_FOLLOW_PATH"`
	out, err := exec.Command("bash", "-c", script, "_", stdout.String()).Output()
	if err != nil {
		t.Fatalf("bash eval: %v", err)
	}
	want := filepath.Join(sess.WorktreePath, run.ContentPath("tele", "fix-it", "design"))
	if string(out) != want {
		t.Fatalf("eval MOE_FOLLOW_PATH = %q, want %q", string(out), want)
	}
}

// TestRunFollowIdleNonZeroExit: with nothing live, the idle line
// lands on stderr and exit code is 1 so a `while p=$(moe follow
// --path); do …` shell loop terminates. Stdout must be empty so a
// caller using --path doesn't accidentally feed an idle-status
// sentence into less.
func TestRunFollowIdleNonZeroExit(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--path"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty on idle, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no run in play") {
		t.Fatalf("idle line missing from stderr: %q", stderr.String())
	}
}

// TestRunFollowMutuallyExclusiveFlags: --path/--base/--dir/--shell
// can't combine. The CLI rejects with exit 2 and a usage error; this
// keeps the contract that a single invocation produces one output
// shape, so wrapper scripts don't have to defensively pick.
func TestRunFollowMutuallyExclusiveFlags(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var stdout, stderr bytes.Buffer
	code := runFollow([]string{"--path", "--base"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("expected 'mutually exclusive' in stderr, got %q", stderr.String())
	}
}

// TestShellQuoteRoundTrips pins the POSIX single-quote escape contract
// directly. Belt-and-braces alongside TestRunFollowShellEvalsCleanly:
// even without bash on PATH, the shape of the escape (no spaces, no
// double-quote tricks, just the close / backslash-quote / reopen
// dance from shellQuote) is the wire format.
func TestShellQuoteRoundTrips(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", `'plain'`},
		{"with space", `'with space'`},
		{"it's mine", `'it'\''s mine'`},
		{"", `''`},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
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
