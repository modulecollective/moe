package dash

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// TestBuildRowsChoresHeadTheBacklog: a due chore is pending, unopened
// work, so it sorts into the backlog region — after ACTIVE and INTENTS,
// ahead of the idea rows it renders beside.
func TestBuildRowsChoresHeadTheBacklog(t *testing.T) {
	now := time.Now().UTC()
	rows, err := BuildRows(Inputs{
		Now:   now,
		Index: &run.JournalIndex{LastActivity: map[string]time.Time{}},
		Runs: []*run.Metadata{
			{Project: "moe", ID: "an-idea", Workflow: IdeaWorkflow, Status: run.StatusInProgress},
		},
		Intents: []IntentInput{{Project: "moe", Slug: "north-star", Title: "Be fastest"}},
		Chores: []ChoreInput{
			{Project: "moe", Name: "older", Reason: "cadence", When: now.Add(-48 * time.Hour)},
			{Project: "moe", Name: "newer", Reason: "changed paths", When: now.Add(-time.Hour)},
		},
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}

	var order []string
	for _, r := range rows {
		order = append(order, r.Run)
	}
	// Recency-desc among the chores themselves, and both ahead of the idea.
	want := "north-star,newer,older,an-idea"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("row order = %q, want %q", got, want)
	}
	for _, r := range rows[1:3] {
		if r.Bucket != BucketChores {
			t.Errorf("row %s bucket = %v, want BucketChores — the bucket stays the row's identity", r.Run, r.Bucket)
		}
	}
}

// TestRenderChoresFoldIntoBacklog: there is no CHORES section. A due
// chore renders at the head of BACKLOG and counts in its total —
// otherwise a bureaucracy with only judged chores carries a standing
// `CHORES (0)` heading for a family that can never appear under it.
func TestRenderChoresFoldIntoBacklog(t *testing.T) {
	now := time.Now().UTC()
	rows, err := BuildRows(Inputs{
		Now:   now,
		Index: &run.JournalIndex{LastActivity: map[string]time.Time{}},
		Runs: []*run.Metadata{
			{Project: "moe", ID: "an-idea", Workflow: IdeaWorkflow, Status: run.StatusInProgress},
		},
		Chores: []ChoreInput{{Project: "moe", Name: "bump-deps", Reason: "cadence", When: now.Add(-time.Hour)}},
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}

	var buf strings.Builder
	Render(&buf, now, nil, rows, 1, 0, false, FactoryStateFromRows(rows), rand.New(rand.NewSource(1)))
	out := buf.String()

	if strings.Contains(out, "CHORES") {
		t.Errorf("CHORES section still rendered:\n%s", out)
	}
	if !strings.Contains(out, "BACKLOG (2)") {
		t.Errorf("BACKLOG must count the chore row:\n%s", out)
	}
	backlogIdx := strings.Index(out, "BACKLOG (2)")
	choreIdx := strings.Index(out, "moe/bump-deps")
	ideaIdx := strings.Index(out, "moe/an-idea")
	if choreIdx < backlogIdx || ideaIdx < choreIdx {
		t.Errorf("chore row must render under BACKLOG, ahead of the ideas:\n%s", out)
	}
	// The chore's reason column is what tells it apart from an idea title.
	if !strings.Contains(out, "cadence") {
		t.Errorf("chore reason missing from the row:\n%s", out)
	}
	// The art's crate count reads the same backlog the heading counts.
	if got := FactoryStateFromRows(rows).BacklogCount; got != 2 {
		t.Errorf("BacklogCount = %d, want 2", got)
	}
}
