package dash

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

func ideaRun(project, id, status string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: IdeaWorkflow, Status: status}
}

func backlogRows(t *testing.T, runs []*run.Metadata, when map[string]time.Time, picks []PullNextPick) []Row {
	t.Helper()
	idx := &run.JournalIndex{LastActivity: when}
	rows, err := BuildRows(Inputs{Now: time.Now().UTC(), Runs: runs, Index: idx, PullNext: picks})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var backlog []Row
	for _, r := range rows {
		if r.Bucket == BucketBacklog {
			backlog = append(backlog, r)
		}
	}
	return backlog
}

// TestFloatPullNextOrdersAndAnnotates: the pulse's ranked picks lead the
// BACKLOG in report order, each carrying its reason as the note; an
// unpicked open idea keeps its recency slot after the picks and its
// plain idea:capture note.
func TestFloatPullNextOrdersAndAnnotates(t *testing.T) {
	runs := []*run.Metadata{
		ideaRun("moe", "aaa", run.StatusInProgress),
		ideaRun("moe", "bbb", run.StatusInProgress),
		ideaRun("moe", "ccc", run.StatusInProgress),
	}
	when := map[string]time.Time{
		"moe/aaa": time.Unix(300, 0),
		"moe/bbb": time.Unix(200, 0),
		"moe/ccc": time.Unix(100, 0),
	}
	// Report ranks ccc first, then aaa. bbb is unranked.
	picks := []PullNextPick{
		{Project: "moe", Slug: "ccc", Reason: "unblocked by the merge"},
		{Project: "moe", Slug: "aaa", Reason: "quick win"},
	}
	backlog := backlogRows(t, runs, when, picks)

	var got []string
	for _, r := range backlog {
		got = append(got, r.Run)
	}
	want := []string{"ccc", "aaa", "bbb"}
	if len(got) != len(want) {
		t.Fatalf("backlog order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backlog order = %v, want %v", got, want)
		}
	}
	if backlog[0].Note != "pull: unblocked by the merge" {
		t.Errorf("first pick note = %q, want pull reason", backlog[0].Note)
	}
	if backlog[1].Note != "pull: quick win" {
		t.Errorf("second pick note = %q, want pull reason", backlog[1].Note)
	}
	if backlog[2].Note != "idea:capture" {
		t.Errorf("unpicked note = %q, want idea:capture", backlog[2].Note)
	}
}

// TestFloatPullNextDropsPromotedAndClosedPicks: a pick whose idea has
// been promoted or closed has no backlog row to float, so it drops out
// silently — a stale report can only over-highlight, never resurrect.
func TestFloatPullNextDropsPromotedAndClosedPicks(t *testing.T) {
	runs := []*run.Metadata{
		ideaRun("moe", "still-open", run.StatusInProgress),
		ideaRun("moe", "handed-off", run.StatusPromoted),
		ideaRun("moe", "dropped", run.StatusClosed),
	}
	when := map[string]time.Time{
		"moe/still-open": time.Unix(300, 0),
		"moe/handed-off": time.Unix(200, 0),
		"moe/dropped":    time.Unix(100, 0),
	}
	picks := []PullNextPick{
		{Project: "moe", Slug: "handed-off", Reason: "stale — already promoted"},
		{Project: "moe", Slug: "still-open", Reason: "do this next"},
		{Project: "moe", Slug: "dropped", Reason: "stale — already closed"},
	}
	backlog := backlogRows(t, runs, when, picks)

	if len(backlog) != 1 {
		t.Fatalf("backlog rows = %d, want 1 (only the still-open idea)", len(backlog))
	}
	if backlog[0].Run != "still-open" {
		t.Fatalf("backlog[0] = %q, want still-open", backlog[0].Run)
	}
	if backlog[0].Note != "pull: do this next" {
		t.Errorf("note = %q, want the pull reason", backlog[0].Note)
	}
}

// TestFloatPullNextNoPicksIsInert: an empty pick slice (no pulse report,
// or a report with no Pull next entries) leaves BACKLOG in recency order
// with plain notes.
func TestFloatPullNextNoPicksIsInert(t *testing.T) {
	runs := []*run.Metadata{
		ideaRun("moe", "newer", run.StatusInProgress),
		ideaRun("moe", "older", run.StatusInProgress),
	}
	when := map[string]time.Time{
		"moe/newer": time.Unix(200, 0),
		"moe/older": time.Unix(100, 0),
	}
	backlog := backlogRows(t, runs, when, nil)
	if len(backlog) != 2 || backlog[0].Run != "newer" || backlog[1].Run != "older" {
		t.Fatalf("backlog = %v, want recency order [newer older]", backlog)
	}
	if backlog[0].Note != "idea:capture" {
		t.Errorf("note = %q, want plain idea:capture", backlog[0].Note)
	}
}
