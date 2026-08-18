package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// activeRowFor returns the ACTIVE row a gathered snapshot carries for
// project/slug, or nil.
func activeRowFor(t *testing.T, root, project, slug string) *dash.Row {
	t.Helper()
	snap, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{})
	if err != nil {
		t.Fatalf("GatherDashSnapshot: %v", err)
	}
	for _, r := range snap.Rows {
		if r.Bucket == dash.BucketActiveRuns && r.Project == project && r.Run == slug {
			return &r
		}
	}
	return nil
}

// TestGatherDashSnapshotIntentChatSessionRow drives the whole path an
// `intent edit --chat` session takes to the dash: a real session
// worktree, a real turn commit on the session branch, and the gatherer
// reading both back. The date assertion is the point — the run's journal
// activity is twelve days old on main, because session commits don't
// land there until close, so a row dated from LastActivity would call a
// minutes-old session stale.
func TestGatherDashSnapshotIntentChatSessionRow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)

	trailerstest.SeedRun(t, root, "tele", "sharpen-me", dash.IntentWorkflow, run.StatusInProgress)
	writeContent(t, root, "tele", "sharpen-me", dash.IntentDocID, "# Sharpen me\n")
	stale := time.Now().UTC().Add(-12 * 24 * time.Hour)
	trailerstest.CommitWorkTurnAt(t, root, "tele", "sharpen-me", dash.IntentWorkflow, dash.IntentDocID, stale)

	// Idle intent: standing row only.
	if got := activeRowFor(t, root, "tele", "sharpen-me"); got != nil {
		t.Fatalf("idle intent has an ACTIVE row: %+v", got)
	}

	sess, err := session.Open(root, "tele", "sharpen-me", dash.IntentDocID)
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Abandon(sess) })
	// A turn's commit on the session branch, which is what moves the tip
	// past the stale main-side history.
	gittest.Run(t, sess.WorktreePath, "commit", "--allow-empty", "-m", "work: start session for intent")

	got := activeRowFor(t, root, "tele", "sharpen-me")
	if got == nil {
		t.Fatal("open intent session has no ACTIVE row")
	}
	if got.Note != "intent:edit --chat [running]" {
		t.Errorf("note = %q, want %q", got.Note, "intent:edit --chat [running]")
	}
	if got.RunningDoc != dash.IntentDocID {
		t.Errorf("runningDoc = %q, want %q", got.RunningDoc, dash.IntentDocID)
	}
	if !got.When.After(stale.Add(24 * time.Hour)) {
		t.Errorf("when = %v, want the session branch tip (well after the stale main history at %v)", got.When, stale)
	}

	// The standing INTENTS entry survives alongside it — the run renders
	// twice while the session is open, by design.
	snap, err := GatherDashSnapshot(root, time.Now().UTC(), DashFilter{})
	if err != nil {
		t.Fatalf("GatherDashSnapshot: %v", err)
	}
	var standing int
	for _, r := range snap.Rows {
		if r.Bucket == dash.BucketIntents && r.Run == "sharpen-me" {
			standing++
		}
	}
	if standing != 1 {
		t.Errorf("INTENTS rows for sharpen-me = %d, want 1", standing)
	}

	// And it survives the real render — the factory art has no station
	// glyph for an "intent" stage, so this also pins that the unknown
	// name falls through to the generic boiler rather than panicking.
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := Run([]string{"dash"}, &out, &errb); code != 0 {
		t.Fatalf("dash exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "intent:edit --chat [running]") {
		t.Fatalf("rendered dash missing the live-session row:\n%s", out.String())
	}

	if err := session.Abandon(sess); err != nil {
		t.Fatalf("session.Abandon: %v", err)
	}
	if got := activeRowFor(t, root, "tele", "sharpen-me"); got != nil {
		t.Fatalf("ACTIVE row survives session abandon: %+v", got)
	}
}
