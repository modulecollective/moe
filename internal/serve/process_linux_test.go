//go:build linux

package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a tiny self-exec helper for the shutdown
// phase-3 test. When MOE_TEST_IGNORE_SIGNALS=1 is set in the env
// the binary installs SIG_IGN for INT and TERM, touches the file
// named by MOE_TEST_READY_FILE so the test can sync on
// signal-handler-installed, then waits for SIGHUP and exits — i.e.
// survives the two Ctrl-Cs of phase 1/2 but dies cleanly when phase
// 3's pty.Close lands. Everything else routes through the normal
// test entry point.
func TestMain(m *testing.M) {
	if os.Getenv("MOE_TEST_IGNORE_SIGNALS") == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		if ready := os.Getenv("MOE_TEST_READY_FILE"); ready != "" {
			_ = os.WriteFile(ready, nil, 0o644)
		}
		<-hup
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestChildKeepsAnOutputTail: PTY bytes used to go straight on the floor,
// so a sweep that died to a vendor error left an exit code and nothing
// else. The tail is what makes the difference between a glance and an ssh
// session.
func TestChildKeepsAnOutputTail(t *testing.T) {
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/echo", []string{"credit limit reached"}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	if got := c.tail(); !strings.Contains(got, "credit limit reached") {
		t.Errorf("tail = %q, want the child's output", got)
	}
}

// TestChildTailIsBounded: the tail rides in memory for the life of the
// process, so a chatty child must not be able to grow it without limit.
func TestChildTailIsBounded(t *testing.T) {
	c := &child{}
	chunk := strings.Repeat("x", 4096)
	for range 20 {
		c.appendTail([]byte(chunk))
	}
	if got := len(c.tail()); got != childTailBytes {
		t.Errorf("tail is %d bytes after 80KB of output, want it capped at %d", got, childTailBytes)
	}
}

// TestChildTailKeepsTheNewestBytes: a child that died says why in its last
// lines, so the ring has to drop from the front.
func TestChildTailKeepsTheNewestBytes(t *testing.T) {
	c := &child{}
	c.appendTail([]byte(strings.Repeat("o", childTailBytes)))
	c.appendTail([]byte("the actual error"))
	if got := c.tail(); !strings.HasSuffix(got, "the actual error") {
		t.Errorf("tail = %q…%q, want it to end with the newest bytes", got[:20], got[len(got)-20:])
	}
}

// TestChildHooksLandInTheActivityRing: the registry fires one spawn and
// one exit hook per child, which is how every spawn site — a phone-launched
// run, a chore, a heartbeat sweep — reaches the activity record without
// each one remembering to say so.
func TestChildHooksLandInTheActivityRing(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/false"})
	if _, err := s.children.spawn("alpha/fix-it", "/bin/false", nil, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := s.children.get("alpha/fix-it")
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}

	waitFor(t, "both hooks to land", func() bool {
		return len(s.activity.panel(time.Now()).Events) == 2
	})
	vm := s.activity.panel(time.Now())
	// Newest first: the exit leads.
	if vm.Events[0].Kind != "exit" || !vm.Events[0].Failed {
		t.Errorf("newest event = %+v, want the failed exit", vm.Events[0])
	}
	if vm.Events[1].Kind != "spawn" {
		t.Errorf("older event = %+v, want the spawn", vm.Events[1])
	}
}

// TestSpawnAndReap is the minimum-viable spawn check: a child
// records under the requested id, its read loop drains the master
// PTY to EIO, and `done` closes after `cmd.Wait` returns. With the
// rename / tail apparatus gone, that's all spawn is on the hook for.
func TestSpawnAndReap(t *testing.T) {
	cs := newChildren()
	_, err := cs.spawn("p/r", "/bin/echo", []string{"-n", "hi"}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, ok := cs.get("p/r")
	if !ok {
		t.Fatal("expected child in registry")
	}

	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	exited, exitErr := c.snapshot()
	if !exited {
		t.Fatal("expected child to report exited")
	}
	if exitErr != nil {
		t.Errorf("exit err: %v", exitErr)
	}
}

func TestSpawnRefusesDuplicateLiveID(t *testing.T) {
	cs := newChildren()
	if _, err := cs.spawn("dup/run", "/bin/sleep", []string{"1"}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	defer func() {
		if c, ok := cs.get("dup/run"); ok {
			_ = c.pty.Close()
		}
	}()

	if _, err := cs.spawn("dup/run", "/bin/echo", []string{"hi"}, t.TempDir(), io.Discard); err == nil {
		t.Fatal("second spawn should refuse duplicate id")
	}
}

func TestRunPageRendersForExitedChild(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/echo", []string{"-n", "marker"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	<-c.done

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	// Swap in our pre-populated children registry so the test
	// doesn't need to re-spawn.
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"p/r", "exited"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// The collapsed per-run page renders no PTY tail, no chain
	// prompt, no end-agent button. Asserting absence keeps the
	// trim honest.
	for _, banned := range []string{
		"marker", "End Agent", "chain prompt", "activity",
		"/key", "/end-agent",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("collapsed page must not contain %q\n%s", banned, body)
		}
	}
}

func TestRunPage404ForUnknownRun(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/nope/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

// withShortShutdownGrace shrinks the phase budgets for the duration
// of a test so we don't spend 20+ seconds per shutdown case. Not
// safe under t.Parallel.
func withShortShutdownGrace(t *testing.T, soft, hangup time.Duration) {
	t.Helper()
	origSoft, origHangup := shutdownSoftGrace, shutdownHangupGrace
	shutdownSoftGrace = soft
	shutdownHangupGrace = hangup
	t.Cleanup(func() {
		shutdownSoftGrace = origSoft
		shutdownHangupGrace = origHangup
	})
}

// TestShutdownPhaseTwoExitsCat exercises the Ctrl-C + natural-exit
// branch of children.shutdown: /bin/cat in PTY cooked mode receives
// SIGINT from the \x03 byte and dies within the grace window.
func TestShutdownPhaseTwoExitsCat(t *testing.T) {
	withShortShutdownGrace(t, 2*time.Second, 500*time.Millisecond)
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/cat", nil, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	logger := &strings.Builder{}
	done := make(chan struct{})
	go func() {
		cs.shutdown(context.Background(), logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownSoftGrace + 2*time.Second):
		_ = c.pty.Close()
		t.Fatal("shutdown didn't return within phase-2 budget")
	}
	if !strings.Contains(logger.String(), "exited cleanly") {
		t.Errorf("expected 'exited cleanly' log line, got:\n%s", logger.String())
	}
	select {
	case <-c.done:
	default:
		t.Error("child should be reaped after shutdown")
	}
}

// TestShutdownPhaseThreeHangsUpStubbornChild exercises the hang-up
// branch: a child that ignores SIGINT/SIGTERM survives the two
// Ctrl-Cs, so shutdown moves on to pty.Close after the soft grace
// window and the child dies via SIGHUP from the controlling-terminal
// disconnect. The "stubborn child" is the test binary re-exec'd with
// MOE_TEST_IGNORE_SIGNALS=1 (see TestMain), which installs SIG_IGN
// for INT and TERM and blocks until SIGHUP.
func TestShutdownPhaseThreeHangsUpStubbornChild(t *testing.T) {
	withShortShutdownGrace(t, 500*time.Millisecond, 2*time.Second)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// File-based ready sync: the helper touches readyFile once
	// signal.Ignore is in place. Without this sync, the race between
	// exec-start and signal-handler-install can let the default
	// SIGINT handler kill the child mid-startup.
	readyFile := filepath.Join(t.TempDir(), "ready")
	t.Setenv("MOE_TEST_IGNORE_SIGNALS", "1")
	t.Setenv("MOE_TEST_READY_FILE", readyFile)

	cs := newChildren()
	if _, err := cs.spawn("p/r", self, []string{"-test.run=^$"}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		shutdownSoftGrace+shutdownHangupGrace+2*time.Second)
	defer cancel()

	// SIGKILL the helper after the test regardless of whether
	// SIGHUP managed to take it out — the helper is contrived and
	// production-side a real moe child wouldn't ignore SIGHUP, so
	// the test just verifies the *shape* of the phase walk.
	t.Cleanup(func() {
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})

	logger := &strings.Builder{}
	done := make(chan struct{})
	go func() {
		cs.shutdown(ctx, logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownSoftGrace + shutdownHangupGrace + 3*time.Second):
		_ = c.pty.Close()
		t.Fatal("shutdown didn't return within total budget")
	}
	// The phase-3 advance is the assertion: shutdown survived the
	// two Ctrl-Cs, exhausted the soft grace, and reached the
	// hang-up branch.
	if !strings.Contains(logger.String(), "hanging up PTY") {
		t.Errorf("expected 'hanging up PTY' log line, got:\n%s", logger.String())
	}
	// And it walked all the way to phase 4 — anything still alive
	// is left for the kernel to reap on os.Exit, as designed.
	if !strings.Contains(logger.String(), "leaving for kernel reap") {
		t.Errorf("expected 'leaving for kernel reap' log line, got:\n%s", logger.String())
	}
}

// TestShutdownWithALiveChildLeavesNoStateFile is the shutdown ordering
// the whole crash signal rests on. children.shutdown returns as soon as
// each child's done channel closes, but the reader goroutine runs its
// exit hook — and the state-file save inside it — after closing it, and
// the sweep watchers aren't waited on at all. So the ordinary Ctrl-C
// with a live child has a straggling save landing after ListenAndServe
// removed the file, re-creating it. Serve then exits leaving a record
// that names a pid which no longer exists, and every later `moe dash`
// reads a crash that never happened — permanently, until the next serve
// start.
//
// The window is real but short, so the test holds the hook open across
// the shutdown rather than betting on the scheduler. The wrapper only
// delays the real hook; what it does when it runs is untouched.
func TestShutdownWithALiveChildLeavesNoStateFile(t *testing.T) {
	withShortShutdownGrace(t, 2*time.Second, 500*time.Millisecond)
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/cat"})

	hooked := s.children.onExit
	release, exited := make(chan struct{}), make(chan struct{})
	s.children.onExit = func(id string, at time.Time, exitErr error, tail string) {
		<-release
		hooked(id, at, exitErr, tail)
		close(exited)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe(ctx) }()
	waitFor(t, "the state file to appear on listen", func() bool {
		_, ok, _ := ReadActivitySnapshot(root)
		return ok
	})

	// /bin/cat in PTY cooked mode dies to the shutdown's Ctrl-C: a child
	// that exits during the wind-down, which is the common case.
	if _, err := s.children.spawn("alpha/fix-it", "/bin/cat", nil, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the child's exit hook")
	}

	if _, ok, _ := ReadActivitySnapshot(root); ok {
		t.Error("a straggling save re-created the state file after a clean shutdown; " +
			"every later `moe dash` would report a crash that never happened")
	}
}
