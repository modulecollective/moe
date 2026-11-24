package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildPromote runs BuildRows over the given runs with both lineage
// edge maps and returns the rows keyed by "<project>/<run>" plus the
// ordered row slice. Includes the PromotedTo index. All three maps are keyed (and
// PromotedTo/SpawnedBy valued) by qualified "<project>/<slug>".
func buildPromote(t *testing.T, runs []*run.Metadata, when map[string]time.Time, promotedTo, spawnedBy map[string]string) (map[string]Row, []Row) {
	t.Helper()
	next := make(map[string]NextDecision)
	for _, md := range runs {
		if md.Status == run.StatusInProgress && md.Workflow != IdeaWorkflow {
			next[md.Project+"/"+md.ID] = NextDecision{Stage: "code"}
		}
	}
	idx := &run.JournalIndex{LastActivity: when, PromotedTo: promotedTo, SpawnedBy: spawnedBy}
	rows, err := BuildRows(Inputs{Now: time.Now().UTC(), Runs: runs, Index: idx, NextByRun: next})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	byKey := make(map[string]Row, len(rows))
	for _, r := range rows {
		byKey[r.Project+"/"+r.Run] = r
	}
	return byKey, rows
}

func promotedIdea(project, id string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: IdeaWorkflow, Status: run.StatusPromoted}
}

func mergedRun(project, id string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: "sdlc", Status: run.StatusMerged}
}

func keysOf(rows []Row, bucket Bucket) []string {
	var out []string
	for _, r := range rows {
		if r.Bucket == bucket {
			out = append(out, r.Project+"/"+r.Run)
		}
	}
	return out
}

func TestPromoteAndSpawnKeepOwnRecency(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{promotedIdea("p", "idea"), mergedRun("p", "successor"), closedRun("p", "pulse", "pulse"), mergedRun("p", "unrelated")}
	when := map[string]time.Time{"p/idea": base.Add(-21 * 24 * time.Hour), "p/successor": base.Add(-2 * time.Hour), "p/pulse": base.Add(-time.Hour), "p/unrelated": base.Add(-24 * time.Hour)}
	byKey, rows := buildPromote(t, runs, when, map[string]string{"p/idea": "p/successor"}, map[string]string{"p/pulse": "p/successor"})
	if got, want := strings.Join(keysOf(rows, BucketCompletedRuns), ","), "p/pulse,p/successor,p/unrelated,p/idea"; got != want {
		t.Fatalf("completed = %s, want %s", got, want)
	}
	if byKey["p/idea"].Note != "idea:promoted → p/successor" || !byKey["p/idea"].When.Equal(when["p/idea"]) {
		t.Fatalf("idea = %+v", byKey["p/idea"])
	}
}

// A reopened idea belongs in BACKLOG; its closed successor stays completed.
func TestPromoteReopenedIdeaDoesNotFold(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		{ID: "some-idea", Project: "p", Workflow: IdeaWorkflow, Status: run.StatusInProgress},
		closedRun("p", "the-run", "sdlc"),
	}
	when := map[string]time.Time{
		"p/some-idea": base.Add(-1 * time.Hour),
		"p/the-run":   base.Add(-3 * time.Hour),
	}
	byKey, rows := buildPromote(t, runs, when, map[string]string{"p/some-idea": "p/the-run"}, nil)

	if got := strings.Join(keysOf(rows, BucketBacklog), ","); got != "p/some-idea" {
		t.Fatalf("backlog rows = %s, want the reopened idea alone at top level", got)
	}
	if got := strings.Join(keysOf(rows, BucketCompletedRuns), ","); got != "p/the-run" {
		t.Fatalf("completed rows = %s, want the abandoned run left flat in COMPLETED", got)
	}
	if got := byKey["p/some-idea"].Note; got != "idea:capture" {
		t.Fatalf("reopened idea note = %q, want %q", got, "idea:capture")
	}
}

// An open successor stays ACTIVE while its promoted idea keeps the target hint.
func TestPromoteOpenSuccessorStaysFlat(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		promotedIdea("p", "some-idea"),
		{ID: "the-run", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	when := map[string]time.Time{
		"p/some-idea": base.Add(-21 * 24 * time.Hour),
		"p/the-run":   base.Add(-2 * time.Hour),
	}
	byKey, rows := buildPromote(t, runs, when, map[string]string{"p/some-idea": "p/the-run"}, nil)

	if got := strings.Join(keysOf(rows, BucketActiveRuns), ","); got != "p/the-run" {
		t.Fatalf("active rows = %s, want the open successor top-level", got)
	}
	if got := strings.Join(keysOf(rows, BucketCompletedRuns), ","); got != "p/some-idea" {
		t.Fatalf("completed rows = %s, want the idea flat", got)
	}
	if got := byKey["p/some-idea"].Note; got != "idea:promoted → p/the-run" {
		t.Fatalf("idea note = %q, want the inline hint while the successor is open", got)
	}
}

// TestTaggedIdeaNoteNamesItsWorkflow: a tag is the machine's license to
// start an idea, and the backlog note is where the operator's eye
// already lands — so the tag rides there rather than hiding in
// run.json. Untagged ideas read exactly as before, which is the fence
// staying visible from across the room.
func TestTaggedIdeaNoteNamesItsWorkflow(t *testing.T) {
	runs := []*run.Metadata{
		{ID: "licensed", Project: "p", Workflow: IdeaWorkflow, Status: run.StatusInProgress, PromoteTo: "sdlc"},
		{ID: "fenced", Project: "p", Workflow: IdeaWorkflow, Status: run.StatusInProgress},
	}
	when := map[string]time.Time{
		"p/licensed": time.Now().UTC().Add(-2 * time.Hour),
		"p/fenced":   time.Now().UTC().Add(-1 * time.Hour),
	}
	byKey, _ := buildPromote(t, runs, when, nil, nil)

	if got := byKey["p/licensed"].Note; got != "idea:capture → sdlc" {
		t.Fatalf("tagged idea note = %q, want %q", got, "idea:capture → sdlc")
	}
	if got := byKey["p/fenced"].Note; got != "idea:capture" {
		t.Fatalf("untagged idea note = %q, want %q", got, "idea:capture")
	}
}

// TestDesignOnlyTaggedIdeaNoteSaysSo: a design-only tag is a narrower
// licence — one design turn, then a hold — and the board is where a
// survey reads which bar to apply to a slug. Without it the note can't
// tell "ship this" from "think about this", and neither can the eye.
func TestDesignOnlyTaggedIdeaNoteSaysSo(t *testing.T) {
	runs := []*run.Metadata{
		{ID: "briefed", Project: "p", Workflow: IdeaWorkflow, Status: run.StatusInProgress, PromoteTo: "sdlc", DesignOnly: true},
	}
	when := map[string]time.Time{"p/briefed": time.Now().UTC().Add(-2 * time.Hour)}
	byKey, _ := buildPromote(t, runs, when, nil, nil)

	if got, want := byKey["p/briefed"].Note, "idea:capture → sdlc, design only"; got != want {
		t.Fatalf("design-only idea note = %q, want %q", got, want)
	}
}
