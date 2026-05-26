//go:build linux

package serve

import (
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
	tail, _, exited, exitErr := c.snapshot()
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
