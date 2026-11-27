package dash

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildSpawn runs BuildRows over the given runs with a SpawnedBy edge
// map and returns the rows keyed by "<project>/<run>" plus the ordered
// COMPLETED slice. when is keyed by qualified "<project>/<slug>"; spawnedBy is keyed and valued
// by qualified "<project>/<slug>" (the always-qualified index contract).
func buildSpawn(t *testing.T, runs []*run.Metadata, when map[string]time.Time, spawnedBy map[string]string) (map[string]Row, []Row) {
	t.Helper()
	next := make(map[string]NextDecision)
	for _, md := range runs {
		if md.Status == run.StatusInProgress && md.Workflow != IdeaWorkflow {
			next[md.Project+"/"+md.ID] = NextDecision{Stage: "code"}
		}
	}
	idx := &run.JournalIndex{LastActivity: when, SpawnedBy: spawnedBy}
	rows, err := BuildRows(Inputs{Now: time.Now().UTC(), Runs: runs, Index: idx, NextByRun: next})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	byKey := make(map[string]Row, len(rows))
	var completed []Row
	for _, r := range rows {
		byKey[r.Project+"/"+r.Run] = r
		if r.Bucket == BucketCompletedRuns {
			completed = append(completed, r)
		}
	}
	return byKey, completed
}

func closedRun(project, id, workflow string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: workflow, Status: run.StatusClosed}
}

// The incident: an old pulse must not bury a child that finishes today.
func TestSpawnCompletionKeepsOwnRecency(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{closedRun("p", "pulse", "pulse"), closedRun("p", "child", "sdlc")}
	when := map[string]time.Time{"p/pulse": base.Add(-72 * time.Hour), "p/child": base}
	for i := range 6 {
		id := fmt.Sprintf("other-%d", i)
		runs = append(runs, closedRun("p", id, "sdlc"))
		when["p/"+id] = base.Add(-time.Duration(i+1) * time.Hour)
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/child": "p/pulse"})
	if completed[0].Run != "child" || completed[len(completed)-1].Run != "pulse" {
		t.Fatalf("completed order = %+v", completed)
	}
	if got := byKey["p/child"]; got.Note != "sdlc:closed · reopen? · from p/pulse" || !got.Agent || !got.When.Equal(base) {
		t.Fatalf("child = %+v", got)
	}
	var buf bytes.Buffer
	Render(&buf, base, nil, completed, 1, 0, false)
	out := buf.String()
	if !strings.Contains(out, "COMPLETED (5 of 8)") || !strings.Contains(out, "\n  p/child ") || strings.Contains(out, "\n  p/pulse ") || strings.Contains(out, "↳") {
		t.Fatalf("default dash must show the child flat, with the old pulse capped out:\n%s", out)
	}
}

func TestSpawnCompletedChildKeepsBucket(t *testing.T) {
	for _, parent := range []*run.Metadata{
		{ID: "parent", Project: "p", Workflow: "pulse", Status: run.StatusInProgress},
		{ID: "parent", Project: "p", Workflow: "sdlc", Status: run.StatusPushed},
		{ID: "parent", Project: "p", Workflow: ChainWorkflow, Status: run.StatusInProgress},
	} {
		t.Run(parent.Workflow+"/"+string(parent.Status), func(t *testing.T) {
			base := time.Now().UTC()
			byKey, completed := buildSpawn(t, []*run.Metadata{parent, closedRun("p", "child", "sdlc")}, map[string]time.Time{"p/parent": base.Add(-time.Hour), "p/child": base}, map[string]string{"p/child": "p/parent"})
			if len(completed) != 1 || completed[0].Run != "child" || byKey["p/child"].Note != "sdlc:closed · reopen? · from p/parent" {
				t.Fatalf("completed = %+v", completed)
			}
		})
	}
}

func TestSpawnNotesKeepDirectEdges(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("a", "parent", "pulse"),
		closedRun("b", "child", "sdlc"),
		{ID: "open", Project: "b", Workflow: "sdlc", Status: run.StatusInProgress},
		{ID: "older", Project: "b", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	byKey, completed := buildSpawn(t, runs, map[string]time.Time{"a/parent": base.Add(-3 * time.Hour), "b/child": base.Add(-2 * time.Hour), "b/open": base, "b/older": base.Add(-time.Hour)}, map[string]string{"b/child": "a/parent", "b/open": "b/child", "b/older": "b/child"})
	if len(completed) != 2 || completed[0].Run != "child" {
		t.Fatalf("completed = %+v", completed)
	}
	if got, want := byKey["b/child"].Note, "sdlc:closed · reopen? · from a/parent · spawned → b/open · spawned → b/older"; got != want {
		t.Fatalf("child note = %q, want %q", got, want)
	}
	if byKey["a/parent"].Note != "pulse:closed" || byKey["b/open"].Bucket != BucketActiveRuns || byKey["b/older"].Bucket != BucketActiveRuns {
		t.Fatalf("rows = %+v", byKey)
	}
}

func TestSpawnOffBoardOriginIsNamed(t *testing.T) {
	byKey, _ := buildSpawn(t, []*run.Metadata{closedRun("b", "child", "sdlc")}, nil, map[string]string{"b/child": "a/absent"})
	if got := byKey["b/child"]; got.Note != "sdlc:closed · reopen? · from a/absent" || !got.Agent {
		t.Fatalf("child = %+v", got)
	}
}

func TestCompletedCutoffCountsRows(t *testing.T) {
	for _, tc := range []struct{ n, cap, want int }{{12, -1, 12}, {12, 5, 5}, {12, 0, 0}, {12, 100, 12}, {0, 5, 0}} {
		if got := CompletedCutoff(tc.n, tc.cap); got != tc.want {
			t.Errorf("CompletedCutoff(%d,%d) = %d, want %d", tc.n, tc.cap, got, tc.want)
		}
	}
}
