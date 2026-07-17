package dash

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// buildSpawn runs BuildRows over the given runs with a SpawnedBy edge
// map and returns the rows keyed by "<project>/<run>" plus the ordered
// COMPLETED slice — the two views the nesting assertions need. when and
// spawnedBy are keyed by qualified "<project>/<slug>".
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

// TestSpawnOneChildRendersArrowNoRow: a parent with a single spawned
// pulse keeps its one completed row and gains a "· spawned →" hint; the
// pulse costs zero rows.
func TestSpawnOneChildRendersArrowNoRow(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it": base.Add(-2 * time.Hour),
		"p/pulse-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/pulse-1": "ship-it"})

	if len(completed) != 1 {
		t.Fatalf("completed rows = %d, want 1 (child folded into an arrow): %+v", len(completed), completed)
	}
	if _, ok := byKey["p/pulse-1"]; ok {
		t.Fatalf("pulse-1 should not render its own row when it is an only child")
	}
	if got := byKey["p/ship-it"].Note; !strings.Contains(got, "· spawned → pulse-1") {
		t.Fatalf("parent note = %q, want the spawned arrow to pulse-1", got)
	}
}

// TestSpawnMultiChildRendersMemberRows: a parent that spawned two
// pulses (an sdlc run pulses at push and at close) renders like a chain
// — the parent followed by both children as Member rows, newest first,
// and no arrow hint on the parent.
func TestSpawnMultiChildRendersMemberRows(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-old", "pulse"),
		closedRun("p", "pulse-new", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it":   base.Add(-3 * time.Hour),
		"p/pulse-old": base.Add(-2 * time.Hour),
		"p/pulse-new": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{
		"p/pulse-old": "ship-it",
		"p/pulse-new": "ship-it",
	})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"p/ship-it", "p/pulse-new", "p/pulse-old"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want parent then children newest-first %v", order, want)
	}
	if byKey["p/ship-it"].Member {
		t.Fatal("parent row must not be a Member")
	}
	if !byKey["p/pulse-new"].Member || !byKey["p/pulse-old"].Member {
		t.Fatal("both spawned children must be Member rows")
	}
	if strings.Contains(byKey["p/ship-it"].Note, "spawned →") {
		t.Fatalf("multi-child parent must not carry the single-child arrow: %q", byKey["p/ship-it"].Note)
	}
}

// TestSpawnUnresolvedParentRendersTopLevel: a pulse whose spawner is not
// on the board (pruned, or a standalone `moe pulse new`) renders as a
// normal top-level completed row.
func TestSpawnUnresolvedParentRendersTopLevel(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{closedRun("p", "pulse-x", "pulse")}
	when := map[string]time.Time{"p/pulse-x": base}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/pulse-x": "ghost"})

	if len(completed) != 1 {
		t.Fatalf("completed rows = %d, want 1", len(completed))
	}
	if byKey["p/pulse-x"].Member {
		t.Fatal("an orphaned spawned run must render top-level, not as a Member")
	}
}

// TestSpawnCrossBucketArrowOnActiveParent: while the parent is pushed
// (ACTIVE, awaiting merge) it has exactly one pulse child (fired at
// push), which renders as an arrow on the active row — no completed
// child row, so the cross-bucket nesting never materialises a row.
func TestSpawnCrossBucketArrowOnActiveParent(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		{ID: "ship-it", Project: "p", Workflow: "sdlc", Status: run.StatusPushed},
		closedRun("p", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it": base.Add(-1 * time.Hour),
		"p/pulse-1": base,
	}
	idx := &run.JournalIndex{LastActivity: when, SpawnedBy: map[string]string{"p/pulse-1": "ship-it"}}
	rows, err := BuildRows(Inputs{Now: base, Runs: runs, Index: idx, NextByRun: map[string]NextDecision{}})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var active, completed []Row
	for _, r := range rows {
		switch r.Bucket {
		case BucketActiveRuns:
			active = append(active, r)
		case BucketCompletedRuns:
			completed = append(completed, r)
		}
	}
	if len(completed) != 0 {
		t.Fatalf("completed rows = %d, want 0 (only child folds into the active parent's arrow)", len(completed))
	}
	if len(active) != 1 || !strings.Contains(active[0].Note, "· spawned → pulse-1") {
		t.Fatalf("active parent = %+v, want a single row carrying the spawned arrow", active)
	}
}

// TestCompletedCutoffCountsTopLevelOnly pins the cap semantics directly:
// member children never count against CompletedCap, so a shown parent
// drags all its children in and the cutoff falls on the (cap+1)th
// top-level row.
func TestCompletedCutoffCountsTopLevelOnly(t *testing.T) {
	// Layout: CompletedCap top-level rows, each followed by one member,
	// then one more top-level row (the eviction boundary) with a member.
	var member []bool
	for range CompletedCap + 1 {
		member = append(member, false, true) // parent, child
	}
	isMember := func(i int) bool { return member[i] }

	if got := CompletedCutoff(len(member), true /*showAll*/, isMember); got != len(member) {
		t.Fatalf("showAll cutoff = %d, want %d (no cap)", got, len(member))
	}
	// The (cap+1)th parent sits at index 2*CompletedCap; the cut drops it
	// and its child, keeping the first CompletedCap parents + their kids.
	if got := CompletedCutoff(len(member), false, isMember); got != 2*CompletedCap {
		t.Fatalf("cutoff = %d, want %d (cap parents + their members, evicting the extra parent)", got, 2*CompletedCap)
	}
}

// TestRenderNestedCompletedDrawsConnector: the CLI completed section
// draws the "↳" connector for a spawned member child, matching the
// active-chain renderer.
func TestRenderNestedCompletedDrawsConnector(t *testing.T) {
	rows := []Row{
		{Project: "p", Run: "ship-it", Note: "sdlc: closed", Bucket: BucketCompletedRuns},
		{Project: "p", Run: "pulse-a", Note: "pulse: closed", Bucket: BucketCompletedRuns, Member: true},
		{Project: "p", Run: "pulse-b", Note: "pulse: closed", Bucket: BucketCompletedRuns, Member: true},
	}
	var buf bytes.Buffer
	Render(&buf, time.Now().UTC(), nil, rows, 1, 0, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := buf.String()
	if !strings.Contains(out, "↳ p/pulse-a") || !strings.Contains(out, "↳ p/pulse-b") {
		t.Fatalf("completed member rows should render with the ↳ connector:\n%s", out)
	}
	if strings.Contains(out, "↳ p/ship-it") {
		t.Fatalf("the parent row must not carry a connector:\n%s", out)
	}
}
