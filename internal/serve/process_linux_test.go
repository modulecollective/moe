//go:build linux

package serve

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a tiny self-exec helper for the shutdown
// phase-3 test. When MOE_TEST_IGNORE_SIGNALS=1 is set in the env
// the binary installs SIG_IGN for INT and TERM, prints READY to
// stderr so the test can sync on signal-handler-installed, then
// waits for SIGHUP and exits — i.e. survives the two Ctrl-Cs of
// phase 1/2 but dies cleanly when phase 3's pty.Close lands.
// Everything else routes through the normal test entry point.
func TestMain(m *testing.M) {
	if os.Getenv("MOE_TEST_IGNORE_SIGNALS") == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		fmt.Fprint(os.Stderr, "READY")
		<-hup
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSpawnFillsTailAndReapsChild(t *testing.T) {
	cs := newChildren()
	_, err := cs.spawn("p/r", "/bin/echo", []string{"-n", "tail-marker"}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, ok := cs.get("p/r")
	if !ok {
		t.Fatal("expected child in registry")
	}

	// Wait for the child to exit, then snapshot.
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	tail, exited, exitErr, _ := c.snapshot()
	if !exited {
		t.Fatal("expected child to report exited")
	}
	if exitErr != nil {
		t.Errorf("exit err: %v", exitErr)
	}
	if !strings.Contains(string(tail), "tail-marker") {
		t.Errorf("tail missing marker: %q", string(tail))
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

func TestPOSTNewRunSpawnsAndRedirects(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")

	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   root,
		MoeBin: "/bin/echo", // stand-in: any binary that exits cleanly
	})

	form := url.Values{}
	form.Set("project", "alpha")
	form.Set("slug", "first-idea")
	req := httptest.NewRequest("POST", "/run/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/first-idea" {
		t.Errorf("Location = %q, want /run/alpha/first-idea", got)
	}

	c, ok := s.children.get("alpha/first-idea")
	if !ok {
		t.Fatal("child not recorded in registry")
	}
	// /bin/echo with no args exits immediately.
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
}

func TestPOSTNewRunRejectsBadSlug(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	form := url.Values{}
	form.Set("project", "alpha")
	form.Set("slug", "Bad Slug!")
	req := httptest.NewRequest("POST", "/run/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "kebab-case") {
		t.Errorf("error banner should mention kebab-case, got:\n%s", rr.Body.String())
	}
}

func TestRunPageRendersForLiveChild(t *testing.T) {
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

// TestPromoteSpawnRenamesAfterOpenedRunLine drives the placeholder
// → real-slug rename path: spawn a child under `<p>/<s>:promoting`,
// have it print the `opened run …` line moe sdlc new emits, then
// assert the registry has rebucketed it under the new slug. Uses
// `/bin/sh -c` to drive deterministic stdout without invoking moe
// itself.
func TestPromoteSpawnRenamesAfterOpenedRunLine(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")

	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   root,
		MoeBin: "/bin/sh",
	})
	// Sneak the script in via MoeBin + args by re-spawning manually:
	// handlePromote builds args from the form, so to keep the test
	// focused on the watcher, drive children.spawn directly with the
	// echo command and the `:promoting` id.
	args := []string{"-c", "echo 'opened run alpha/my-idea-2026-05-27'; sleep 2"}
	if _, err := s.children.spawn("alpha/my-idea:promoting", "/bin/sh", args, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Poll for the rename — bounded by the child's own write speed.
	// /bin/sh + echo lands well within 1s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.children.get("alpha/my-idea-2026-05-27"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, ok := s.children.get("alpha/my-idea-2026-05-27"); !ok {
		t.Fatal("expected child to be renamed to alpha/my-idea-2026-05-27")
	}
	if _, ok := s.children.get("alpha/my-idea:promoting"); ok {
		t.Error("placeholder id should have been removed from registry")
	}

	// Cleanup: the sleep child is still alive; close its PTY.
	if c, ok := s.children.get("alpha/my-idea-2026-05-27"); ok {
		_ = c.pty.Close()
	}
}

// TestPromotePOSTSpawnsPlaceholderChild drives the full HTTP path:
// POST /promote on an in-progress idea spawns a child under the
// `:promoting` id and redirects back to the idea page. Doesn't
// assert the rename (that's the watcher test); just that the
// placeholder lands in the registry.
func TestPromotePOSTSpawnsPlaceholderChild(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")

	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   root,
		MoeBin: "/bin/sh",
	})

	form := url.Values{}
	form.Set("agent", "claude")
	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/my-idea" {
		t.Errorf("Location = %q, want /run/alpha/my-idea", got)
	}
	c, ok := s.children.get("alpha/my-idea:promoting")
	if !ok {
		t.Fatal("placeholder child not in registry")
	}
	defer c.pty.Close()
	// The fake child is `/bin/sh` with no args — it reads its
	// controlling-tty stdin and would block on us forever. /bin/sh
	// reads stdin until EOF; closing the PTY in defer is enough.
}

// TestPromotePOSTRejectsBadWorkspace: validation re-renders the
// idea page with an ErrorBanner. The spawn must not have happened.
func TestPromotePOSTRejectsBadWorkspace(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	form := url.Values{}
	form.Set("workspace", "Not A Workspace!")
	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "workspace:") {
		t.Errorf("body should surface validation error, got:\n%s", rr.Body.String())
	}
	if _, ok := s.children.get("alpha/my-idea:promoting"); ok {
		t.Error("invalid form must not spawn a placeholder child")
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

// TestShutdownPhase1WritesTwoCtrlCs is the byte-level guard for the
// "two Ctrl-Cs, not two EOTs" decision: put the slave tty in raw
// mode (-isig disables SIGINT generation, -icanon disables line
// buffering, -echo silences the line-discipline echo) so cat just
// passes its stdin through to stdout verbatim, then assert the
// master read back exactly \x03\x03. Waits for a READY marker so
// stty has settled before shutdown writes the bytes.
func TestShutdownPhase1WritesTwoCtrlCs(t *testing.T) {
	withShortShutdownGrace(t, 500*time.Millisecond, 2*time.Second)
	cs := newChildren()
	script := "stty -echo -icanon -isig; printf READY; cat"
	if _, err := cs.spawn("p/r", "/bin/sh", []string{"-c", script}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tail, _, _, _ := c.snapshot()
		if strings.Contains(string(tail), "READY") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		cs.shutdown(context.Background(), io.Discard)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownSoftGrace + shutdownHangupGrace + 3*time.Second):
		_ = c.pty.Close()
		t.Fatal("shutdown didn't return within total budget")
	}
	tail, _, _, _ := c.snapshot()
	if !strings.Contains(string(tail), "\x03\x03") {
		t.Errorf("expected raw \\x03\\x03 in cat output, got: %q", string(tail))
	}
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
	// Inherited by the child via spawn's os.Environ() snapshot.
	t.Setenv("MOE_TEST_IGNORE_SIGNALS", "1")
	cs := newChildren()
	if _, err := cs.spawn("p/r", self, []string{"-test.run=^$"}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	// Wait for the helper's READY marker so signal.Ignore is in
	// place before shutdown writes \x03. Without this sync, the
	// race between exec-start and signal-handler-install can let
	// the default SIGINT handler kill the child mid-startup.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tail, _, _, _ := c.snapshot()
		if strings.Contains(string(tail), "READY") {
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
