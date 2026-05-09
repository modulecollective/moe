package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
func readQueue(t *testing.T, root string) []queueItem {
	t.Helper()
	items, err := loadQueue(root)
	if err != nil {
		t.Fatalf("loadQueue: %v", err)
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

func TestQueueAddFromIdea(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	captureIdea(t, "tele", "Promote me")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"sdlc", "--from-idea=promote-me", "tele"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), "promoted idea tele/promote-me to sdlc run tele/") {
		t.Fatalf("missing promote line in stdout: %q", out.String())
	}
	items := readQueue(t, root)
	if len(items) != 1 {
		t.Fatalf("expected 1 queued item, got %d: %v", len(items), items)
	}
	if items[0].Workflow != "sdlc" || items[0].Project != "tele" {
		t.Fatalf("queued item shape wrong: %+v", items[0])
	}
	// Slug is date-suffixed because the idea slug collided with itself.
	if !strings.HasPrefix(items[0].Run, "promote-me-") {
		t.Fatalf("expected dated slug, got: %q", items[0].Run)
	}
	// Check the new sdlc run is loadable and the idea is marked promoted.
	md, err := run.Load(root, "tele", items[0].Run)
	if err != nil {
		t.Fatalf("load promoted run: %v", err)
	}
	if md.Workflow != "sdlc" || md.Status != run.StatusInProgress {
		t.Fatalf("promoted run shape wrong: %+v", md)
	}
	idea, err := run.Load(root, "tele", "promote-me")
	if err != nil {
		t.Fatalf("load idea: %v", err)
	}
	if idea.Status != run.StatusPromoted {
		t.Fatalf("idea should be promoted, got status=%q", idea.Status)
	}
}

func TestQueueAddFromIdeaMissing(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := runQueueAdd([]string{"sdlc", "--from-idea=nope", "tele"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero exit for missing idea; stdout=%q", out.String())
	}
	if items := readQueue(t, root); len(items) != 0 {
		t.Fatalf("queue should be untouched, got %v", items)
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
	if !strings.Contains(got, "tele/"+slug) {
		t.Fatalf("expected slug in output: %q", got)
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
	calls []queueItem
	exit  func(it queueItem) int
}

func (r *dispatchRecorder) record(it queueItem, _ queueDispatchOpts, _, _ io.Writer) int {
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

func stubDispatch(t *testing.T, exit func(it queueItem) int) *dispatchRecorder {
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
	if !strings.Contains(walkerOut, "dropping sdlc tele/"+dead) {
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

	rec := stubDispatch(t, func(it queueItem) int {
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
	rec := stubDispatch(t, func(it queueItem) int {
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
	for _, want := range []string{"starting sdlc tele/" + slug + " in 3", "in 2", "in 1"} {
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
	rec := stubDispatch(t, func(it queueItem) int {
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
