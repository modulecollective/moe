package dash

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildPromote runs BuildRows over the given runs with both lineage
// edge maps and returns the rows keyed by "<project>/<run>" plus the
// ordered row slice. Same shape as buildSpawn, with the PromotedTo
// index the promotion fold reads. All three maps are keyed (and
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

// keysOf renders the bucket's rows as "<key>@<depth>" strings so an
// assertion pins order and nesting in one comparison.
func keysOf(rows []Row, bucket Bucket) []string {
	var out []string
	for _, r := range rows {
		if r.Bucket != bucket {
			continue
		}
		out = append(out, r.Project+"/"+r.Run+"@"+string(rune('0'+r.Depth)))
	}
	return out
}

// TestPromoteFoldsIntoLadder: once both the idea and the run it was
// promoted to are completed, they render as a ladder — idea on top,
// successor nested — instead of two unrelated flat rows. The pair sits
// at the *successor's* recency slot, not the idea's, so a run that
// merged long after its promotion stays near the top of COMPLETED
// rather than sinking to the idea's age.
func TestPromoteFoldsIntoLadder(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		promotedIdea("p", "some-idea"),
		mergedRun("p", "the-run"),
		mergedRun("p", "unrelated"),
	}
	when := map[string]time.Time{
		"p/some-idea": base.Add(-21 * 24 * time.Hour),
		"p/the-run":   base.Add(-2 * time.Hour),
		"p/unrelated": base.Add(-24 * time.Hour),
	}
	byKey, rows := buildPromote(t, runs, when, map[string]string{"p/some-idea": "p/the-run"}, nil)

	got := strings.Join(keysOf(rows, BucketCompletedRuns), ",")
	want := "p/some-idea@0,p/the-run@1,p/unrelated@0"
	if got != want {
		t.Fatalf("completed rows = %s, want %s (pair at the successor's recency slot, idea on top)", got, want)
	}
	if note := byKey["p/some-idea"].Note; note != "idea:promoted" {
		t.Fatalf("idea note = %q, want bare %q — the nested row already names the destination", note, "idea:promoted")
	}
	// The idea keeps its own age. The pair moves; the ages stay honest.
	if !byKey["p/some-idea"].When.Equal(when["p/some-idea"]) {
		t.Fatalf("idea When = %v, want its own promote-commit age %v", byKey["p/some-idea"].When, when["p/some-idea"])
	}
}

// TestPromoteComposesWithSpawnLineage: an idea → run → tailed-pulse
// lineage renders as one three-rung ladder. The promotion fold runs
// after the spawn fold and moves whole blocks, so the run's already
// attached descendants ride down a level with it.
func TestPromoteComposesWithSpawnLineage(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		promotedIdea("p", "some-idea"),
		mergedRun("p", "the-run"),
		closedRun("p", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"p/some-idea": base.Add(-21 * 24 * time.Hour),
		"p/the-run":   base.Add(-3 * time.Hour),
		"p/pulse-1":   base.Add(-1 * time.Hour),
	}
	_, rows := buildPromote(t, runs, when,
		map[string]string{"p/some-idea": "p/the-run"},
		map[string]string{"p/pulse-1": "p/the-run"})

	got := strings.Join(keysOf(rows, BucketCompletedRuns), ",")
	want := "p/some-idea@0,p/the-run@1,p/pulse-1@2"
	if got != want {
		t.Fatalf("completed rows = %s, want three-rung ladder %s", got, want)
	}
}

// TestPromoteReopenedIdeaDoesNotFold: an idea reopened after its
// destination was abandoned is back in BACKLOG. Folding across the
// bucket boundary would drag the dead run out of COMPLETED and into the
// backlog, so the both-completed gate leaves both rows where they are.
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

	if got := strings.Join(keysOf(rows, BucketBacklog), ","); got != "p/some-idea@0" {
		t.Fatalf("backlog rows = %s, want the reopened idea alone at top level", got)
	}
	if got := strings.Join(keysOf(rows, BucketCompletedRuns), ","); got != "p/the-run@0" {
		t.Fatalf("completed rows = %s, want the abandoned run left flat in COMPLETED", got)
	}
	if got := byKey["p/some-idea"].Note; got != "idea:capture" {
		t.Fatalf("reopened idea note = %q, want %q", got, "idea:capture")
	}
}

// TestPromoteOpenSuccessorStaysFlat: an in-progress successor is a
// top-level ACTIVE row and the idea keeps the inline
// "idea:promoted → x/y" hint — the same open-counterpart convention
// spawn uses. Nothing folds until both ends settle.
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

	if got := strings.Join(keysOf(rows, BucketActiveRuns), ","); got != "p/the-run@0" {
		t.Fatalf("active rows = %s, want the open successor top-level", got)
	}
	if got := strings.Join(keysOf(rows, BucketCompletedRuns), ","); got != "p/some-idea@0" {
		t.Fatalf("completed rows = %s, want the idea flat", got)
	}
	if got := byKey["p/some-idea"].Note; got != "idea:promoted → p/the-run" {
		t.Fatalf("idea note = %q, want the inline hint while the successor is open", got)
	}
}
