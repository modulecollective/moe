package serve

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestCanvasRouteRendersBody: the happy path — a baked canvas file
// on disk renders inside <pre> with the body and an mtime line.
func TestCanvasRouteRendersBody(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	body := "# Design body\n\nfirst paragraph.\n"
	canvasPath := writeCanvas(t, root, "alpha", "fix-it", "design", body)

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(_, _, _ string) (string, error) {
			return canvasPath, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/canvas/design", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	for _, want := range []string{
		`<pre class="canvas-body">`,
		"# Design body",
		"first paragraph.",
		`href="/run/alpha/fix-it"`,
		"alpha/fix-it",
		"design",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q\n%s", want, out)
		}
	}
}

// TestCanvasRouteMissingFileEmptyState: a resolver that points at a
// non-existent file (stale bookmark to a stage that hasn't been
// written yet) renders 200 with an empty-state message — not 404 —
// so the operator sees where the file *would* live.
func TestCanvasRouteMissingFileEmptyState(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	ghost := filepath.Join(root, "projects", "alpha", "runs", "fix-it",
		"documents", "design", "content.md")

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(_, _, _ string) (string, error) {
			return ghost, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/canvas/design", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	out := rr.Body.String()
	if !strings.Contains(out, "no canvas yet") {
		t.Errorf("body should announce empty state, got:\n%s", out)
	}
	if strings.Contains(out, `<pre class="canvas-body">`) {
		t.Errorf("body should not render an empty <pre>, got:\n%s", out)
	}
}

// TestCanvasRouteBogusStage404: when ResolveCanvas refuses (unknown
// project / run / stage / workflow-mismatch), the handler maps that
// to 404 with the resolver's message in the body.
func TestCanvasRouteBogusStage404(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		ResolveCanvas: func(_, _, stage string) (string, error) {
			return "", fmt.Errorf("no such stage: %s", stage)
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/canvas/bogus", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such stage: bogus") {
		t.Errorf("body should surface resolver message, got:\n%s", rr.Body.String())
	}
}

// TestCanvasRouteWithoutResolver500: an unwired Options.ResolveCanvas
// is configuration drift — fail loud rather than serve nothing.
func TestCanvasRouteWithoutResolver500(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/canvas/design", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestCanvasRouteIOError500: ResolveCanvas returns a path, but
// reading it errors with something other than not-exist (e.g. a
// directory in place of a file). The handler must surface this as
// 500, not as empty-state.
func TestCanvasRouteIOError500(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	dirPath := filepath.Join(root, "projects", "alpha", "runs", "fix-it",
		"documents", "design")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(_, _, _ string) (string, error) {
			return dirPath, nil
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/canvas/design", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 for non-not-exist read error, got %d body=%s",
			rr.Code, rr.Body.String())
	}
	// Sanity-check the empty-state branch *only* fires for
	// fs.ErrNotExist, not the broader "read failed" family.
	if _, err := os.ReadFile(dirPath); errors.Is(err, fs.ErrNotExist) {
		t.Fatal("test fixture: reading a directory should not return ErrNotExist")
	}
}

// seedRun writes a minimal project.json + run.json under root — no
// git, no commit. Sufficient for handler tests that only need
// run.Load to succeed.
func seedRun(t *testing.T, root, projectID, runID, workflow string) {
	t.Helper()
	seedProject(t, root, projectID)
	md := &run.Metadata{
		ID:        runID,
		Project:   projectID,
		Status:    run.StatusInProgress,
		Workflow:  workflow,
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}

// writeCanvas drops a content.md under the run's documents/<stage>/
// dir, returning the absolute path so callers can wire it into a
// ResolveCanvas closure.
func writeCanvas(t *testing.T, root, projectID, runID, stage, body string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", projectID, "runs", runID, "documents", stage)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "content.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
