package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

func TestChoresStandingSection(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Now: now, Index: &run.JournalIndex{LastActivity: map[string]time.Time{}},
		Runs:    []*run.Metadata{{Project: "moe", ID: "an-idea", Workflow: IdeaWorkflow, Status: run.StatusInProgress}},
		Intents: []IntentInput{{Project: "moe", Slug: "north-star", Title: "Direction"}},
		Chores: []ChoreInput{
			{Project: "moe", Name: "a-judged", Status: "judged — pulse decides", Summary: "when docs change"},
			{Project: "moe", Name: "z-due", Status: "due — cadence", Summary: "every 30d", Due: true},
			{Project: "moe", Name: "b-idle", Status: "not due", Summary: "on *.go"},
			{Project: "other", Name: "a-due", Status: "due — cadence", Due: true},
		},
	}
	rows, err := BuildRows(in)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range rows {
		order = append(order, r.Project+"/"+r.Run)
	}
	if got, want := strings.Join(order, ","), "moe/north-star,moe/z-due,other/a-due,moe/a-judged,moe/b-idle,moe/an-idea"; got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
	var out strings.Builder
	positions := RenderIndexedRows(&out, now, rows, 2, 0, true)
	lines := strings.Split(out.String(), "\n")
	for i, pos := range positions {
		if pos < 0 || !strings.Contains(lines[pos], rows[i].Project+"/"+rows[i].Run) {
			t.Fatalf("row %d at %d", i, pos)
		}
	}
	for _, s := range []string{"CHORES (4)", "BACKLOG (1)", "judged — pulse decides · when docs change", "due — cadence · every 30d"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("missing %q:\n%s", s, out.String())
		}
	}
	if !(strings.Index(out.String(), "INTENTS") < strings.Index(out.String(), "CHORES") && strings.Index(out.String(), "CHORES") < strings.Index(out.String(), "BACKLOG")) {
		t.Fatal("section order")
	}
	if state := FactoryStateFromRows(rows); state.BacklogCount != 1 || len(state.ActiveStages) != 0 {
		t.Fatalf("factory counts %+v", state)
	}
	in.ProjectFilter = "moe"
	rows, err = BuildRows(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Project != "moe" {
			t.Fatal("project filter", r)
		}
	}
	in.WorkflowFilter = "sdlc"
	rows, err = BuildRows(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Bucket == BucketChores {
			t.Fatal("workflow filter", r)
		}
	}
	out.Reset()
	RenderIndexedRows(&out, now, nil, 0, 0, false)
	if !strings.Contains(out.String(), "CHORES (0)\n  (none)") {
		t.Fatalf("empty section: %s", out.String())
	}
}
