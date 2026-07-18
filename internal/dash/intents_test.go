package dash

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildIntentRows runs BuildRows over an Intents-only Inputs and returns
// the INTENTS bucket rows in their final order.
func buildIntentRows(t *testing.T, intents []IntentInput, projectFilter string) []Row {
	t.Helper()
	idx := &run.JournalIndex{LastActivity: map[string]time.Time{}}
	rows, err := BuildRows(Inputs{
		Now:           time.Now().UTC(),
		ProjectFilter: projectFilter,
		Index:         idx,
		Intents:       intents,
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var out []Row
	for _, r := range rows {
		if r.Bucket == BucketIntents {
			out = append(out, r)
		}
	}
	return out
}

// TestBuildRowsIntentsSortedByProjectSlug: intent inputs become
// BucketIntents rows, title carried as the note, ordered by project then
// slug regardless of input order.
func TestBuildRowsIntentsSortedByProjectSlug(t *testing.T) {
	rows := buildIntentRows(t, []IntentInput{
		{Project: "tele", Slug: "zephyr", Title: "Z theme"},
		{Project: "moe", Slug: "beacon", Title: "B theme"},
		{Project: "tele", Slug: "aurora", Title: "A theme"},
	}, "")

	var got []string
	for _, r := range rows {
		got = append(got, r.Project+"/"+r.Run)
		if r.Note == "" {
			t.Errorf("intent row %s/%s missing title note", r.Project, r.Run)
		}
	}
	want := "moe/beacon,tele/aurora,tele/zephyr"
	if strings.Join(got, ",") != want {
		t.Fatalf("intent order = %v, want %v", got, want)
	}
	if rows[0].Note != "B theme" {
		t.Errorf("expected title as note, got %q", rows[0].Note)
	}
}

// TestBuildRowsIntentsRespectProjectFilter: a project-scoped dash only
// carries that project's intents.
func TestBuildRowsIntentsRespectProjectFilter(t *testing.T) {
	rows := buildIntentRows(t, []IntentInput{
		{Project: "tele", Slug: "keep", Title: "kept"},
		{Project: "moe", Slug: "drop", Title: "dropped"},
	}, "tele")
	if len(rows) != 1 || rows[0].Project != "tele" || rows[0].Run != "keep" {
		t.Fatalf("expected only tele/keep, got %v", rows)
	}
}

// TestRenderIntentsSectionAlwaysPresent: the INTENTS heading renders even
// with zero intents (an empty list is itself a signal), and above
// BACKLOG. With intents, each renders as project/slug + title.
func TestRenderIntentsSectionAlwaysPresent(t *testing.T) {
	// Zero intents: heading present, "(none)", above BACKLOG.
	var buf strings.Builder
	Render(&buf, time.Now().UTC(), nil, nil, 0, 0, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := buf.String()
	intentsIdx := strings.Index(out, "INTENTS (0)")
	backlogIdx := strings.Index(out, "BACKLOG")
	if intentsIdx < 0 {
		t.Fatalf("INTENTS heading missing at zero:\n%s", out)
	}
	if backlogIdx < 0 || intentsIdx > backlogIdx {
		t.Fatalf("INTENTS must render above BACKLOG:\n%s", out)
	}

	// With an intent row: slug + title both appear under the heading.
	rows := buildIntentRows(t, []IntentInput{{Project: "tele", Slug: "north-star", Title: "Be fastest"}}, "")
	var buf2 strings.Builder
	Render(&buf2, time.Now().UTC(), nil, rows, 0, 0, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out2 := buf2.String()
	if !strings.Contains(out2, "INTENTS (1)") {
		t.Fatalf("expected INTENTS (1) header:\n%s", out2)
	}
	if !strings.Contains(out2, "tele/north-star") || !strings.Contains(out2, "Be fastest") {
		t.Fatalf("expected slug + title in INTENTS section:\n%s", out2)
	}
}
