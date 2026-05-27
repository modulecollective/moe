//go:build linux

package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
	tail, _, exited, exitErr, _ := c.snapshot()
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

func TestRunPageRendersAndCanvasLinks(t *testing.T) {
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
	for _, want := range []string{"p/r", "marker", "exited"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestRunPageRendersButtonsForActivePrompt(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	// Spawn a sleeper so the child stays live; manually inject a
	// prompt-shaped line into the tail so detectPrompt picks it up.
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()
	c.appendTail([]byte("next: moe sdlc design p/r — run now? [Y/n/x/b/!]\n"))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`name="key" value="Y"`,
		`name="key" value="!"`,
		`name="key" value="!!"`,
		`name="key" value="x"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("button missing: %q\n%s", want, body)
		}
	}
}

func TestPostKeyRefusesUnknownKey(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()
	c.appendTail([]byte("next: moe sdlc design p/r — run now? [Y/n/!]\n"))

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	form := url.Values{}
	form.Set("key", "x") // not in [Y/n/!]
	req := httptest.NewRequest("POST", "/run/p/r/key", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for off-set key, got %d", rr.Code)
	}
}

func TestPostKeyRefusesIfNoActivePrompt(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()
	// no prompt in tail
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	form := url.Values{}
	form.Set("key", "Y")
	req := httptest.NewRequest("POST", "/run/p/r/key", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
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

func TestRunFragmentReturnsPartial(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r/fragment", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Fragment must contain the live subtree...
	if !strings.Contains(body, `class="run-fragment"`) {
		t.Errorf("fragment missing run-fragment wrapper:\n%s", body)
	}
	// ...and must NOT carry the page chrome (head, banner, theme toggle).
	for _, forbidden := range []string{"<!doctype", "<html", "MINISTRY OF EVERYTHING", "id=\"theme-toggle\""} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("fragment leaked chrome %q:\n%s", forbidden, body)
		}
	}
}

func TestRunFragment404ForUnknownRun(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/nope/nope/fragment", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestRunFragmentDataStopAfterGrace(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/echo", []string{"-n", "done"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
	// Backdate the exit so the grace window is already past.
	c.exitedAt = time.Now().Add(-2 * pollStopGrace)

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r/fragment", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `data-stop="1"`) {
		t.Errorf("fragment missing data-stop after grace:\n%s", rr.Body.String())
	}
}

func TestRunFragmentNoDataStopWhileLive(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r/fragment", nil))
	if strings.Contains(rr.Body.String(), `data-stop="1"`) {
		t.Errorf("fragment should not stop poller while live:\n%s", rr.Body.String())
	}
}

func TestRunPageRendersEndAgentButtonWhileLive(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"2"}, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	defer c.pty.Close()

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/p/r", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/run/p/r/end-agent") {
		t.Errorf("expected end-agent form action, got:\n%s", body)
	}
	if !strings.Contains(body, ">end agent<") {
		t.Errorf("expected end-agent button label, got:\n%s", body)
	}
}

// TestEndAgentPostExitsCat verifies the soft-EOF endpoint: two \x04
// bytes 100ms apart land in /bin/cat's stdin via the PTY's line
// discipline, which terminates it with EOF.
func TestEndAgentPostExitsCat(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/cat", nil, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	req := httptest.NewRequest("POST", "/run/p/r/end-agent", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		_ = c.pty.Close()
		t.Fatal("cat never exited after end-agent POST")
	}
}

func TestEndAgentRefusesIfAlreadyExited(t *testing.T) {
	root := t.TempDir()
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/echo", nil, root, io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")
	<-c.done

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	s.children = cs

	req := httptest.NewRequest("POST", "/run/p/r/end-agent", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
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

// TestShutdownPhaseTwoExitsCat exercises the soft-EOF + natural-exit
// branch of children.shutdown: /bin/cat sees EOT on its terminal
// stdin and exits cleanly within the grace window.
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

// TestShutdownPhaseThreeHangsUpSleep exercises the hang-up branch:
// /bin/sleep ignores EOT, so shutdown moves on to pty.Close after
// the soft grace window and the child dies via SIGHUP/SIGTERM.
func TestShutdownPhaseThreeHangsUpSleep(t *testing.T) {
	withShortShutdownGrace(t, 500*time.Millisecond, 2*time.Second)
	cs := newChildren()
	if _, err := cs.spawn("p/r", "/bin/sleep", []string{"30"}, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	c, _ := cs.get("p/r")

	ctx, cancel := context.WithTimeout(context.Background(),
		shutdownSoftGrace+shutdownHangupGrace+2*time.Second)
	defer cancel()

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
	if !strings.Contains(logger.String(), "hanging up PTY") {
		t.Errorf("expected 'hanging up PTY' log line, got:\n%s", logger.String())
	}
	select {
	case <-c.done:
	default:
		t.Error("sleep child should be reaped after hangup")
	}
}
