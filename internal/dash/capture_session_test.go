package dash

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// The capture-session row is the one place a run renders twice on
// purpose: an idea or intent with an open `edit --chat` session keeps
// its standing entry (BACKLOG / INTENTS) *and* earns an ACTIVE row for
// the live session. These tests pin both halves — the standing row must
// survive, and the ACTIVE row must only appear when a session is really
// open on an unsettled capture run.

// captureInputs assembles the Inputs a capture run needs: the run
// itself, whatever session docs are open on it, and the two date
// sources the row chooses between.
func captureInputs(md *run.Metadata, openDocs []string, sessionWhen, lastActivity time.Time) Inputs {
	key := md.Project + "/" + md.ID
	in := Inputs{
		Now:   time.Now().UTC(),
		Runs:  []*run.Metadata{md},
		Index: &run.JournalIndex{LastActivity: map[string]time.Time{}},
	}
	if !lastActivity.IsZero() {
		in.Index.LastActivity[key] = lastActivity
	}
	if len(openDocs) > 0 {
		in.SessionDocsByRun = map[string][]string{key: openDocs}
	}
	if !sessionWhen.IsZero() {
		in.SessionWhenByRun = map[string]time.Time{key: sessionWhen}
	}
	return in
}

func rowsInBucket(t *testing.T, in Inputs, b Bucket) []Row {
	t.Helper()
	rows, err := BuildRows(in)
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var out []Row
	for _, r := range rows {
		if r.Bucket == b {
			out = append(out, r)
		}
	}
	return out
}

// TestCaptureSessionRowIntentKeepsStandingRow: an open intent session
// adds an ACTIVE row carrying the resume verb, the canvas it's on, and
// the session date — without disturbing the INTENTS entry.
func TestCaptureSessionRowIntentKeepsStandingRow(t *testing.T) {
	md := &run.Metadata{ID: "sharpen-me", Project: "tele", Workflow: IntentWorkflow, Status: run.StatusInProgress}
	sessionWhen := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	in := captureInputs(md, []string{IntentDocID}, sessionWhen, time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC))
	in.Intents = []IntentInput{{Project: "tele", Slug: "sharpen-me", Title: "Sharpen me"}}

	active := rowsInBucket(t, in, BucketActiveRuns)
	if len(active) != 1 {
		t.Fatalf("active rows = %d, want 1: %+v", len(active), active)
	}
	got := active[0]
	if got.Project != "tele" || got.Run != "sharpen-me" {
		t.Errorf("active row = %s/%s, want tele/sharpen-me", got.Project, got.Run)
	}
	if got.Note != "intent:edit --chat [running]" {
		t.Errorf("note = %q, want %q", got.Note, "intent:edit --chat [running]")
	}
	if got.Stage != IntentDocID || got.RunningDoc != IntentDocID {
		t.Errorf("stage/runningDoc = %q/%q, want %q/%q", got.Stage, got.RunningDoc, IntentDocID, IntentDocID)
	}
	if !got.When.Equal(sessionWhen) {
		t.Errorf("when = %v, want session branch tip %v", got.When, sessionWhen)
	}

	intents := rowsInBucket(t, in, BucketIntents)
	if len(intents) != 1 || intents[0].Note != "Sharpen me" {
		t.Fatalf("standing INTENTS row disturbed: %+v", intents)
	}
}

// TestCaptureSessionRowIdeaKeepsBacklogRow: same for an idea, whose
// standing row lives in BACKLOG via classify rather than the splice.
func TestCaptureSessionRowIdeaKeepsBacklogRow(t *testing.T) {
	md := &run.Metadata{ID: "half-thought", Project: "moe", Workflow: IdeaWorkflow, Status: run.StatusInProgress}
	in := captureInputs(md, []string{IdeaDocID}, time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC), time.Time{})

	active := rowsInBucket(t, in, BucketActiveRuns)
	if len(active) != 1 || active[0].Note != "idea:edit --chat [running]" {
		t.Fatalf("active rows = %+v, want one idea:edit --chat row", active)
	}
	if active[0].Stage != IdeaDocID || active[0].RunningDoc != IdeaDocID {
		t.Errorf("stage/runningDoc = %q/%q, want %q both", active[0].Stage, active[0].RunningDoc, IdeaDocID)
	}
	backlog := rowsInBucket(t, in, BucketBacklog)
	if len(backlog) != 1 || backlog[0].Note != "idea:capture" {
		t.Fatalf("standing BACKLOG row disturbed: %+v", backlog)
	}
}

// TestCaptureSessionRowAbsentWithoutSession: an idle capture run renders
// exactly as before — the row is liveness, not decoration. A session
// open on some *other* doc id doesn't count either.
func TestCaptureSessionRowAbsentWithoutSession(t *testing.T) {
	md := &run.Metadata{ID: "half-thought", Project: "moe", Workflow: IdeaWorkflow, Status: run.StatusInProgress}
	for _, docs := range [][]string{nil, {"design"}} {
		in := captureInputs(md, docs, time.Time{}, time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC))
		if active := rowsInBucket(t, in, BucketActiveRuns); len(active) != 0 {
			t.Fatalf("open docs %v: active rows = %+v, want none", docs, active)
		}
	}
}

// TestCaptureSessionRowSkipsSettledRuns: a stray session branch left on
// a closed or promoted capture run gets no ACTIVE row. Cleaning strays
// up is the harvest fix's job; an ACTIVE row inviting a resume on a
// settled run advertises the wrong action.
func TestCaptureSessionRowSkipsSettledRuns(t *testing.T) {
	for _, status := range []string{run.StatusClosed, run.StatusPromoted} {
		md := &run.Metadata{ID: "half-thought", Project: "moe", Workflow: IdeaWorkflow, Status: status}
		in := captureInputs(md, []string{IdeaDocID}, time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC), time.Time{})
		if active := rowsInBucket(t, in, BucketActiveRuns); len(active) != 0 {
			t.Fatalf("status %s: active rows = %+v, want none", status, active)
		}
	}
}

// TestCaptureSessionRowFallsBackToLastActivity: the branch-tip read is
// best-effort, so a missing entry must still render a row — dated from
// the journal, possibly stale. Degraded, not dropped.
func TestCaptureSessionRowFallsBackToLastActivity(t *testing.T) {
	md := &run.Metadata{ID: "sharpen-me", Project: "tele", Workflow: IntentWorkflow, Status: run.StatusInProgress}
	last := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	in := captureInputs(md, []string{IntentDocID}, time.Time{}, last)

	active := rowsInBucket(t, in, BucketActiveRuns)
	if len(active) != 1 {
		t.Fatalf("active rows = %d, want 1", len(active))
	}
	if !active[0].When.Equal(last) {
		t.Errorf("when = %v, want LastActivity fallback %v", active[0].When, last)
	}
}

// TestCaptureSessionRowRespectsWorkflowFilter: the row is run traffic,
// emitted from the run loop, so `moe dash -w sdlc` drops it — unlike the
// standing INTENTS splice, which ignores the filter.
func TestCaptureSessionRowRespectsWorkflowFilter(t *testing.T) {
	md := &run.Metadata{ID: "sharpen-me", Project: "tele", Workflow: IntentWorkflow, Status: run.StatusInProgress}
	in := captureInputs(md, []string{IntentDocID}, time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC), time.Time{})
	in.WorkflowFilter = "sdlc"
	if active := rowsInBucket(t, in, BucketActiveRuns); len(active) != 0 {
		t.Fatalf("active rows under -w sdlc = %+v, want none", active)
	}
}

// TestCaptureSessionRowSortsAboveOlderActive: a freshly opened session
// rides the ordinary bucket-then-recency sort, so it lands at the top of
// ACTIVE rather than wherever the capture run happens to sit.
func TestCaptureSessionRowSortsAboveOlderActive(t *testing.T) {
	intent := &run.Metadata{ID: "sharpen-me", Project: "tele", Workflow: IntentWorkflow, Status: run.StatusInProgress}
	sdlc := &run.Metadata{ID: "older-work", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	in := Inputs{
		Now:  time.Now().UTC(),
		Runs: []*run.Metadata{sdlc, intent},
		Index: &run.JournalIndex{LastActivity: map[string]time.Time{
			"tele/older-work": time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
		}},
		SessionDocsByRun: map[string][]string{"tele/sharpen-me": {IntentDocID}},
		SessionWhenByRun: map[string]time.Time{"tele/sharpen-me": time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)},
		NextByRun:        map[string]NextDecision{"tele/older-work": {Stage: "code"}},
	}
	active := rowsInBucket(t, in, BucketActiveRuns)
	if len(active) != 2 {
		t.Fatalf("active rows = %d, want 2: %+v", len(active), active)
	}
	if active[0].Run != "sharpen-me" {
		t.Errorf("active order = %s,%s; want the fresh session first", active[0].Run, active[1].Run)
	}
}
