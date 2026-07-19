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
// COMPLETED slice — the two views the nesting assertions need. when is
// keyed by qualified "<project>/<slug>"; spawnedBy is keyed and valued
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

// TestSpawnOneChildNestsAsRow: a parent with a single spawned pulse
// renders it as a nested row, same as any other descendant count — one
// grammar, no inline-arrow special case.
func TestSpawnOneChildNestsAsRow(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it": base.Add(-2 * time.Hour),
		"p/pulse-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/pulse-1": "p/ship-it"})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"p/ship-it", "p/pulse-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want parent then nested child %v", order, want)
	}
	if got := byKey["p/ship-it"].Depth; got != 0 {
		t.Fatalf("parent Depth = %d, want 0", got)
	}
	if got := byKey["p/pulse-1"].Depth; got != 1 {
		t.Fatalf("sole child Depth = %d, want 1", got)
	}
	if got := byKey["p/ship-it"].Note; strings.Contains(got, "spawned →") {
		t.Fatalf("a folded child renders as a row, not an arrow on the parent: %q", got)
	}
}

// TestSpawnMultiChildRendersNestedRows: a parent that spawned two
// pulses (an sdlc run pulses at push and at close) renders like a chain
// — the parent followed by both children as nested rows, newest first,
// and no arrow hint on the parent.
func TestSpawnMultiChildRendersNestedRows(t *testing.T) {
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
		"p/pulse-old": "p/ship-it",
		"p/pulse-new": "p/ship-it",
	})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"p/ship-it", "p/pulse-new", "p/pulse-old"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want parent then children newest-first %v", order, want)
	}
	if got := byKey["p/ship-it"].Depth; got != 0 {
		t.Fatalf("parent Depth = %d, want 0", got)
	}
	if byKey["p/pulse-new"].Depth != 1 || byKey["p/pulse-old"].Depth != 1 {
		t.Fatal("both spawned children must nest one level under the parent")
	}
	if strings.Contains(byKey["p/ship-it"].Note, "spawned →") {
		t.Fatalf("a parent whose children all fold carries no arrow: %q", byKey["p/ship-it"].Note)
	}
}

// TestSpawnDepth2ChainRendersThreeRows: a closed two-hop chain
// (ship-it → pulse → reflect) hoists both descendants under the top-level
// sdlc run — three rows in lineage (DFS) order, no arrow on the root, and
// each generation one Depth deeper than the last. Without the multi-level
// walk the reflect would fold under the (itself folded) pulse and vanish
// entirely.
func TestSpawnDepth2ChainRendersThreeRows(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-1", "pulse"),
		closedRun("p", "reflect-1", "twin"),
	}
	when := map[string]time.Time{
		"p/ship-it":   base.Add(-3 * time.Hour),
		"p/pulse-1":   base.Add(-2 * time.Hour),
		"p/reflect-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{
		"p/pulse-1":   "p/ship-it",
		"p/reflect-1": "p/pulse-1",
	})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"p/ship-it", "p/pulse-1", "p/reflect-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want root then lineage %v", order, want)
	}
	wantDepth := map[string]int{"p/ship-it": 0, "p/pulse-1": 1, "p/reflect-1": 2}
	for k, d := range wantDepth {
		if got := byKey[k].Depth; got != d {
			t.Errorf("%s Depth = %d, want %d (one level per generation)", k, got, d)
		}
	}
	if strings.Contains(byKey["p/ship-it"].Note, "spawned →") {
		t.Fatalf("a root with nested descendants must not carry the arrow: %q", byKey["p/ship-it"].Note)
	}
}

// TestSpawnCrossProjectNestsUnderForeignSpawner: a run in project b
// spawned from a run in project a nests under its foreign spawner. This
// is the capability the qualified edge buys — the child carries its own
// project (b) while its spawner lives in a. The nested row keeps its own
// qualified slug so the cross-project origin is legible.
func TestSpawnCrossProjectNestsUnderForeignSpawner(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("a", "ship-it", "sdlc"),
		closedRun("b", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"a/ship-it": base.Add(-2 * time.Hour),
		"b/pulse-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"b/pulse-1": "a/ship-it"})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"a/ship-it", "b/pulse-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want the foreign child nested under its spawner %v", order, want)
	}
	if byKey["b/pulse-1"].Depth != 1 {
		t.Fatal("cross-project child must nest one level under its foreign spawner")
	}
}

// TestSpawnOpenMidChainNestsUnderActivePulse: ship-it(closed) →
// pulse(open) → reflect(closed). The open pulse doesn't fold (top-level
// ACTIVE); the reflect's walk stops at the pulse and attaches there, so
// it renders as a nested row normalised into the pulse's ACTIVE bucket
// rather than as its own completed row. ship-it keeps only the hint for
// its still-open child.
func TestSpawnOpenMidChainNestsUnderActivePulse(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		{ID: "pulse-1", Project: "p", Workflow: "pulse", Status: run.StatusInProgress},
		closedRun("p", "reflect-1", "twin"),
	}
	when := map[string]time.Time{
		"p/ship-it":   base.Add(-3 * time.Hour),
		"p/pulse-1":   base.Add(-2 * time.Hour),
		"p/reflect-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{
		"p/pulse-1":   "p/ship-it",
		"p/reflect-1": "p/pulse-1",
	})

	// Only ship-it remains a top-level completed row — reflect-1 moved
	// into ACTIVE alongside the pulse it hangs off.
	if len(completed) != 1 || completed[0].Project+"/"+completed[0].Run != "p/ship-it" {
		t.Fatalf("completed rows = %+v, want only p/ship-it", completed)
	}
	pulse := byKey["p/pulse-1"]
	if pulse.Bucket != BucketActiveRuns || pulse.Depth != 0 {
		t.Fatalf("open mid-chain pulse must stay a top-level ACTIVE row: %+v", pulse)
	}
	reflect := byKey["p/reflect-1"]
	if reflect.Bucket != BucketActiveRuns || reflect.Depth != 1 {
		t.Fatalf("closed reflect must nest under the active pulse: %+v", reflect)
	}
	if strings.Contains(pulse.Note, "spawned →") {
		t.Fatalf("pulse note = %q, want no arrow — reflect-1 renders as a nested row", pulse.Note)
	}
	if got := byKey["p/ship-it"].Note; !strings.Contains(got, "· spawned → p/pulse-1") {
		t.Fatalf("ship-it note = %q, want a hint to its open child pulse-1", got)
	}
}

// TestSpawnChainWithSiblingKeepsLineageOrder: a root with both a chain
// (pulse-old → reflect) and a bare sibling pulse (pulse-new) emits in
// lineage order — each direct child immediately followed by its own
// subtree — so the newer sibling never interleaves into the older
// child's lineage.
func TestSpawnChainWithSiblingKeepsLineageOrder(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-old", "pulse"),
		closedRun("p", "reflect-1", "twin"),
		closedRun("p", "pulse-new", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it":   base.Add(-4 * time.Hour),
		"p/pulse-old": base.Add(-3 * time.Hour),
		"p/reflect-1": base.Add(-2 * time.Hour),
		"p/pulse-new": base.Add(-1 * time.Hour),
	}
	_, completed := buildSpawn(t, runs, when, map[string]string{
		"p/pulse-old": "p/ship-it",
		"p/reflect-1": "p/pulse-old",
		"p/pulse-new": "p/ship-it",
	})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	// Direct children newest-first (pulse-new, pulse-old); pulse-old's
	// reflect follows pulse-old, not the newer sibling.
	want := []string{"p/ship-it", "p/pulse-new", "p/pulse-old", "p/reflect-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want lineage DFS %v", order, want)
	}
}

// TestSpawnCycleRendersTopLevel: a two-row SpawnedBy cycle (a → b → a) is
// bad data; both rows degrade to top-level completed rows with no arrow,
// and the walk must not hang.
func TestSpawnCycleRendersTopLevel(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "a", "sdlc"),
		closedRun("p", "b", "pulse"),
	}
	when := map[string]time.Time{
		"p/a": base.Add(-2 * time.Hour),
		"p/b": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{
		"p/a": "p/b",
		"p/b": "p/a",
	})

	if len(completed) != 2 {
		t.Fatalf("completed rows = %d, want 2 (cycle degrades to top-level): %+v", len(completed), completed)
	}
	for _, k := range []string{"p/a", "p/b"} {
		if byKey[k].Depth != 0 {
			t.Fatalf("%s must render top-level under a cycle, not nested", k)
		}
		if strings.Contains(byKey[k].Note, "spawned →") {
			t.Fatalf("%s must carry no spawned arrow under a cycle: %q", k, byKey[k].Note)
		}
	}
}

// TestSpawnUnresolvedParentRendersTopLevel: a pulse whose spawner is not
// on the board (pruned, or a standalone `moe pulse new`) renders as a
// normal top-level completed row.
func TestSpawnUnresolvedParentRendersTopLevel(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{closedRun("p", "pulse-x", "pulse")}
	when := map[string]time.Time{"p/pulse-x": base}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/pulse-x": "p/ghost"})

	if len(completed) != 1 {
		t.Fatalf("completed rows = %d, want 1", len(completed))
	}
	if byKey["p/pulse-x"].Depth != 0 {
		t.Fatal("an orphaned spawned run must render top-level, not nested")
	}
}

// TestSpawnCrossBucketNestsUnderActiveParent: while the parent is pushed
// (ACTIVE, awaiting merge) its closed pulse child nests under it in
// ACTIVE — the child is normalised into the parent's bucket, so it never
// leaves a stray row in COMPLETED.
func TestSpawnCrossBucketNestsUnderActiveParent(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		{ID: "ship-it", Project: "p", Workflow: "sdlc", Status: run.StatusPushed},
		closedRun("p", "pulse-1", "pulse"),
	}
	when := map[string]time.Time{
		"p/ship-it": base.Add(-1 * time.Hour),
		"p/pulse-1": base,
	}
	idx := &run.JournalIndex{LastActivity: when, SpawnedBy: map[string]string{"p/pulse-1": "p/ship-it"}}
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
		t.Fatalf("completed rows = %d, want 0 (the child nests into the active parent's bucket)", len(completed))
	}
	if len(active) != 2 {
		t.Fatalf("active rows = %+v, want the pushed parent plus its nested pulse", active)
	}
	if active[0].Run != "ship-it" || active[0].Depth != 0 {
		t.Fatalf("active[0] = %+v, want the top-level pushed parent", active[0])
	}
	if active[1].Run != "pulse-1" || active[1].Depth != 1 {
		t.Fatalf("active[1] = %+v, want the pulse nested one level", active[1])
	}
}

// TestSpawnOpenChildStaysTopLevelActive: a spawned pulse that is still
// open — the broken-sweep case, where a failed survey leaves the run
// open by design so a human escalates to it — must not fold under its
// (completed) parent. It stays a top-level ACTIVE row, so the broken
// sweep stays visible on the dash — and the parent still renders the
// edge as a hint, so the operator can click through to the live child.
func TestSpawnOpenChildStaysTopLevelActive(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		{ID: "pulse-1", Project: "p", Workflow: "pulse", Status: run.StatusInProgress},
	}
	when := map[string]time.Time{
		"p/ship-it": base.Add(-2 * time.Hour),
		"p/pulse-1": base.Add(-1 * time.Hour),
	}
	byKey, _ := buildSpawn(t, runs, when, map[string]string{"p/pulse-1": "p/ship-it"})

	child, ok := byKey["p/pulse-1"]
	if !ok {
		t.Fatal("open pulse child must render its own row, not fold under the parent")
	}
	if child.Bucket != BucketActiveRuns {
		t.Fatalf("open pulse child bucket = %v, want BucketActiveRuns", child.Bucket)
	}
	if child.Depth != 0 {
		t.Fatal("open pulse child must be a top-level row, not nested")
	}
	if got := byKey["p/ship-it"].Note; !strings.Contains(got, "· spawned → p/pulse-1") {
		t.Fatalf("parent note = %q, want a hint to its still-open child", got)
	}
}

// TestSpawnOpenGrandchildHintsOnNestedRow: the open-child hint attaches
// to whichever row carries the edge, nested or not. sdlc ← pulse (both
// completed) ← reflect (open): the pulse nests under ship-it and carries
// the reflect hint itself, while the open reflect stays top-level ACTIVE.
func TestSpawnOpenGrandchildHintsOnNestedRow(t *testing.T) {
	base := time.Now().UTC()
	runs := []*run.Metadata{
		closedRun("p", "ship-it", "sdlc"),
		closedRun("p", "pulse-1", "pulse"),
		{ID: "reflect-1", Project: "p", Workflow: "twin", Status: run.StatusInProgress},
	}
	when := map[string]time.Time{
		"p/ship-it":   base.Add(-3 * time.Hour),
		"p/pulse-1":   base.Add(-2 * time.Hour),
		"p/reflect-1": base.Add(-1 * time.Hour),
	}
	byKey, completed := buildSpawn(t, runs, when, map[string]string{
		"p/pulse-1":   "p/ship-it",
		"p/reflect-1": "p/pulse-1",
	})

	var order []string
	for _, r := range completed {
		order = append(order, r.Project+"/"+r.Run)
	}
	want := []string{"p/ship-it", "p/pulse-1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("completed order = %v, want the pulse nested under its parent %v", order, want)
	}
	if byKey["p/pulse-1"].Depth != 1 {
		t.Fatal("pulse-1 must nest one level under ship-it")
	}
	if got := byKey["p/pulse-1"].Note; !strings.Contains(got, "· spawned → p/reflect-1") {
		t.Fatalf("pulse-1 note = %q, want the hint to its open child", got)
	}
	if got := byKey["p/ship-it"].Note; strings.Contains(got, "spawned →") {
		t.Fatalf("ship-it has no open child of its own and nests pulse-1 as a row — no hint: %q", got)
	}
	reflect := byKey["p/reflect-1"]
	if reflect.Bucket != BucketActiveRuns || reflect.Depth != 0 {
		t.Fatalf("open reflect must stay a top-level ACTIVE row: %+v", reflect)
	}
}

// TestCompletedCutoffCountsTopLevelOnly pins the cap semantics directly:
// nested children never count against CompletedCap, so a shown parent
// drags all its children in and the cutoff falls on the (cap+1)th
// top-level row.
func TestCompletedCutoffCountsTopLevelOnly(t *testing.T) {
	// Layout: CompletedCap top-level rows, each followed by one nested
	// child, then one more top-level row (the eviction boundary) with a child.
	var nested []bool
	for range CompletedCap + 1 {
		nested = append(nested, false, true) // parent, child
	}
	isNested := func(i int) bool { return nested[i] }

	if got := CompletedCutoff(len(nested), true /*showAll*/, isNested); got != len(nested) {
		t.Fatalf("showAll cutoff = %d, want %d (no cap)", got, len(nested))
	}
	// The (cap+1)th parent sits at index 2*CompletedCap; the cut drops it
	// and its child, keeping the first CompletedCap parents + their kids.
	if got := CompletedCutoff(len(nested), false, isNested); got != 2*CompletedCap {
		t.Fatalf("cutoff = %d, want %d (cap parents + their children, evicting the extra parent)", got, 2*CompletedCap)
	}
}

// TestRenderNestedCompletedDrawsConnector: the CLI completed section
// draws the "↳" connector for a spawned child — lineage, unlike an
// active chain's flush "→" — and steps each further generation two
// columns right so a
// depth-2 reflect reads as hanging off the pulse, not off the root.
func TestRenderNestedCompletedDrawsConnector(t *testing.T) {
	rows := []Row{
		{Project: "p", Run: "ship-it", Note: "sdlc: closed", Bucket: BucketCompletedRuns},
		{Project: "p", Run: "pulse-a", Note: "pulse: closed", Bucket: BucketCompletedRuns, Depth: 1},
		{Project: "p", Run: "reflect-a", Note: "twin: closed", Bucket: BucketCompletedRuns, Depth: 2},
	}
	var buf bytes.Buffer
	Render(&buf, time.Now().UTC(), nil, rows, 1, 0, false, FactoryState{}, rand.New(rand.NewSource(1)))
	out := buf.String()
	// Each rendered row already carries a two-space section indent, so
	// depth 1 is "  ↳ " and depth 2 is that plus one more level.
	if !strings.Contains(out, "\n  ↳ p/pulse-a") {
		t.Fatalf("a depth-1 row should render with the ↳ connector:\n%s", out)
	}
	if !strings.Contains(out, "\n    ↳ p/reflect-a") {
		t.Fatalf("a depth-2 row should indent past its depth-1 parent:\n%s", out)
	}
	if strings.Contains(out, "↳ p/ship-it") {
		t.Fatalf("the parent row must not carry a connector:\n%s", out)
	}
}
