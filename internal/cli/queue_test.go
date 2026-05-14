package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/queue"
	"github.com/modulecollective/moe/internal/run"
)

// fastQueueCountdown shrinks the per-tick interval so a 3-tick
// countdown takes ~3ms instead of ~3s. Tests that drive runQueueRun
// through the new countdown pay the wait once per dispatched item, so
// keeping the prod tick would slow the suite by a multiple. Restored
// on cleanup.
func fastQueueCountdown(t *testing.T) {
	t.Helper()
	old := queueCountdownTick
	queueCountdownTick = 1 * time.Millisecond
	t.Cleanup(func() { queueCountdownTick = old })
}

// markRunStatus rewrites run.json's status field directly. Test helper
// for tests that need a "merged" or "closed" run without driving the
// full close path; the queue's drop logic only reads run.json's status.
func markRunStatus(t *testing.T, root, projectID, runID, status string) {
	t.Helper()
	path := filepath.Join(root, "projects", projectID, "runs", runID, "run.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var md map[string]any
	if err := json.Unmarshal(b, &md); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	md["status"] = status
	out, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readQueue returns the on-disk queue. Empty/missing → nil.
func readQueue(t *testing.T, root string) []queue.Item {
	t.Helper()
	items, err := queue.Load(root)
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	return items
}

// openSdlcRun opens an empty sdlc run by shelling runNew (no --one-shot,
// just create the run dir). Returns the slug. Title becomes the slug.
func openSdlcRun(t *testing.T, projectID, title string) string {
	t.Helper()
	suppressNextStagePrompt(t)
	stubEditor(t)
	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{projectID, title}, &out, &errb); code != 0 {
		t.Fatalf("runNew %q: exit=%d stderr=%q", title, code, errb.String())
	}
	return run.Slugify(title)
}

func TestQueueRegistered(t *testing.T) {
	cmd, ok := commands["queue"]
	if !ok {
		t.Fatal(`expected top-level command "queue" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("queue command summary should not be empty")
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"add", "remove", "list", "run"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("queue usage missing subcommand %q: %q", want, out.String())
		}
	}
}

func TestQueueAddRefusesMissingRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"sdlc", "tele", "no-such-run"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", out.String(), errb.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be empty, got %v", items)
	}
}

func TestQueueAddRefusesTerminalRun(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Some run")
	markRunStatus(t, root, "tele", slug, run.StatusMerged)

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), run.StatusMerged) {
		t.Fatalf("expected merged in stderr, got: %q", errb.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be empty, got %v", items)
	}
}

func TestQueueAddRejectsDuplicate(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Dup test")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("first add: exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal on duplicate; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "already queued at position 1") {
		t.Fatalf("expected position-1 message, got: %q", errb.String())
	}
}

// TestQueueAddIdeaLazy pins the new lazy-idea-queue shape: `moe queue
// add idea <project> <slug>` just appends a pointer; the idea is not
// promoted at add time. Until the walker dispatches the item the idea
// stays in_progress and the canvas is unchanged on disk.
func TestQueueAddIdeaLazy(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	captureIdea(t, "tele", "Promote me")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"idea", "tele", "promote-me"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "queued idea tele promote-me") {
		t.Fatalf("missing queued line in stdout: %q", out.String())
	}
	items := readQueue(t, root)
	if len(items) != 1 {
		t.Fatalf("expected 1 queued item, got %d: %v", len(items), items)
	}
	want := queue.Item{Workflow: "idea", Project: "tele", Run: "promote-me"}
	if items[0] != want {
		t.Fatalf("queued item wrong: got %+v want %+v", items[0], want)
	}
	// Idea is still in_progress — lazy means not yet promoted.
	idea, err := run.Load(root, "tele", "promote-me")
	if err != nil {
		t.Fatalf("load idea: %v", err)
	}
	if idea.Status != run.StatusInProgress {
		t.Fatalf("idea should still be in_progress, got %q", idea.Status)
	}
}

// TestQueueAddIdeaRefusesMissing mirrors the sdlc check: a queue add
// against a nonexistent idea is a fail-loud at the verb boundary, not
// a deferred surprise at dispatch.
func TestQueueAddIdeaRefusesMissing(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"idea", "tele", "nope"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing idea; stdout=%q", out.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be untouched, got %v", items)
	}
}

// TestQueueAddIdeaRefusesWrongWorkflow guards against `queue add idea
// <project> <slug>` pointing at an sdlc run. The workflow on the
// queue item has to match the run's workflow on disk.
func TestQueueAddIdeaRefusesWrongWorkflow(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Wrong shape")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"idea", "tele", slug}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not idea") {
		t.Fatalf("expected workflow-mismatch reason, got: %q", errb.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be untouched, got %v", items)
	}
}

// TestQueueAddFromIdeaFlagRemoved makes the removal of --from-idea
// visible at the CLI: a typed-by-muscle-memory invocation must fail
// with a hint at the new lazy form, not pretend it parsed.
func TestQueueAddFromIdeaFlagRemoved(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"sdlc", "--from-idea=foo", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "--from-idea was removed") {
		t.Fatalf("expected migration hint, got: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "moe queue add idea") {
		t.Fatalf("expected pointer to new form, got: %q", errb.String())
	}
}

func TestQueueAddFront(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "First")
	second := openSdlcRun(t, "tele", "Second")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: exit=%d stderr=%q", code, errb.String())
	}
	if code := runQueueAdd([]string{"--front", "sdlc", "tele", second}, &out, &errb); code != 0 {
		t.Fatalf("add second --front: exit=%d stderr=%q", code, errb.String())
	}
	items := readQueue(t, root)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %v", items)
	}
	if items[0].Run != second || items[1].Run != first {
		t.Fatalf("expected --front to prepend; got order: %v", items)
	}
}

func TestQueueRemove(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Removable")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runQueueRemove([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("remove: exit=%d stderr=%q", code, errb.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("expected empty queue, got %v", items)
	}
	// Removing again is non-zero with a clear message.
	out.Reset()
	errb.Reset()
	if code := runQueueRemove([]string{"sdlc", "tele", slug}, &out, &errb); code == 0 {
		t.Fatalf("expected non-zero on no-op remove; stderr=%q", errb.String())
	}
}

func TestQueueListEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := runQueueList(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "queue is empty") {
		t.Fatalf("expected empty marker, got: %q", out.String())
	}
}

func TestQueueListPreviewsNextStage(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Needs design")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runQueueList(nil, &out, &errb); code != 0 {
		t.Fatalf("list: exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "sdlc tele "+slug) {
		t.Fatalf("expected space-separated identity in output: %q", got)
	}
	if !strings.Contains(got, "next: design") {
		t.Fatalf("expected next: design in output: %q", got)
	}
}

func TestQueueListMarksDeadItems(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Will be merged")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}
	// Mark merged after queueing (simulates a hand-driven push between
	// add and walk).
	markRunStatus(t, root, "tele", slug, run.StatusMerged)

	out.Reset()
	errb.Reset()
	if code := runQueueList(nil, &out, &errb); code != 0 {
		t.Fatalf("list: exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "will drop") {
		t.Fatalf("expected drop marker: %q", got)
	}
	if !strings.Contains(got, "merged") {
		t.Fatalf("expected merged reason: %q", got)
	}
}

// TestQueueRunExitsOnEmpty pins the design's "empty queue → exit 0"
// property: the walker prints one line and exits without waiting for
// adds from another terminal. The previous shape was an idle poll, and
// the seed for this run was an operator finding that loop weird —
// guard against it coming back.
func TestQueueRunExitsOnEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	walkerExit := make(chan int, 1)
	walkerOut := &safeBuffer{}
	walkerErr := &safeBuffer{}
	go func() {
		walkerExit <- runQueueRun(nil, walkerOut, walkerErr)
	}()

	select {
	case code := <-walkerExit:
		if code != 0 {
			t.Fatalf("walker exit=%d stderr=%q", code, walkerErr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("walker did not exit on empty queue within 2s")
	}
	if got := strings.TrimSpace(walkerOut.String()); got != "queue: empty" {
		t.Fatalf("expected exactly 'queue: empty' on stdout, got: %q", got)
	}
}

// stubDispatch swaps out dispatchQueueItem for the duration of the test
// and returns a recorder. Tests do not run in parallel, so the
// global swap is safe.
type dispatchRecorder struct {
	mu    sync.Mutex
	calls []queue.Item
	exit  func(it queue.Item) int
}

func (r *dispatchRecorder) record(it queue.Item, _, _ io.Writer) int {
	r.mu.Lock()
	r.calls = append(r.calls, it)
	r.mu.Unlock()
	if r.exit != nil {
		return r.exit(it)
	}
	return 0
}

// runWalkerExpectingDrain spawns runQueueRun in a goroutine and waits
// for it to exit on its own — the walker now returns the moment a
// peek finds an empty queue, so tests that drain the queue don't need
// to signal it.
func runWalkerExpectingDrain(t *testing.T) (int, string, string) {
	t.Helper()
	walkerOut := &safeBuffer{}
	walkerErr := &safeBuffer{}
	walkerExit := make(chan int, 1)
	go func() {
		walkerExit <- runQueueRun(nil, walkerOut, walkerErr)
	}()
	select {
	case code := <-walkerExit:
		return code, walkerOut.String(), walkerErr.String()
	case <-time.After(5 * time.Second):
		t.Fatal("walker did not exit within 5s")
		return 0, "", ""
	}
}

func stubDispatch(t *testing.T, exit func(it queue.Item) int) *dispatchRecorder {
	t.Helper()
	rec := &dispatchRecorder{exit: exit}
	old := dispatchQueueItem
	dispatchQueueItem = rec.record
	t.Cleanup(func() { dispatchQueueItem = old })
	return rec
}

func TestQueueRunWalksItemsInOrder(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "First item")
	second := openSdlcRun(t, "tele", "Second item")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", second}, &out, &errb); code != 0 {
		t.Fatalf("add second: %d %q", code, errb.String())
	}

	rec := stubDispatch(t, nil)

	code, _, errOut := runWalkerExpectingDrain(t)
	if code != 0 {
		t.Fatalf("walker exit=%d stderr=%q", code, errOut)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("expected 2 dispatches, got %d: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].Run != first || rec.calls[1].Run != second {
		t.Fatalf("dispatch order wrong: %v", rec.calls)
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be drained, got %v", items)
	}
}

func TestQueueRunDropsDeadItem(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	dead := openSdlcRun(t, "tele", "Dead one")
	live := openSdlcRun(t, "tele", "Live one")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", dead}, &out, &errb); code != 0 {
		t.Fatalf("add dead: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", live}, &out, &errb); code != 0 {
		t.Fatalf("add live: %d %q", code, errb.String())
	}
	markRunStatus(t, root, "tele", dead, run.StatusMerged)

	rec := stubDispatch(t, nil)

	code, walkerOut, walkerErr := runWalkerExpectingDrain(t)
	if code != 0 {
		t.Fatalf("walker exit=%d stderr=%q", code, walkerErr)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 dispatch (live only), got %d: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].Run != live {
		t.Fatalf("expected live dispatch, got: %v", rec.calls[0])
	}
	if !strings.Contains(walkerOut, "dropping sdlc tele "+dead) {
		t.Fatalf("expected drop log line: %q", walkerOut)
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be drained, got %v", items)
	}
}

func TestQueueRunStopsOnFailure(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "Fails")
	second := openSdlcRun(t, "tele", "Survives")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", second}, &out, &errb); code != 0 {
		t.Fatalf("add second: %d %q", code, errb.String())
	}

	rec := stubDispatch(t, func(it queue.Item) int {
		if it.Run == first {
			return 7
		}
		return 0
	})

	out.Reset()
	errb.Reset()
	code := runQueueRun(nil, &out, &errb)
	if code != 7 {
		t.Fatalf("expected exit 7, got %d (stderr=%q)", code, errb.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("walker should have stopped after first failure, got %v", rec.calls)
	}
	items := readQueue(t, root)
	if len(items) != 2 || items[0].Run != first {
		t.Fatalf("failed item should remain at head: %v", items)
	}
}

func TestQueueRunReleasesLockDuringDispatch(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "Walker target")
	second := openSdlcRun(t, "tele", "Concurrent add target")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}

	started := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	rec := stubDispatch(t, func(it queue.Item) int {
		// Only the first dispatch (the one we're racing against) signals
		// + waits. Subsequent dispatches (the concurrently-added second
		// item, picked up after the walker pops the first) just return
		// so the walker drains and the goroutine exits cleanly.
		if it.Run == first {
			once.Do(func() { close(started) })
			<-proceed
		}
		return 0
	})

	walkerExit := make(chan int, 1)
	walkerOut := &safeBuffer{}
	walkerErr := &safeBuffer{}
	go func() {
		walkerExit <- runQueueRun(nil, walkerOut, walkerErr)
	}()

	<-started
	// Walker is mid-dispatch. A concurrent queue add must NOT block on
	// the walker holding the queue's lock — the contract says the lock
	// is released before dispatch runs.
	addOut := &bytes.Buffer{}
	addErr := &bytes.Buffer{}
	if code := runQueueAdd([]string{"sdlc", "tele", second}, addOut, addErr); code != 0 {
		t.Fatalf("concurrent add blocked or failed: code=%d stderr=%q", code, addErr.String())
	}
	close(proceed)
	// With idle removed, the walker exits on the next peek that finds
	// the queue empty — wait for that exit instead of breaking out.
	select {
	case code := <-walkerExit:
		if code != 0 {
			t.Fatalf("walker exit=%d stderr=%q", code, walkerErr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walker did not drain within 5s")
	}
	if len(rec.calls) < 1 || rec.calls[0].Run != first {
		t.Fatalf("walker should have dispatched first, got %v", rec.calls)
	}
}

// TestQueueRunCountdownPrecedesEveryDispatch is the design property:
// the countdown gates every dispatch, including the first item. With
// the per-tick interval shrunk to ~1ms by fastQueueCountdown, a 3-tick
// countdown still fires its three "starting … in N…" frames before
// dispatch is invoked. Catches a regression that skips the countdown
// for the first item (the seed's "between items" framing nearly led
// the design there).
func TestQueueRunCountdownPrecedesEveryDispatch(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "First")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	rec := stubDispatch(t, nil)

	code, walkerOut, walkerErr := runWalkerExpectingDrain(t)
	if code != 0 {
		t.Fatalf("walker exit=%d stderr=%q", code, walkerErr)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(rec.calls))
	}
	for _, want := range []string{"starting sdlc tele " + slug + " in 3", "in 2", "in 1"} {
		if !strings.Contains(walkerOut, want) {
			t.Errorf("countdown frame %q missing from stdout:\n%s", want, walkerOut)
		}
	}
}

// TestQueueRunStopsOnSignalDuringCountdown is the load-bearing case
// for the design: SIGINT during the countdown returns 0 with the head
// still at head and a "stopped" message. Drives the stop by overriding
// dispatchQueueItem to never fire (it would only run after countdown
// completed) and racing a real syscall.SIGINT into the process while
// the walker is mid-countdown — the same path the operator's Ctrl-C
// takes in production.
func TestQueueRunStopsOnSignalDuringCountdown(t *testing.T) {
	// Slow the tick down so the walker is reliably *inside* the
	// countdown when we raise SIGINT — fastQueueCountdown's 1ms
	// would race the test goroutine's signal-raise.
	old := queueCountdownTick
	queueCountdownTick = 200 * time.Millisecond
	t.Cleanup(func() { queueCountdownTick = old })

	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "Stays at head")
	second := openSdlcRun(t, "tele", "Never reached")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", second}, &out, &errb); code != 0 {
		t.Fatalf("add second: %d %q", code, errb.String())
	}

	// Dispatch must not fire — the test's contract is "stopped during
	// the first item's countdown." If it does, the test fails loudly
	// rather than silently passing on the wrong path.
	rec := stubDispatch(t, func(it queue.Item) int {
		t.Errorf("dispatch should not have fired; got %v", it)
		return 0
	})

	walkerExit := make(chan int, 1)
	walkerOut := &bytes.Buffer{}
	walkerErr := &bytes.Buffer{}
	go func() {
		walkerExit <- runQueueRun(nil, walkerOut, walkerErr)
	}()

	// Give the walker time to enter the countdown's signal-listening
	// select (signal.Notify must be installed before the kill, or the
	// default handler tears the test process down). One tick interval
	// is enough — the walker reaches the select on the very first tick.
	time.Sleep(queueCountdownTick / 2)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}

	select {
	case code := <-walkerExit:
		if code != 0 {
			t.Fatalf("walker exit=%d stderr=%q", code, walkerErr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walker did not exit within 5s of SIGINT")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("dispatch should not have fired, got %v", rec.calls)
	}
	if !strings.Contains(walkerOut.String(), "queue: stopped") {
		t.Errorf("expected stopped message, got: %q", walkerOut.String())
	}
	if !strings.Contains(walkerOut.String(), first) {
		t.Errorf("expected stopped message to name head item, got: %q", walkerOut.String())
	}
	// Queue file must be untouched — head still at head, both items present.
	items := readQueue(t, root)
	if len(items) != 2 || items[0].Run != first || items[1].Run != second {
		t.Fatalf("queue should be unchanged, got %v", items)
	}
}

// TestQueueListPreviewsIdeaItem pins the preview column shape for an
// idea queue item: `promote → sdlc, next: design`. Different from sdlc
// items because the idea hasn't been promoted yet — the column has to
// predict the post-dispatch state instead of reading it off the run.
func TestQueueListPreviewsIdeaItem(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	captureIdea(t, "tele", "Promote me lazy")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"idea", "tele", "promote-me-lazy"}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runQueueList(nil, &out, &errb); code != 0 {
		t.Fatalf("list: exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "idea tele promote-me-lazy") {
		t.Fatalf("expected idea identity in output: %q", got)
	}
	if !strings.Contains(got, "promote → sdlc, next: design") {
		t.Fatalf("expected lazy-promote preview: %q", got)
	}
}

// TestQueueRunPromotesIdeaItemLazily is the load-bearing dispatch
// property: when the walker hits an idea queue item, it calls
// promoteIdeaToSdlcRun first and then drives the resulting sdlc run.
// The dispatch stub records what the walker handed it — for an idea
// item that should be the *new* sdlc run, not the original idea
// pointer. Identity-pop on the way out removes the idea triple (not
// the new sdlc one) so the queue reflects what was added.
func TestQueueRunPromotesIdeaItemLazily(t *testing.T) {
	fastQueueCountdown(t)
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	captureIdea(t, "tele", "Promote on walk")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"idea", "tele", "promote-on-walk"}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	rec := stubDispatch(t, nil)

	code, walkerOut, walkerErr := runWalkerExpectingDrain(t)
	if code != 0 {
		t.Fatalf("walker exit=%d stderr=%q", code, walkerErr)
	}
	// Dispatch fires for the idea item itself — defaultDispatchQueueItem
	// owns the promote-then-resume choreography, and the stub replaces
	// the whole thing. We're verifying the walker dispatched the idea
	// item exactly once.
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 dispatch, got %d: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0].Workflow != "idea" || rec.calls[0].Run != "promote-on-walk" {
		t.Fatalf("expected idea dispatch, got: %+v", rec.calls[0])
	}
	// Queue drained — identity-pop matched on the idea triple.
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be drained, got %v", items)
	}
	_ = walkerOut
}

// TestQueueEditNoEditorRefuses pins the editor gate: no $EDITOR /
// $VISUAL → refuse with a clear message, don't crash trying to spawn
// an empty command. Mirrors `idea edit`.
func TestQueueEditNoEditorRefuses(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Edit me")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	// noEditor must come after openSdlcRun (which calls stubEditor) so
	// the editor gate trips on queue edit rather than on the seed.
	noEditor(t)
	out.Reset()
	errb.Reset()
	code := runQueueEdit(nil, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal without editor; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "set $EDITOR") {
		t.Fatalf("expected $EDITOR hint, got: %q", errb.String())
	}
}

// TestQueueEditEmptyQueueShortCircuits pins the empty-queue branch:
// no editor, no tempfile, exit 0 with the same `(queue is empty)`
// marker `queue list` prints. The design's open question came down on
// the side of read-verb parity.
func TestQueueEditEmptyQueueShortCircuits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)

	var out, errb bytes.Buffer
	code := runQueueEdit(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "queue is empty") {
		t.Fatalf("expected empty marker, got: %q", out.String())
	}
}

// TestQueueEditReorders is the happy path: the operator swaps two
// lines and saves; the on-disk order flips. Uses a scripted "editor"
// that rewrites the tempfile in place.
func TestQueueEditReorders(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "First")
	second := openSdlcRun(t, "tele", "Second")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", second}, &out, &errb); code != 0 {
		t.Fatalf("add second: %d %q", code, errb.String())
	}

	scriptedEditor(t, []queue.Item{
		{Workflow: "sdlc", Project: "tele", Run: second},
		{Workflow: "sdlc", Project: "tele", Run: first},
	})

	out.Reset()
	errb.Reset()
	if code := runQueueEdit(nil, &out, &errb); code != 0 {
		t.Fatalf("edit: exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "queue updated") {
		t.Fatalf("expected success marker, got: %q", out.String())
	}
	items := readQueue(t, root)
	if len(items) != 2 || items[0].Run != second || items[1].Run != first {
		t.Fatalf("expected reordered queue, got %v", items)
	}
}

// TestQueueEditDropsItems pins delete-a-line semantics: removing a
// line from the buffer removes the item from the queue.
func TestQueueEditDropsItems(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	keep := openSdlcRun(t, "tele", "Keep me")
	drop := openSdlcRun(t, "tele", "Drop me")

	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", keep}, &out, &errb); code != 0 {
		t.Fatalf("add keep: %d %q", code, errb.String())
	}
	if code := runQueueAdd([]string{"sdlc", "tele", drop}, &out, &errb); code != 0 {
		t.Fatalf("add drop: %d %q", code, errb.String())
	}

	scriptedEditor(t, []queue.Item{
		{Workflow: "sdlc", Project: "tele", Run: keep},
	})

	out.Reset()
	errb.Reset()
	if code := runQueueEdit(nil, &out, &errb); code != 0 {
		t.Fatalf("edit: exit=%d stderr=%q", code, errb.String())
	}
	items := readQueue(t, root)
	if len(items) != 1 || items[0].Run != keep {
		t.Fatalf("expected only keep to remain, got %v", items)
	}
}

// TestQueueEditDropAllEmptiesQueue is the all-deleted variant: an
// empty buffer (after comment-stripping) means an empty queue. The
// design names this explicitly so the test pins it.
func TestQueueEditDropAllEmptiesQueue(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Will be dropped")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	scriptedEditorRaw(t, "# all dropped\n")

	out.Reset()
	errb.Reset()
	if code := runQueueEdit(nil, &out, &errb); code != 0 {
		t.Fatalf("edit: exit=%d stderr=%q", code, errb.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("expected empty queue, got %v", items)
	}
}

// TestQueueEditRefusesNewIdentity pins the design's "adds go through
// queue add" rule: an identity that wasn't in the buffer at edit-open
// time gets refused, the buffer is saved to a `.bak`, and the on-disk
// queue is untouched.
func TestQueueEditRefusesNewIdentity(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "On queue")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	pinQueueEditTimestamp(t, 1715500000)
	scriptedEditorRaw(t, "sdlc tele not-in-original\n")

	out.Reset()
	errb.Reset()
	code := runQueueEdit(nil, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "not in the queue") {
		t.Fatalf("expected new-identity reason, got: %q", errb.String())
	}
	// Queue is untouched.
	items := readQueue(t, root)
	if len(items) != 1 || items[0].Run != slug {
		t.Fatalf("queue should be untouched, got %v", items)
	}
	// Backup exists.
	bak := filepath.Join(root, ".moe", "queue.json.edit-1715500000.bak")
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("expected backup at %s: %v", bak, err)
	}
}

// TestQueueEditRefusesMalformedLine pins the "three tokens, no slash
// form, known workflow" rule.
func TestQueueEditRefusesMalformedLine(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "On queue")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	scriptedEditorRaw(t, "sdlc tele/"+slug+"\n")

	out.Reset()
	errb.Reset()
	code := runQueueEdit(nil, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "expected") {
		t.Fatalf("expected malformed-line reason, got: %q", errb.String())
	}
	if items := readQueue(t, root); len(items) != 1 || items[0].Run != slug {
		t.Fatalf("queue should be untouched, got %v", items)
	}
}

// TestQueueEditRefusesDuplicate pins the no-duplicate rule.
func TestQueueEditRefusesDuplicate(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	slug := openSdlcRun(t, "tele", "Dupe me")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", slug}, &out, &errb); code != 0 {
		t.Fatalf("add: %d %q", code, errb.String())
	}

	scriptedEditorRaw(t, "sdlc tele "+slug+"\nsdlc tele "+slug+"\n")

	out.Reset()
	errb.Reset()
	code := runQueueEdit(nil, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "appears twice") {
		t.Fatalf("expected duplicate reason, got: %q", errb.String())
	}
}

// TestQueueEditRefusesConcurrentChange pins the optimistic-concurrency
// check: while the editor is open we don't hold the lock, so another
// terminal can mutate .moe/queue.json. On re-acquire we compare against
// the snapshot and refuse if it shifted. Drives the change by writing
// queue.json directly between the editor invocation and the lock
// re-acquire — modelled via a scripted editor that performs the
// mutation itself.
func TestQueueEditRefusesConcurrentChange(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	first := openSdlcRun(t, "tele", "First")
	second := openSdlcRun(t, "tele", "Second")
	var out, errb bytes.Buffer
	if code := runQueueAdd([]string{"sdlc", "tele", first}, &out, &errb); code != 0 {
		t.Fatalf("add first: %d %q", code, errb.String())
	}

	// The "editor" mutates .moe/queue.json (simulating a concurrent
	// `queue add` from another terminal) and rewrites the tempfile to
	// the reordering the operator intended. On re-acquire, the queue's
	// snapshot from step 1 won't match the file on disk → refuse.
	pinQueueEditTimestamp(t, 1715500001)
	concurrentBody := `[
  {"workflow":"sdlc","project":"tele","run":"` + first + `"},
  {"workflow":"sdlc","project":"tele","run":"` + second + `"}
]
`
	scriptedEditorWithSideEffect(t, "sdlc tele "+first+"\n", queue.Path(root), concurrentBody)

	out.Reset()
	errb.Reset()
	code := runQueueEdit(nil, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "queue changed while editing") {
		t.Fatalf("expected concurrent-change reason, got: %q", errb.String())
	}
	// Backup written.
	bak := filepath.Join(root, ".moe", "queue.json.edit-1715500001.bak")
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("expected backup at %s: %v", bak, err)
	}
	// Concurrent add survived — the queue still has both items in the
	// order the concurrent writer left them.
	items := readQueue(t, root)
	if len(items) != 2 || items[0].Run != first || items[1].Run != second {
		t.Fatalf("expected concurrent state preserved, got %v", items)
	}
}

// scriptedEditor installs a `sh`-flavoured EDITOR that rewrites the
// edited tempfile to the supplied item list (one item per line,
// space-separated). Returns once launchEditor finishes; runQueueEdit
// then reads the rewritten tempfile.
func scriptedEditor(t *testing.T, items []queue.Item) {
	t.Helper()
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "%s %s %s\n", it.Workflow, it.Project, it.Run)
	}
	scriptedEditorRaw(t, b.String())
}

// scriptedEditorRaw installs an EDITOR that overwrites the tempfile
// with raw body (caller controls every line, including malformed
// shapes used by refusal tests).
func scriptedEditorRaw(t *testing.T, body string) {
	t.Helper()
	scriptDir := t.TempDir()
	bodyPath := filepath.Join(scriptDir, "body.txt")
	if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(scriptDir, "editor.sh")
	script := "#!/bin/sh\ncat \"" + bodyPath + "\" > \"$1\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", scriptPath)
	t.Setenv("VISUAL", "")
}

// scriptedEditorWithSideEffect installs an EDITOR that performs a
// scripted side-effect (writing sidePath with sideBody, simulating a
// concurrent terminal's `queue add`) AND rewrites the tempfile to
// bufferBody. Used by the concurrent-change test to mutate the
// on-disk queue between the snapshot and the re-acquire.
func scriptedEditorWithSideEffect(t *testing.T, bufferBody, sidePath, sideBody string) {
	t.Helper()
	scriptDir := t.TempDir()
	bodyPath := filepath.Join(scriptDir, "body.txt")
	if err := os.WriteFile(bodyPath, []byte(bufferBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sideBodyPath := filepath.Join(scriptDir, "side.txt")
	if err := os.WriteFile(sideBodyPath, []byte(sideBody), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(scriptDir, "editor.sh")
	// Write the concurrent change first, then rewrite the tempfile.
	script := "#!/bin/sh\ncp \"" + sideBodyPath + "\" \"" + sidePath + "\" && cat \"" + bodyPath + "\" > \"$1\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", scriptPath)
	t.Setenv("VISUAL", "")
}

// pinQueueEditTimestamp pins queueEditNow so tests can predict the
// `.bak` filename. Restored on cleanup.
func pinQueueEditTimestamp(t *testing.T, ts int64) {
	t.Helper()
	old := queueEditNow
	queueEditNow = func() int64 { return ts }
	t.Cleanup(func() { queueEditNow = old })
}
