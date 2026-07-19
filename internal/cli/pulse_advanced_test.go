package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// advanceAt seeds the timeline of a run the operator clicked forward at
// docID's chain prompt: a work-turn for docID, then an advance marker a
// minute later. That is exactly what `moe sdlc design` followed by `a`
// leaves in the journal.
func advanceAt(t *testing.T, root, projectID, runID, docID string, when time.Time) {
	t.Helper()
	trailerstest.CommitWorkTurnAt(t, root, projectID, runID, "sdlc", docID, when)
	trailerstest.CommitAdvanceAt(t, root, projectID, runID, "sdlc", docID, when.Add(time.Minute))
}

// TestAdvancedRunsBlockListsAdvancedRun is the happy path: the run the
// stall was observed on. Design committed, operator hit `a`, nothing
// picked it up — the block names it, the stage it waits at, and the
// marker's age.
func TestAdvancedRunsBlockListsAdvancedRun(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "evolution", "sdlc", run.StatusInProgress, now,
		map[string]string{"design": "# Evolve the ladder\n\nbody\n"})
	advanceAt(t, root, "moe", "evolution", "design", now.Add(-3*time.Hour))

	got := advancedRunsBlock(root, "moe")
	for _, want := range []string{"`evolution` (sdlc)", "waiting at **code**", "Evolve the ladder"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
}

// TestAdvancedRunsBlockSkipsUnadvancedRun pins the deliberate exclusion
// the design turns on: a run whose design canvas is complete but which
// carries no marker is *not* eligible. A finished canvas is not consent
// to proceed, and an in-flight run must never be groomed underneath.
func TestAdvancedRunsBlockSkipsUnadvancedRun(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "in-flight", "sdlc", run.StatusInProgress, now, nil)
	trailerstest.CommitWorkTurnAt(t, root, "moe", "in-flight", "sdlc", "design", now.Add(-time.Hour))

	if got := advancedRunsBlock(root, "moe"); got != "" {
		t.Errorf("a run with no advance marker must not be eligible:\n%s", got)
	}
}

// TestAdvancedRunsBlockSkipsStartedSuccessor is the double-run guard.
// The operator hit `a` and later started `moe sdlc code` by hand; a
// pulse fired by unrelated traffic mid-session must not still see the
// run as waiting. The session id is what makes that visible — it is
// committed before the agent runs, well ahead of any code work-turn.
func TestAdvancedRunsBlockSkipsStartedSuccessor(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "picked-up", "sdlc", run.StatusInProgress, now, nil)
	advanceAt(t, root, "moe", "picked-up", "design", now.Add(-2*time.Hour))
	if got := advancedRunsBlock(root, "moe"); !strings.Contains(got, "picked-up") {
		t.Fatalf("precondition: run should be eligible before its code session starts:\n%s", got)
	}

	// `moe sdlc code` mints the session and commits run.json before the
	// agent turn — no canvas edit, no work-turn yet.
	md, err := run.Load(root, "moe", "picked-up")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
		t.Fatal(err)
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := commitSessionStart(root, md, "code"); err != nil {
		t.Fatal(err)
	}

	if got := advancedRunsBlock(root, "moe"); got != "" {
		t.Errorf("a run whose successor session already started must leave pickup range:\n%s", got)
	}
}

// TestAdvancedRunsBlockSkipsReopenedStage: a marker out-dated by a
// fresher turn on its own stage is inert, the same rule stageSatisfied
// applies. The operator advanced past design, then re-opened design —
// the run is being worked again, not waiting.
func TestAdvancedRunsBlockSkipsReopenedStage(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "reopened", "sdlc", run.StatusInProgress, now, nil)
	advanceAt(t, root, "moe", "reopened", "design", now.Add(-5*time.Hour))
	trailerstest.CommitWorkTurnAt(t, root, "moe", "reopened", "sdlc", "design", now.Add(-time.Hour))

	if got := advancedRunsBlock(root, "moe"); got != "" {
		t.Errorf("a re-opened stage out-dates its marker:\n%s", got)
	}
}

// TestAdvancedRunsBlockSkipsTerminalRun: a merged run carries its
// design-stage marker forever. Terminal status short-circuits in
// NextWithIndex, so it never reads as waiting.
func TestAdvancedRunsBlockSkipsTerminalRun(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "shipped", "sdlc", run.StatusMerged, now, nil)
	advanceAt(t, root, "moe", "shipped", "design", now.Add(-4*time.Hour))

	if got := advancedRunsBlock(root, "moe"); got != "" {
		t.Errorf("a merged run must not read as advanced-and-waiting:\n%s", got)
	}
}

// TestAdvancedRunsBlockOrdersOldestFirst and scopes to the project.
// Oldest marker first: the longest-stranded run is the one most worth
// a thread. A run in another project is not this sweep's business.
func TestAdvancedRunsBlockOrdersOldestFirst(t *testing.T) {
	root := newTestBureaucracy(t)
	now := time.Now().Local()
	seedRun(t, root, "moe", "recent", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "moe", "stranded", "sdlc", run.StatusInProgress, now, nil)
	seedRun(t, root, "other", "foreign", "sdlc", run.StatusInProgress, now, nil)
	advanceAt(t, root, "moe", "recent", "design", now.Add(-2*time.Hour))
	advanceAt(t, root, "moe", "stranded", "design", now.Add(-72*time.Hour))
	advanceAt(t, root, "other", "foreign", "design", now.Add(-time.Hour))

	got := advancedRunsBlock(root, "moe")
	si, ri := strings.Index(got, "`stranded`"), strings.Index(got, "`recent`")
	if si < 0 || ri < 0 || si > ri {
		t.Errorf("expected the older marker first:\n%s", got)
	}
	if strings.Contains(got, "foreign") {
		t.Errorf("block lists another project's run:\n%s", got)
	}
}

// TestAdvancedRunsBlockEmpty: nothing advanced means no block at all,
// consistent with every sibling in pulseKickoffWithContext. An empty
// block with a header reads as a bug.
func TestAdvancedRunsBlockEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	seedRun(t, root, "moe", "lone-run", "sdlc", run.StatusInProgress, time.Now().Local(), nil)
	if got := advancedRunsBlock(root, "moe"); got != "" {
		t.Errorf("expected no block, got:\n%s", got)
	}
}
