package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func TestParseChainEditFileSplitsBlocksOnBlankLines(t *testing.T) {
	// The all-comment header is a dropped empty block; the blank line
	// after run-foo is a chain boundary, so run-foo is its own block
	// and run-bar/run-baz form a second. Comment lines mid-block stay
	// transparent and do not split the block.
	body := `# moe chain edit
#
# instructions
projA/run-foo          # orphan

projA/run-bar  # chained-from nothing
# a stray comment must not break this block
projB/run-baz  # chains-to projC/run-qux
`
	got, err := parseChainEditFile(body)
	if err != nil {
		t.Fatalf("parseChainEditFile: %v", err)
	}
	want := [][]string{
		{"projA/run-foo"},
		{"projA/run-bar", "projB/run-baz"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseChainEditFileRejectsMalformedSlug(t *testing.T) {
	body := "not-qualified\n"
	_, err := parseChainEditFile(body)
	if err == nil {
		t.Fatal("expected error for unqualified slug, got nil")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error should name the line number, got: %v", err)
	}
}

func TestParseChainEditFileRejectsDuplicateAcrossBlocks(t *testing.T) {
	// The duplicate check is global — a run may not appear in two
	// blocks, so the blank-line boundary doesn't launder a repeat.
	body := "p/foo\n\np/foo\n"
	_, err := parseChainEditFile(body)
	if err == nil {
		t.Fatal("expected error for duplicate slug, got nil")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error should mention the duplication, got: %v", err)
	}
}

func TestParseChainEditFileEmpty(t *testing.T) {
	got, err := parseChainEditFile("# only a comment\n\n")
	if err != nil {
		t.Fatalf("parseChainEditFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("comment-only file: want no blocks, got %v", got)
	}
}

func TestDiffChainEditNewChainOnAllOrphans(t *testing.T) {
	// All three runs are unchained today. The save links them
	// head-first into a linear chain.
	blocks := [][]string{{"p/a", "p/b", "p/c"}}
	live := map[string]string{}
	adds, removes := diffChainEdit(blocks, live)
	wantAdds := []string{"p/a p/b", "p/b p/c"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v", adds, wantAdds)
	}
	if len(removes) != 0 {
		t.Errorf("removes should be empty, got %v", removes)
	}
}

func TestDiffChainEditReplaceMidchainEdge(t *testing.T) {
	// Existing chain a→b. Operator reorders to a→c→b. The diff
	// must drop the prior a→b edge and add a→c and c→b. b's
	// outgoing edge (it had none) stays nil — it's now the last
	// line, so desired b → "" matches live b → "" → no-op.
	blocks := [][]string{{"p/a", "p/c", "p/b"}}
	live := map[string]string{"p/a": "p/b"}
	adds, removes := diffChainEdit(blocks, live)
	wantAdds := []string{"p/a p/c", "p/c p/b"}
	wantRemoves := []string{"p/a p/b"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v", adds, wantAdds)
	}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Errorf("removes = %v, want %v", removes, wantRemoves)
	}
}

func TestDiffChainEditUntouchedWhenParentAbsent(t *testing.T) {
	// Decision 4: parents NOT in the file are untouched. p/x has
	// a live edge but isn't in `desired`; the diff must not emit
	// any trailer for it.
	blocks := [][]string{{"p/a", "p/b"}}
	live := map[string]string{
		"p/x": "p/y",
		"p/a": "p/old",
	}
	adds, removes := diffChainEdit(blocks, live)
	// p/a's edge changes from old → b; p/b is the file's last line
	// so its desired-child is "" matching live's absence → no-op.
	wantAdds := []string{"p/a p/b"}
	wantRemoves := []string{"p/a p/old"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v", adds, wantAdds)
	}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Errorf("removes = %v, want %v", removes, wantRemoves)
	}
}

func TestDiffChainEditNoChanges(t *testing.T) {
	// The saved file matches the live chain exactly.
	blocks := [][]string{{"p/a", "p/b"}}
	live := map[string]string{"p/a": "p/b"}
	adds, removes := diffChainEdit(blocks, live)
	if len(adds) != 0 || len(removes) != 0 {
		t.Errorf("unchanged save should produce no trailers: adds=%v removes=%v", adds, removes)
	}
}

func TestDiffChainEditBlocksStaySeparate(t *testing.T) {
	// Two blocks must not chain into one another: the last line of
	// block one (p/b) keeps no successor even though p/c sits on the
	// next line of the file. This is the footgun fix — a blank line is
	// a hard chain boundary, so an unchanged two-chain save is a no-op.
	blocks := [][]string{{"p/a", "p/b"}, {"p/c", "p/d"}}
	live := map[string]string{"p/a": "p/b", "p/c": "p/d"}
	adds, removes := diffChainEdit(blocks, live)
	if len(adds) != 0 || len(removes) != 0 {
		t.Errorf("two separate chains should produce no trailers: adds=%v removes=%v", adds, removes)
	}
	// And with no live edges, each block links only within itself —
	// no p/b p/c edge bridging the boundary.
	adds, removes = diffChainEdit(blocks, map[string]string{})
	wantAdds := []string{"p/a p/b", "p/c p/d"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v (no cross-block edge)", adds, wantAdds)
	}
	if len(removes) != 0 {
		t.Errorf("removes should be empty, got %v", removes)
	}
}

func TestDiffChainEditLastLineClearsItsOldEdge(t *testing.T) {
	// p/a had an edge to p/b but the operator dropped p/b from the
	// file entirely (or moved p/a to be the last line). p/a is
	// still in the file so its edge must be cleared.
	blocks := [][]string{{"p/a"}}
	live := map[string]string{"p/a": "p/old"}
	adds, removes := diffChainEdit(blocks, live)
	if len(adds) != 0 {
		t.Errorf("adds should be empty, got %v", adds)
	}
	wantRemoves := []string{"p/a p/old"}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Errorf("removes = %v, want %v", removes, wantRemoves)
	}
}

func TestChainAnnotationOrphanVsChainedTo(t *testing.T) {
	byKey := map[string]*run.Metadata{
		"p/a": {ID: "a", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
		"p/b": {ID: "b", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	chainedChild := map[string]string{"p/a": "p/b"}
	chainedFrom := invertEffectiveChain(chainedChild, byKey)
	if got := chainAnnotation("p/a", chainedChild, chainedFrom, byKey); got != "chains-to p/b" {
		t.Errorf("p/a annotation = %q, want \"chains-to p/b\"", got)
	}
	if got := chainAnnotation("p/b", chainedChild, chainedFrom, byKey); got != "chained-from p/a" {
		t.Errorf("p/b annotation = %q, want \"chained-from p/a\"", got)
	}
	if got := chainAnnotation("p/c", chainedChild, chainedFrom, byKey); got != "orphan" {
		t.Errorf("p/c annotation = %q, want \"orphan\" (no edges)", got)
	}
}

func TestChainAnnotationSuppressesTerminalEdges(t *testing.T) {
	// Decision 1: terminal children are skipped at read time. p/a's
	// trailer points at p/b but p/b is closed — the annotation
	// must read "orphan" rather than dangle.
	byKey := map[string]*run.Metadata{
		"p/a": {ID: "a", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
		"p/b": {ID: "b", Project: "p", Workflow: "sdlc", Status: run.StatusClosed},
	}
	chainedChild := map[string]string{"p/a": "p/b"}
	chainedFrom := invertEffectiveChain(chainedChild, byKey)
	if got := chainAnnotation("p/a", chainedChild, chainedFrom, byKey); got != "orphan" {
		t.Errorf("p/a annotation = %q, want \"orphan\" (child terminal)", got)
	}
	// The inverted map must also drop the edge so p/b doesn't
	// claim a chained-from parent the read side would hide.
	if _, ok := chainedFrom["p/b"]; ok {
		t.Errorf("invertEffectiveChain should drop edges into terminal children: got %v", chainedFrom)
	}
}

// TestCascadeFromGateRidesIntoLiveChainChild pins the end-of-cascade
// chain ride: when a parent ships at push and a live trailer points
// at an unresolved child, the cascade transparently opens that child
// at its first pending stage in the same mode. The recursion is by
// construction — the child's own push then re-fires the hook on its
// outgoing edge, so `!!!` rides the whole chain in one shot.
func TestCascadeFromGateRidesIntoLiveChainChild(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	parentMD, err := run.New(root, "tele", run.Options{ID: "parent-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("run.New parent: %v", err)
	}
	if _, err := run.New(root, "tele", run.Options{ID: "child-run", Workflow: "sdlc"}); err != nil {
		t.Fatalf("run.New child: %v", err)
	}
	// Stamp the chain edge as a `chain edit` would: an empty commit
	// carrying just the MoE-Chained-To trailer (no MoE-Run scope —
	// one edit can touch several parents, no single canonical run).
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: tele/parent-run tele/child-run\n")

	t.Chdir(root)
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must ship: %+v", res)
	}
	if got := len(*pushCaptured); got != 2 {
		t.Fatalf("pushFromCascade dispatched %d times, want 2 (parent + child via recursive ride)", got)
	}

	// Child's first pending stage is `design` (nothing committed
	// against the child's docs yet). The chain ride opens the child
	// there, then the child's own cascadeFromGate walks design → code
	// → test via openSdlcStage and ships at push. That gives 4
	// dispatches: parent (code, test), child (design, code, test).
	// push happens via pushFromCascade for both.
	wantStages := []string{"code", "test", "design", "code", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v\nstdout=%q", gotStages, wantStages, stdout.String())
	}
	// The child's three dispatches must be against the child run.
	childInvs := (*openCaptured)[2:]
	for _, inv := range childInvs {
		if inv.projectID != "tele" || inv.runID != "child-run" {
			t.Errorf("child dispatch routed to wrong run: %+v", inv)
		}
		if inv.headless != true {
			t.Errorf("child dispatch should be headless (parent yolo'd): %+v", inv)
		}
	}
	// The cascade printed the chain-ride preamble so the operator
	// sees the hop in the log.
	if !strings.Contains(stdout.String(), "chain: riding into tele/child-run at design (headless)") {
		t.Errorf("expected chain-ride preamble in stdout, got:\n%s", stdout.String())
	}
}

// TestCascadeFromGateSkipsRideWhenChildTerminal pins Decision 1:
// the chain ride filters terminal children at read time, so a
// trailer that points at a closed/merged/promoted run does not
// open it. The parent ships normally; the cascade ends without
// further dispatches.
func TestCascadeFromGateSkipsRideWhenChildTerminal(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	parentMD, err := run.New(root, "tele", run.Options{ID: "parent-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("run.New parent: %v", err)
	}
	childMD, err := run.New(root, "tele", run.Options{ID: "child-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("run.New child: %v", err)
	}
	childMD.Status = run.StatusClosed
	if err := run.Save(root, childMD); err != nil {
		t.Fatalf("save closed child: %v", err)
	}
	gittest.Run(t, root, "add", "--", "projects/tele/runs/child-run/run.json")
	gittest.Run(t, root, "commit", "-m",
		"Close run tele/child-run\n\nMoE-Run: child-run\nMoE-Project: tele\n")
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: tele/parent-run tele/child-run\n")

	t.Chdir(root)
	openCaptured := stubOpenSdlcStage(t, nil)
	stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must ship: %+v", res)
	}
	// Only parent's code+test dispatched. No child stages.
	wantStages := []string{"code", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v (terminal child must not be ridden)", gotStages, wantStages)
	}
	if strings.Contains(stdout.String(), "chain: riding") {
		t.Errorf("chain ride should be silent when child is terminal, stdout:\n%s", stdout.String())
	}
}

// TestCascadeFromGateSkipsRideWhenChainCleared pins the clear-pin
// semantics: a MoE-Chained-To-Removed commit blocks an older
// MoE-Chained-To from re-asserting the edge, so the ride must
// honor the clear even though history still carries the add.
func TestCascadeFromGateSkipsRideWhenChainCleared(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	parentMD, err := run.New(root, "tele", run.Options{ID: "parent-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatalf("run.New parent: %v", err)
	}
	if _, err := run.New(root, "tele", run.Options{ID: "child-run", Workflow: "sdlc"}); err != nil {
		t.Fatalf("run.New child: %v", err)
	}
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chain: edit\n\nMoE-Chained-To: tele/parent-run tele/child-run\n")
	gittest.Run(t, root, "commit", "--allow-empty", "-m",
		"chain: clear\n\nMoE-Chained-To-Removed: tele/parent-run tele/child-run\n")

	t.Chdir(root)
	openCaptured := stubOpenSdlcStage(t, nil)
	stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, false, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must ship: %+v", res)
	}
	wantStages := []string{"code", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v (cleared edge must not ride)", gotStages, wantStages)
	}
}

// TestActiveSDLCChainItemsMatchDashOrder pins the render order: the
// `chain edit` file lists runs in the same grouped order the dash's
// ACTIVE section shows — chains as contiguous head→tail blocks, each
// unit floating by its most-recent member. Built over one fixture (an
// orphan, a 3-run chain, a 2-run chain) and asserted against both the
// expected sequence and a live dash.BuildRows pass, so the two views
// can't drift apart.
func TestActiveSDLCChainItemsMatchDashOrder(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	mk := func(project, id string) *run.Metadata {
		return &run.Metadata{ID: id, Project: project, Workflow: "sdlc", Status: run.StatusInProgress}
	}
	mds := []*run.Metadata{
		mk("p", "a"), mk("p", "b"), mk("p", "c"), // chain a→b→c, rep = c (14:00)
		mk("p", "d"), mk("p", "e"), // chain d→e, rep = e (10:00)
		mk("p", "x"), // orphan (11:00)
	}
	when := map[string]time.Time{
		"a": base.Add(-6 * time.Hour),
		"b": base.Add(-5 * time.Hour),
		"c": base,
		"d": base.Add(-7 * time.Hour),
		"e": base.Add(-4 * time.Hour),
		"x": base.Add(-3 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c", "p/d": "p/e"}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	blocks := activeSDLCChainItems(mds, idx, byKey)
	// The blocks are the dash units: the 3-run chain, the orphan, then
	// the 2-run chain. The blank lines the render writes between these
	// are the chain boundaries.
	var gotBlocks [][]string
	var gotChain []string
	for _, block := range blocks {
		var keys []string
		for _, it := range block {
			keys = append(keys, it.Key)
			gotChain = append(gotChain, it.Key)
		}
		gotBlocks = append(gotBlocks, keys)
	}
	wantBlocks := [][]string{{"p/a", "p/b", "p/c"}, {"p/x"}, {"p/d", "p/e"}}
	if !reflect.DeepEqual(gotBlocks, wantBlocks) {
		t.Fatalf("chain edit blocks = %v, want %v", gotBlocks, wantBlocks)
	}
	want := []string{"p/a", "p/b", "p/c", "p/x", "p/d", "p/e"}
	if !reflect.DeepEqual(gotChain, want) {
		t.Fatalf("chain edit order = %v, want %v", gotChain, want)
	}

	// The same inputs through the dash must yield the same ACTIVE order.
	next := map[string]dash.NextDecision{}
	for _, md := range mds {
		next[md.ID] = dash.NextDecision{Stage: "code"}
	}
	rows, err := dash.BuildRows(dash.Inputs{
		Now:       base.Add(time.Hour),
		Runs:      mds,
		Index:     idx,
		NextByRun: next,
	})
	if err != nil {
		t.Fatalf("BuildRows: %v", err)
	}
	var gotDash []string
	for _, r := range rows {
		if r.Bucket == dash.BucketActiveRuns {
			gotDash = append(gotDash, r.Project+"/"+r.Run)
		}
	}
	if !reflect.DeepEqual(gotChain, gotDash) {
		t.Fatalf("chain edit order %v != dash ACTIVE order %v", gotChain, gotDash)
	}
}

// TestChainEditRoundTripIsNoOp is the headline regression: render the
// live state, parse the rendered file back, diff against live, and get
// an empty diff. The render groups runs into exactly the live chains
// and the parser reads those same blocks back, so opening `chain edit`
// and saving unchanged touches nothing. This is the footgun fix —
// chain-edit-order's "a no-edit save fuses everything" — turned into a
// test. It holds for fan-in-free states; a child with two live parents
// can't be represented in the linear file (see the design's edge cases)
// and a no-edit save still collapses the second edge.
func TestChainEditRoundTripIsNoOp(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	mk := func(project, id string) *run.Metadata {
		return &run.Metadata{ID: id, Project: project, Workflow: "sdlc", Status: run.StatusInProgress}
	}
	mds := []*run.Metadata{
		mk("p", "a"), mk("p", "b"), mk("p", "c"), // chain a→b→c
		mk("p", "d"), mk("p", "e"), // chain d→e
		mk("p", "x"), // orphan
	}
	when := map[string]time.Time{
		"a": base.Add(-6 * time.Hour),
		"b": base.Add(-5 * time.Hour),
		"c": base,
		"d": base.Add(-7 * time.Hour),
		"e": base.Add(-4 * time.Hour),
		"x": base.Add(-3 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c", "p/d": "p/e"}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	body := renderChainEditFile(activeSDLCChainItems(mds, idx, byKey))
	parsed, err := parseChainEditFile(body)
	if err != nil {
		t.Fatalf("parseChainEditFile of rendered body: %v\n%s", err, body)
	}
	adds, removes := diffChainEdit(parsed, idx.ChainedChild)
	if len(adds) != 0 || len(removes) != 0 {
		t.Fatalf("round-trip should be a no-op: adds=%v removes=%v\nrendered:\n%s", adds, removes, body)
	}
}

func TestChainAnnotationMultipleParentsFanIn(t *testing.T) {
	// A child can be the live edge from more than one parent
	// (cross-parent fan-in). The chained-from annotation lists
	// all of them, sorted for deterministic display.
	byKey := map[string]*run.Metadata{
		"p/a": {ID: "a", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
		"p/b": {ID: "b", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
		"p/c": {ID: "c", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
	}
	chainedChild := map[string]string{"p/a": "p/c", "p/b": "p/c"}
	chainedFrom := invertEffectiveChain(chainedChild, byKey)
	if got := chainAnnotation("p/c", chainedChild, chainedFrom, byKey); got != "chained-from p/a, p/b" {
		t.Errorf("p/c annotation = %q, want \"chained-from p/a, p/b\"", got)
	}
}
