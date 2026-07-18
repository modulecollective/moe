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
	byKey, completed := buildSpawn(t, runs, when, map[string]string{"p/pulse-1": "p/ship-it"})

	if len(completed) != 1 {
		t.Fatalf("completed rows = %d, want 1 (child folded into an arrow): %+v", len(completed), completed)
	}
	if _, ok := byKey["p/pulse-1"]; ok {
		t.Fatalf("pulse-1 should not render its own row when it is an only child")
	}
	if got := byKey["p/ship-it"].Note; !strings.Contains(got, "· spawned → p/pulse-1") {
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

// TestSpawnDepth2ChainRendersThreeRows: a closed two-hop chain
// (ship-it → pulse → reflect) hoists both descendants under the top-level
// sdlc run — three rows in lineage (DFS) order, no arrow on the root, and
// every descendant a Member. Without the multi-level walk the reflect
// would fold under the (itself folded) pulse and vanish entirely.
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
	if byKey["p/ship-it"].Member {
		t.Fatal("the top-level sdlc run must not be a Member")
	}
	if !byKey["p/pulse-1"].Member || !byKey["p/reflect-1"].Member {
		t.Fatal("both hoisted descendants must be Member rows")
	}
	if strings.Contains(byKey["p/ship-it"].Note, "spawned →") {
		t.Fatalf("a root with member descendants must not carry the arrow: %q", byKey["p/ship-it"].Note)
	}
}

// TestSpawnCrossProjectNestsUnderForeignSpawner: a run in project b
// spawned from a run in project a nests under its foreign spawner. This
// is the capability the qualified edge buys — the child carries its own
// project (b) while its spawner lives in a. The arrow names the child's
// qualified target so the cross-project origin is legible.
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

	if len(completed) != 1 {
		t.Fatalf("completed rows = %d, want 1 (foreign child folded into an arrow): %+v", len(completed), completed)
	}
	if _, ok := byKey["b/pulse-1"]; ok {
		t.Fatal("cross-project only child must fold into its spawner's arrow, not render its own row")
	}
	if got := byKey["a/ship-it"].Note; !strings.Contains(got, "· spawned → b/pulse-1") {
		t.Fatalf("spawner note = %q, want the spawned arrow to the foreign child b/pulse-1", got)
	}
}

// TestSpawnOpenMidChainArrowsOnActivePulse: ship-it(closed) →
// pulse(open) → reflect(closed). The open pulse doesn't fold (top-level
// ACTIVE); the reflect's walk stops at the pulse and attaches there, so
// the ACTIVE pulse gains the spawned arrow and reflect costs zero
// completed rows. ship-it gets nothing — its only child didn't fold.
func TestSpawnOpenMidChainArrowsOnActivePulse(t *testing.T) {
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

	if _, ok := byKey["p/reflect-1"]; ok {
		t.Fatal("reflect-1 should fold into the pulse's arrow, not render its own row")
	}
	// Only ship-it remains a top-level completed row.
	if len(completed) != 1 || completed[0].Project+"/"+completed[0].Run != "p/ship-it" {
		t.Fatalf("completed rows = %+v, want only p/ship-it", completed)
	}
	pulse := byKey["p/pulse-1"]
	if pulse.Bucket != BucketActiveRuns || pulse.Member {
		t.Fatalf("open mid-chain pulse must stay a top-level ACTIVE row: %+v", pulse)
	}
	if !strings.Contains(pulse.Note, "· spawned → p/reflect-1") {
		t.Fatalf("active pulse note = %q, want the spawned arrow to reflect-1", pulse.Note)
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
		if byKey[k].Member {
			t.Fatalf("%s must render top-level under a cycle, not as a Member", k)
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
		t.Fatalf("completed rows = %d, want 0 (only child folds into the active parent's arrow)", len(completed))
	}
	if len(active) != 1 || !strings.Contains(active[0].Note, "· spawned → p/pulse-1") {
		t.Fatalf("active parent = %+v, want a single row carrying the spawned arrow", active)
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
	if child.Member {
		t.Fatal("open pulse child must be a top-level row, not a Member")
	}
	if got := byKey["p/ship-it"].Note; !strings.Contains(got, "· spawned → p/pulse-1") {
		t.Fatalf("parent note = %q, want a hint to its still-open child", got)
	}
}

// TestSpawnOpenGrandchildKeepsMemberRow pins the composability rule: the
// sole-descendant shortcut (drop the child row, arrow on the parent) must
// not fire when that child has an open child of its own — dropping the row
// would take its hint down with it. sdlc ← pulse (both completed) ←
// reflect (open): pulse renders as a Member carrying the reflect hint.
func TestSpawnOpenGrandchildKeepsMemberRow(t *testing.T) {
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
		t.Fatalf("completed order = %v, want the pulse kept as a member row %v", order, want)
	}
	if !byKey["p/pulse-1"].Member {
		t.Fatal("pulse-1 must render as a Member under ship-it, not be dropped for an arrow")
	}
	if got := byKey["p/pulse-1"].Note; !strings.Contains(got, "· spawned → p/reflect-1") {
		t.Fatalf("pulse-1 note = %q, want the hint to its open child", got)
	}
	if got := byKey["p/ship-it"].Note; strings.Contains(got, "spawned →") {
		t.Fatalf("ship-it has no open child of its own and folds pulse-1 as a member — no hint: %q", got)
	}
	reflect := byKey["p/reflect-1"]
	if reflect.Bucket != BucketActiveRuns || reflect.Member {
		t.Fatalf("open reflect must stay a top-level ACTIVE row: %+v", reflect)
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
