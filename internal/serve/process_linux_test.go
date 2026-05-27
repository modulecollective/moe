//go:build linux

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
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
	exited, exitErr, _ := c.snapshot()
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

// TestPOSTNewRunOpensAndSpawnsAgent drives the form path end-to-end:
// the handler opens the run in-process via runopen.Open (committing
// to git), then spawns the agent verb under the known slug. The
// fixture seeds a git-backed bureaucracy with the target project so
// run.New finds projects/alpha/project.json.
func TestPOSTNewRunOpensAndSpawnsAgent(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   root,
		MoeBin: "/bin/echo", // stand-in: any binary that exits cleanly
	})

	form := url.Values{}
	form.Set("project", "alpha")
	form.Set("slug", "first-thing")
	req := httptest.NewRequest("POST", "/run/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/first-thing" {
		t.Errorf("Location = %q, want /run/alpha/first-thing", got)
	}

	// The run is committed: run.Load must find it without the live
	// child being around.
	if _, err := run.Load(root, "alpha", "first-thing"); err != nil {
		t.Fatalf("run.Load after POST: %v", err)
	}

	c, ok := s.children.get("alpha/first-thing")
	if !ok {
		t.Fatal("child not recorded in registry under the real slug")
	}
	// `/bin/echo` with the spawn-args exits immediately.
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child never exited")
	}
}

func TestPOSTNewRunRejectsBadSlug(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
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

// TestPromotePOSTOpensRunAndSpawnsAgent drives the POST path: an
// in-progress idea is promoted in-process via runopen.Promote, the
// destination run lands on disk under a date-suffixed slug (the idea
// itself occupied the base), and the spawn registers under the
// destination's real slug — no placeholder, no rename watcher.
func TestPromotePOSTOpensRunAndSpawnsAgent(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
	seedIdeaRun(t, root, "alpha", "my-idea")

	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   root,
		MoeBin: "/bin/echo",
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

	// Promote derives the destination slug from the idea's name with
	// a date suffix on collision; the idea reserves the base.
	dated := "my-idea-" + time.Now().Local().Format("2006-01-02")
	wantLoc := "/run/alpha/" + dated
	if got := rr.Header().Get("Location"); got != wantLoc {
		t.Errorf("Location = %q, want %q", got, wantLoc)
	}

	// Destination run is on disk; source idea bumped to promoted.
	if _, err := run.Load(root, "alpha", dated); err != nil {
		t.Fatalf("destination run.Load: %v", err)
	}
	srcMD, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatalf("source idea run.Load: %v", err)
	}
	if srcMD.Status != run.StatusPromoted {
		t.Errorf("source idea status = %q, want promoted", srcMD.Status)
	}

	// Spawn is registered under the real slug.
	if _, ok := s.children.get("alpha/" + dated); !ok {
		t.Fatal("expected child registered under destination slug")
	}
	if _, ok := s.children.get("alpha/my-idea:promoting"); ok {
		t.Error("placeholder id should not appear in the registry")
	}
}

// TestPromotePOSTRejectsBadWorkspace: validation re-renders the
// idea page with an ErrorBanner. No spawn, no destination run.
func TestPromotePOSTRejectsBadWorkspace(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
	seedIdeaRun(t, root, "alpha", "my-idea")
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
	if len(s.children.all) != 0 {
		t.Errorf("invalid form must not spawn any child; registry has %d", len(s.children.all))
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

// seedBureaucracy lays down a git-initialized root with a bureaucracy
// marker and one project, then commits the seed so subsequent
// run.New calls find a clean working tree. Returns the root.
func seedBureaucracy(t *testing.T, projectID string) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(root, "projects", projectID)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	pj := map[string]string{
		"id":             projectID,
		"remote":         "git@example.test:" + projectID + ".git",
		"default_branch": "main",
		"submodule":      "modules/" + projectID,
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(pdir, "project.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "seed")
	return root
}

// seedIdeaRun lays down an in-progress idea run at <root>/projects/
// <p>/runs/<slug>/ with a one-line canvas, then commits. Sufficient
// fixture for runopen.Promote to load + seed + bump status.
func seedIdeaRun(t *testing.T, root, projectID, slug string) {
	t.Helper()
	md := &run.Metadata{
		ID:        slug,
		Project:   projectID,
		Status:    run.StatusInProgress,
		Workflow:  "idea",
		Created:   time.Now().Local().Format("2006-01-02"),
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatalf("run.Save idea: %v", err)
	}
	canvasDir := filepath.Join(root, "projects", projectID, "runs", slug, "documents", "idea")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("# idea: %s\n\nseed body\n", slug)
	if err := os.WriteFile(filepath.Join(canvasDir, "content.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "seed idea "+slug)
}
