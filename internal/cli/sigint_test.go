package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// safeBuffer is a bytes.Buffer guarded by a mutex. Used by tests that
// poll stdout from one goroutine while a helper writes it from another;
// bytes.Buffer itself is not safe for concurrent Write/String.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func stageMetadata(project, id, workflow string) *run.Metadata {
	return &run.Metadata{ID: id, Project: project, Workflow: workflow, Status: run.StatusInProgress}
}

// raiseSIGINT delivers SIGINT to the current process. Tests use this to
// drive the cooked-mode SIGINT path through the same signal.Notify
// machinery production uses.
func raiseSIGINT() error {
	return syscall.Kill(syscall.Getpid(), syscall.SIGINT)
}

// TestReadLineWithSignalReturnsLine is the happy path: the reader
// produces a line and the helper returns it, no interruption.
func TestReadLineWithSignalReturnsLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("yes\n"))
	sig := make(chan os.Signal, 1) // never written; helper must not select it
	line, interrupted, err := readLineWithSignal(r, sig)
	if err != nil && err != io.EOF {
		t.Fatalf("err=%v", err)
	}
	if interrupted {
		t.Fatalf("expected interrupted=false")
	}
	if line != "yes\n" {
		t.Fatalf("line=%q, want %q", line, "yes\n")
	}
}

// TestReadLineWithSignalReturnsOnSignal is the load-bearing case:
// when sigCh fires before the read completes, helper returns
// interrupted=true with no error and an empty line. Stand-in for the
// production path where signal.Notify writes to the channel on Ctrl-C.
func TestReadLineWithSignalReturnsOnSignal(t *testing.T) {
	// blockingReader stays blocked until the test closes done. Mirrors
	// os.Stdin sitting at a `[Y/n]` prompt with no operator input.
	done := make(chan struct{})
	r := bufio.NewReader(&blockingReader{done: done})
	defer close(done)

	sig := make(chan os.Signal, 1)
	type result struct {
		line        string
		interrupted bool
		err         error
	}
	out := make(chan result, 1)
	go func() {
		l, i, e := readLineWithSignal(r, sig)
		out <- result{l, i, e}
	}()

	// Deliver a synthetic interrupt — same shape as signal.Notify
	// writing to a buffered channel on SIGINT.
	sig <- os.Interrupt

	select {
	case res := <-out:
		if !res.interrupted {
			t.Fatalf("expected interrupted=true, got line=%q err=%v", res.line, res.err)
		}
		if res.err != nil {
			t.Fatalf("err=%v on interrupt path", res.err)
		}
		if res.line != "" {
			t.Fatalf("line=%q on interrupt; want empty", res.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLineWithSignal did not return after interrupt")
	}
}

// queueCountdownLine is the production line shape queue's countdown
// renders. Tests reuse it so an assertion drift is caught here, not in
// the production format string.
func queueCountdownLine(label string) func(n int) string {
	return func(n int) string {
		return fmt.Sprintf("queue: starting %s in %d…  (Ctrl-C to stop)", label, n)
	}
}

// TestRunCountdownTicksThenReturnsFalse is the happy path: the
// countdown runs to completion and reports stopped=false. With the
// per-tick interval shrunk to 1ms, three ticks complete in milliseconds.
func TestRunCountdownTicksThenReturnsFalse(t *testing.T) {
	old := queueCountdownTick
	queueCountdownTick = 1 * time.Millisecond
	defer func() { queueCountdownTick = old }()

	sig := make(chan os.Signal, 1)
	var buf bytes.Buffer
	stopped := runCountdown(3, queueCountdownLine("tele/x"), &buf, sig)
	if stopped {
		t.Fatalf("expected stopped=false on completed countdown")
	}
	for _, want := range []string{"in 3", "in 2", "in 1"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing frame %q in output:\n%s", want, buf.String())
		}
	}
}

// TestRunCountdownReturnsTrueOnSignal pins the stop path: a write to
// sigCh during the countdown causes runCountdown to return stopped=true
// without printing all frames. The label is included so the operator
// sees which item was about to dispatch.
func TestRunCountdownReturnsTrueOnSignal(t *testing.T) {
	old := queueCountdownTick
	queueCountdownTick = 100 * time.Millisecond
	defer func() { queueCountdownTick = old }()

	sig := make(chan os.Signal, 1)
	var buf bytes.Buffer
	out := make(chan bool, 1)
	go func() {
		out <- runCountdown(3, queueCountdownLine("tele/x"), &buf, sig)
	}()
	// Let the countdown print its first frame and enter the select.
	time.Sleep(50 * time.Millisecond)
	sig <- os.Interrupt

	select {
	case stopped := <-out:
		if !stopped {
			t.Fatalf("expected stopped=true on signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("countdown did not return after signal")
	}
	if !strings.Contains(buf.String(), "tele/x") {
		t.Errorf("expected label in output:\n%s", buf.String())
	}
}

// TestStdinSharedReaderCachesPerStdin pins the cache identity rule:
// consecutive calls return the same *bufio.Reader while os.Stdin
// holds steady, but a fresh reader after os.Stdin is swapped — the
// pattern tests use to point the helper at a pipe. Without the swap
// branch, tests that bind os.Stdin to a new pipe per case would read
// from the previous run's bufio buffer.
func TestStdinSharedReaderCachesPerStdin(t *testing.T) {
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	defer w1.Close()
	oldStdin := os.Stdin
	os.Stdin = r1
	t.Cleanup(func() { os.Stdin = oldStdin })

	first := stdinSharedReader()
	if again := stdinSharedReader(); again != first {
		t.Fatalf("expected cached reader to be reused while os.Stdin is unchanged")
	}

	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	defer w2.Close()
	os.Stdin = r2

	rebound := stdinSharedReader()
	if rebound == first {
		t.Fatalf("expected fresh reader after os.Stdin swap, got cached")
	}
	if again := stdinSharedReader(); again != rebound {
		t.Fatalf("expected new reader to be cached on subsequent calls")
	}
}

// blockingReader is an io.Reader that blocks until done is closed,
// at which point Read returns io.EOF. Stand-in for os.Stdin with
// no operator input.
type blockingReader struct {
	done chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.done
	return 0, io.EOF
}

// TestPromptStageNextStageDeclinesOnSignal pins the design's stage-prompt
// fix: a real SIGINT arriving while promptStageNextStage is blocked on
// stdin must collapse to a clean "decline" exit (code 0, no dispatch),
// not the runtime trap that used to tear moe down. Drives the helper
// directly with a never-producing pipe so the only way out is via the
// SIGINT path.
func TestPromptStageNextStageDeclinesOnSignal(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "code",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := stageMetadata("tele", "fix-it", "sdlc")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr safeBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- promptStageNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc code tele fix-it", &stdout, &stderr)
	}()

	// Wait for signal.Notify to install (the prompt prints its label
	// synchronously before the helper enters select). safeBuffer
	// guards the bytes.Buffer so this poll doesn't race the helper's
	// concurrent Write.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "[Y/n/o]") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "[Y/n/o]") {
		t.Fatalf("prompt label never printed: %q", stdout.String())
	}
	if err := raiseSIGINT(); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit=%d, want 0 on SIGINT-decline; stderr=%q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promptStageNextStage did not return after SIGINT")
	}
	if rec.ran {
		t.Errorf("dispatch ran on SIGINT-decline path")
	}
	if !strings.Contains(stdout.String(), "^C") {
		t.Errorf("expected ^C feedback in stdout, got: %q", stdout.String())
	}
}

// TestPromptPushNextStageDeclinesOnSignal mirrors the above for the
// three-way push prompt. SIGINT must collapse to the same safe sentinel
// (decline) the [N/m/p] label already defaults to — load-bearing
// because a reflex SIGINT must never accidentally ship via merge or PR.
func TestPromptPushNextStageDeclinesOnSignal(t *testing.T) {
	rec := &promptDispatchRecord{}
	next := &Command{
		Name: "push",
		Run: func(args []string, _, _ io.Writer) int {
			rec.ran = true
			return 0
		},
	}
	md := stageMetadata("tele", "fix-it", "sdlc")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var stdout, stderr safeBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- promptPushNextStage(next, nil, nil, t.TempDir(), md, "moe sdlc push tele fix-it", &stdout, &stderr)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "[N/m/p]") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "[N/m/p]") {
		t.Fatalf("prompt label never printed: %q", stdout.String())
	}
	if err := raiseSIGINT(); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit=%d, want 0 on SIGINT-decline; stderr=%q", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promptPushNextStage did not return after SIGINT")
	}
	if rec.ran {
		t.Errorf("push ran on SIGINT-decline path")
	}
}
