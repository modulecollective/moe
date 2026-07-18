package cli

import (
	"bytes"
	"io"
	"os"
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

// keysOf builds an offered-run set from the file's blocks, plus any
// extra runs the editor showed but the file no longer lists (deleted
// lines). Most diff tests offer exactly the runs in the file.
func keysOf(blocks [][]string, extra ...string) map[string]bool {
	m := map[string]bool{}
	for _, b := range blocks {
		for _, k := range b {
			m[k] = true
		}
	}
	for _, k := range extra {
		m[k] = true
	}
	return m
}

func TestDiffChainEditNewChainOnAllOrphans(t *testing.T) {
	// All three runs are unchained today. The save links them
	// head-first into a linear chain.
	blocks := [][]string{{"p/a", "p/b", "p/c"}}
	live := map[string]string{}
	adds, removes := diffChainEdit(blocks, keysOf(blocks), live)
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
	adds, removes := diffChainEdit(blocks, keysOf(blocks), live)
	wantAdds := []string{"p/a p/c", "p/c p/b"}
	wantRemoves := []string{"p/a p/b"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v", adds, wantAdds)
	}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Errorf("removes = %v, want %v", removes, wantRemoves)
	}
}

func TestDiffChainEditDeletedRunUnchains(t *testing.T) {
	// Decision 3: a run the editor offered but the operator deleted
	// from the file gets its outgoing edge cleared — delete unchains.
	// p/x was offered and has a live edge but is absent from `desired`;
	// its edge must be dropped, not left alone.
	blocks := [][]string{{"p/a", "p/b"}}
	live := map[string]string{
		"p/x": "p/y",
		"p/a": "p/old",
	}
	offered := keysOf(blocks, "p/x", "p/y")
	adds, removes := diffChainEdit(blocks, offered, live)
	// p/a's edge changes from old → b; p/b is the file's last line so
	// its desired-child is "" matching live's absence → no-op. p/x was
	// deleted, so its live edge clears.
	wantAdds := []string{"p/a p/b"}
	wantRemoves := []string{"p/a p/old", "p/x p/y"}
	if !reflect.DeepEqual(adds, wantAdds) {
		t.Errorf("adds = %v, want %v", adds, wantAdds)
	}
	if !reflect.DeepEqual(removes, wantRemoves) {
		t.Errorf("removes = %v, want %v", removes, wantRemoves)
	}
}

func TestDiffChainEditUnofferedParentUntouched(t *testing.T) {
	// The scope guard on Decision 3: a parent the editor never showed
	// (terminal or suppressed) keeps its live edge. p/x has a live edge
	// but was not offered, so it must not be cleared — a save can't
	// touch an edge the operator never saw.
	blocks := [][]string{{"p/a", "p/b"}}
	live := map[string]string{
		"p/x": "p/y",
		"p/a": "p/old",
	}
	offered := keysOf(blocks) // p/x deliberately absent from the offered set
	adds, removes := diffChainEdit(blocks, offered, live)
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
	adds, removes := diffChainEdit(blocks, keysOf(blocks), live)
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
	adds, removes := diffChainEdit(blocks, keysOf(blocks), live)
	if len(adds) != 0 || len(removes) != 0 {
		t.Errorf("two separate chains should produce no trailers: adds=%v removes=%v", adds, removes)
	}
	// And with no live edges, each block links only within itself —
	// no p/b p/c edge bridging the boundary.
	adds, removes = diffChainEdit(blocks, keysOf(blocks), map[string]string{})
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
	adds, removes := diffChainEdit(blocks, keysOf(blocks), live)
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
// chain ride: when a `!!!` parent (rideChain=true) ships at push and a
// live trailer points at an unresolved child, the cascade transparently
// opens that child at its first pending stage, headless. The recursion
// is by construction — the child's own push then re-fires the hook on
// its outgoing edge, so `!!!` rides the whole chain in one shot.
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
	res, code := cascadeFromGate("code", "", false, true, parentMD, &stdout, &stderr)
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
	// → review → test via openSdlcStage and ships at push. That gives
	// seven dispatches: parent (code, review, test), child (design,
	// code, review, test).
	// push happens via pushFromCascade for both.
	wantStages := []string{"code", "review", "test", "design", "code", "review", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v\nstdout=%q", gotStages, wantStages, stdout.String())
	}
	// The child's four dispatches must be against the child run.
	childInvs := (*openCaptured)[3:]
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

// TestCascadeFromGateShipDoesNotRide pins the `!!` half of the new axis:
// rideChain=false ships this run and stops, even with a live chained
// child. Same fixture as TestCascadeFromGateRidesIntoLiveChainChild —
// the only difference is the rideChain flag — so the two read as a
// matched pair: `!!!` rides, `!!` doesn't.
func TestCascadeFromGateShipDoesNotRide(t *testing.T) {
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
	// Parent only: code, review, test, push. The child is never opened.
	if got := len(*pushCaptured); got != 1 {
		t.Fatalf("pushFromCascade dispatched %d times, want 1 (`!!` ships this run only)", got)
	}
	wantStages := []string{"code", "review", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v (`!!` must not ride into the child)", gotStages, wantStages)
	}
	if strings.Contains(stdout.String(), "chain: riding") {
		t.Errorf("`!!` must not print a chain-ride preamble, stdout:\n%s", stdout.String())
	}
}

// seedChainedPushGateRun stands up a parent+child sdlc chain and chdirs
// into the root, the shared fixture for the two push-gate ride tests.
// Returns the bureaucracy root and the parent metadata the prompt is
// driven against.
func seedChainedPushGateRun(t *testing.T) (string, *run.Metadata) {
	t.Helper()
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
	t.Chdir(root)
	return root, parentMD
}

// feedStdin pipes a single answer line into os.Stdin for a chain-prompt
// test, restoring the original on cleanup.
func feedStdin(t *testing.T, answer string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, err := io.WriteString(w, answer+"\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
}

// TestPromptPushNextStageBangBangBangRidesChain pins Move 3: `!!!` at
// the push gate ships this run (the typed cascade merge path) and then rides
// into the next live chained child, opening it at its first pending
// stage. The push gate is a separate prompt handler from
// dispatchCascade, so this ride is the bit the run added — before, the
// push gate never rode the chain at all.
func TestPromptPushNextStageBangBangBangRidesChain(t *testing.T) {
	root, parentMD := seedChainedPushGateRun(t)

	var shipped bool
	next := &Command{Name: "push", Run: func(_ []string, _, _ io.Writer) int { shipped = true; return 0 }}
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)
	feedStdin(t, "!!!")

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, parentMD, "moe sdlc push tele parent-run", &stdout, &stderr); code != 0 {
		t.Fatalf("push prompt exit=%d stderr=%q", code, stderr.String())
	}
	if shipped {
		t.Fatalf("`!!!` at push gate must not dispatch through Command.Run")
	}
	// The child opens at design and walks to its own push.
	wantStages := []string{"design", "code", "review", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
		if inv.runID != "child-run" {
			t.Errorf("ride dispatch routed to %q, want child-run: %+v", inv.runID, inv)
		}
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("ridden child stages = %v, want %v\nstdout=%q", gotStages, wantStages, stdout.String())
	}
	if got := len(*pushCaptured); got != 2 {
		t.Fatalf("pushFromCascade dispatched %d times, want 2 (parent + child ships)", got)
	}
	if got, want := strings.Join((*pushCaptured)[0].args, " "), "tele/parent-run"; got != want {
		t.Fatalf("parent push args = %q, want %q", got, want)
	}
	if !(*pushCaptured)[0].options.SkipTerminalEdit {
		t.Fatalf("parent push SkipTerminalEdit = false, want true")
	}
	if !strings.Contains(stdout.String(), "chain: riding into tele/child-run at design (headless)") {
		t.Errorf("expected chain-ride preamble in stdout, got:\n%s", stdout.String())
	}
}

// TestPromptPushNextStageBangBangDoesNotRide is the matched `!!` half:
// at the push gate `!!` ships this run and stops — no ride into the
// live child, even though the chain edge exists. Same fixture as the
// `!!!` test; the only difference is the answer.
func TestPromptPushNextStageBangBangDoesNotRide(t *testing.T) {
	root, parentMD := seedChainedPushGateRun(t)

	var shipped bool
	next := &Command{Name: "push", Run: func(_ []string, _, _ io.Writer) int { shipped = true; return 0 }}
	openCaptured := stubOpenSdlcStage(t, nil)
	pushCaptured := stubPushFromCascade(t, 0, nil)
	feedStdin(t, "!!")

	var stdout, stderr bytes.Buffer
	if code := promptPushNextStage(next, nil, nil, root, parentMD, "moe sdlc push tele parent-run", &stdout, &stderr); code != 0 {
		t.Fatalf("push prompt exit=%d stderr=%q", code, stderr.String())
	}
	if shipped {
		t.Fatalf("`!!` at push gate must not dispatch through Command.Run")
	}
	if len(*openCaptured) != 0 {
		t.Fatalf("`!!` must not ride into the child: got dispatches %+v", *openCaptured)
	}
	if len(*pushCaptured) != 1 {
		t.Fatalf("`!!` must ship only the parent: got pushes %+v", *pushCaptured)
	}
	if got, want := strings.Join((*pushCaptured)[0].args, " "), "tele/parent-run"; got != want {
		t.Fatalf("parent push args = %q, want %q", got, want)
	}
	if !(*pushCaptured)[0].options.SkipTerminalEdit {
		t.Fatalf("parent push SkipTerminalEdit = false, want true")
	}
	if strings.Contains(stdout.String(), "chain: riding") {
		t.Errorf("`!!` at push gate must not print a chain-ride preamble, stdout:\n%s", stdout.String())
	}
}

// TestCascadeFromGateRideInterruptHaltsParent is the verified-incident
// regression: an operator Ctrl-C inside a chain-ridden child halts the
// whole cascade instead of being swallowed. The parent ships, rides into
// the child, the child's first stage (design) is interrupted
// (exitInterrupted), and that code propagates back through maybeRideChain
// so the parent cascade returns exitInterrupted — no further child stages
// dispatch, no re-prompt. res.shipped stays true because the parent
// genuinely did ship before the ride; the abort code halts everything
// above it.
func TestCascadeFromGateRideInterruptHaltsParent(t *testing.T) {
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

	t.Chdir(root)
	// The child starts at design; interrupt it there. Parent starts at
	// code, so design only ever fires for the child — the parent's own
	// walk (code, review, test) is unaffected.
	openCaptured := stubOpenSdlcStage(t, map[string]int{"design": exitInterrupted})
	pushCaptured := stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, true, parentMD, &stdout, &stderr)
	if code != exitInterrupted {
		t.Fatalf("cascade exit=%d, want %d (interrupt propagates from the ride); stderr=%q", code, exitInterrupted, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent shipped before the ride; res.shipped must stay true: %+v", res)
	}
	// Parent: code, review, test, push (ship). Child: design only —
	// interrupted there, so the child's code/review/test never dispatch.
	wantStages := []string{"code", "review", "test", "design"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v (interrupt stops the child walk)\nstdout=%q", gotStages, wantStages, stdout.String())
	}
	// Parent shipped exactly once; the child never reached its push.
	if got := len(*pushCaptured); got != 1 {
		t.Fatalf("pushFromCascade dispatched %d times, want 1 (parent only; child interrupted before its push)", got)
	}
	// The operator sees the child's interrupted summary.
	if !strings.Contains(stdout.String(), "design interrupted — stopped") {
		t.Errorf("expected child interrupted summary in stdout, got:\n%s", stdout.String())
	}
}

// TestCascadeFromGateRideOrdinaryFailureStillSwallowed is the contract's
// other half: an ordinary (non-interrupt) child-cascade failure stays
// best-effort swallowed — the parent's ship is authoritative and a
// sideways child must not retroactively fail it. Only Ctrl-C halts the
// parent. Mirror of the interrupt test with a bare exit 1 in the child.
func TestCascadeFromGateRideOrdinaryFailureStillSwallowed(t *testing.T) {
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

	t.Chdir(root)
	openCaptured := stubOpenSdlcStage(t, map[string]int{"design": 1})
	stubPushFromCascade(t, 0, nil)

	var stdout, stderr bytes.Buffer
	res, code := cascadeFromGate("code", "", false, true, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d, want 0 (ordinary child failure stays swallowed); stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must still ship: %+v", res)
	}
	// Child interrupted-free failure: design dispatched, code/review/test did not.
	wantStages := []string{"code", "review", "test", "design"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v", gotStages, wantStages)
	}
	// The swallow is logged on stderr (not propagated as the parent's code).
	if !strings.Contains(stderr.String(), "chain ride into tele/child-run exited 1") {
		t.Errorf("expected swallowed-failure stderr line, got:\n%s", stderr.String())
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
	res, code := cascadeFromGate("code", "", false, true, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must ship: %+v", res)
	}
	// Only parent's code+test dispatched. No child stages.
	wantStages := []string{"code", "review", "test"}
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
	res, code := cascadeFromGate("code", "", false, true, parentMD, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cascade exit=%d stderr=%q", code, stderr.String())
	}
	if !res.shipped {
		t.Fatalf("parent cascade must ship: %+v", res)
	}
	wantStages := []string{"code", "review", "test"}
	gotStages := make([]string, 0, len(*openCaptured))
	for _, inv := range *openCaptured {
		gotStages = append(gotStages, inv.stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Fatalf("openSdlcStage stages = %v, want %v (cleared edge must not ride)", gotStages, wantStages)
	}
}

// TestActiveChainItemsMembership pins that chain-edit membership keys
// on operatorCascades, not on workflow=="sdlc": every operator-paced
// workflow (sdlc, twin, kb, hooks, chores) is offered, while chat
// (perpetual) and pulse (machine-paced) stay out — the same predicate
// the stage-verb flags and serve chips use. chain is the one workflow
// admitted on top of the predicate: it has no stage ladder of its own,
// so it takes no cascade flags, but the operator must be able to prune
// and reorder the batch it heads. A merged run is excluded by status
// regardless of workflow.
func TestActiveChainItemsMembership(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	mk := func(id, workflow, status string) *run.Metadata {
		return &run.Metadata{ID: id, Project: "p", Workflow: workflow, Status: status}
	}
	mds := []*run.Metadata{
		mk("s", "sdlc", run.StatusInProgress),
		mk("t", "twin", run.StatusInProgress),
		mk("k", "kb", run.StatusInProgress),
		mk("h", "hooks", run.StatusInProgress),
		mk("c", "chores", run.StatusInProgress),
		mk("q", chainWorkflow, run.StatusInProgress), // batch head — admitted on top of the predicate
		mk("chat1", "chat", run.StatusInProgress),    // perpetual — excluded
		mk("pulse1", "pulse", run.StatusInProgress),  // machine-paced — excluded
		mk("done", "sdlc", run.StatusMerged),         // terminal — excluded
	}
	when := map[string]time.Time{}
	byKey := map[string]*run.Metadata{}
	for i, md := range mds {
		key := md.Project + "/" + md.ID
		when[key] = base.Add(time.Duration(-i) * time.Hour)
		byKey[key] = md
	}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: map[string]string{}}

	blocks := activeChainItems(mds, idx, byKey)
	got := map[string]bool{}
	for _, block := range blocks {
		for _, it := range block {
			got[it.Key] = true
		}
	}
	want := map[string]bool{"p/s": true, "p/t": true, "p/k": true, "p/h": true, "p/c": true, "p/q": true}
	for k := range want {
		if !got[k] {
			t.Errorf("chainable run %q should be offered for chaining", k)
		}
	}
	for _, k := range []string{"p/chat1", "p/pulse1", "p/done"} {
		if got[k] {
			t.Errorf("non-cascade run %q must not be offered for chaining", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("offered set = %v, want exactly %v", got, want)
	}
}

// TestChainEditTwinEdgeSurvivesSave: a twin run can head a chain and its
// outgoing edge survives a no-op round trip — proving twin is a
// first-class chain member now, not just visible. Renders live state
// (an sdlc run chained to a twin run), parses it back, and asserts the
// diff is empty so the twin edge is neither dropped nor duplicated.
func TestChainEditTwinEdgeSurvivesSave(t *testing.T) {
	base := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	mds := []*run.Metadata{
		{ID: "code-it", Project: "p", Workflow: "sdlc", Status: run.StatusInProgress},
		{ID: "reflect-x", Project: "p", Workflow: "twin", Status: run.StatusInProgress},
	}
	when := map[string]time.Time{"p/code-it": base, "p/reflect-x": base.Add(-time.Hour)}
	chained := map[string]string{"p/code-it": "p/reflect-x"} // sdlc → twin
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	body := renderChainEditFile(activeChainItems(mds, idx, byKey))
	parsed, err := parseChainEditFile(body)
	if err != nil {
		t.Fatalf("parseChainEditFile of rendered body: %v\n%s", err, body)
	}
	offered := map[string]bool{"p/code-it": true, "p/reflect-x": true}
	adds, removes := diffChainEdit(parsed, offered, idx.ChainedChild)
	if len(adds) != 0 || len(removes) != 0 {
		t.Fatalf("twin edge must survive a no-op save: adds=%v removes=%v\n%s", adds, removes, body)
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
		"p/a": base.Add(-6 * time.Hour),
		"p/b": base.Add(-5 * time.Hour),
		"p/c": base,
		"p/d": base.Add(-7 * time.Hour),
		"p/e": base.Add(-4 * time.Hour),
		"p/x": base.Add(-3 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c", "p/d": "p/e"}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	blocks := activeChainItems(mds, idx, byKey)
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
		next[md.Project+"/"+md.ID] = dash.NextDecision{Stage: "code"}
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
		"p/a": base.Add(-6 * time.Hour),
		"p/b": base.Add(-5 * time.Hour),
		"p/c": base,
		"p/d": base.Add(-7 * time.Hour),
		"p/e": base.Add(-4 * time.Hour),
		"p/x": base.Add(-3 * time.Hour),
	}
	chained := map[string]string{"p/a": "p/b", "p/b": "p/c", "p/d": "p/e"}
	idx := &run.JournalIndex{LastActivity: when, ChainedChild: chained}
	byKey := map[string]*run.Metadata{}
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	body := renderChainEditFile(activeChainItems(mds, idx, byKey))
	parsed, err := parseChainEditFile(body)
	if err != nil {
		t.Fatalf("parseChainEditFile of rendered body: %v\n%s", err, body)
	}
	offered := map[string]bool{}
	for _, md := range mds {
		offered[md.Project+"/"+md.ID] = true
	}
	adds, removes := diffChainEdit(parsed, offered, idx.ChainedChild)
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
