package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

func TestDashRouteRendersBuckets(t *testing.T) {
	now := time.Now().UTC()
	gather := func(showAll bool) ([]dash.Row, int, int, error) {
		return []dash.Row{
			{Project: "p1", Run: "r1", Note: "sdlc:design", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Hour)},
			{Project: "p2", Run: "r2", Note: "idea:capture", Bucket: dash.BucketBacklog, When: now.Add(-2 * time.Hour)},
			{Project: "p1", Run: "r0", Note: "sdlc:merged", Bucket: dash.BucketCompletedRuns, When: now.Add(-24 * time.Hour)},
		}, 3, 1, nil
	}
	s := newTestServer(t, Options{
		Addr:       "127.0.0.1:0",
		Root:       t.TempDir(),
		GatherDash: gather,
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"p1/r1", "p2/r2", "p1/r0",
		"active", "backlog", "completed",
		"3 projects registered",
		"sdlc:design", "idea:capture", "sdlc:merged",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestServePagesRenderThemeToggleInHeader(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	seedRun(t, root, "alpha", "my-idea", "idea")
	canvasPath := writeCanvas(t, root, "alpha", "fix-it", "design", "# design\n")
	now := time.Now().UTC()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherDash: func(showAll bool) ([]dash.Row, int, int, error) {
			return []dash.Row{{Project: "alpha", Run: "fix-it", Bucket: dash.BucketActiveRuns, When: now}}, 1, 1, nil
		},
		ResolveCanvas: func(_, _, _ string) (string, error) {
			return canvasPath, nil
		},
		RunStages: func(_, _ string) ([]string, error) {
			return []string{"design", "code", "test", "push"}, nil
		},
	})

	for _, path := range []string{
		"/",
		"/run/new",
		"/idea/new",
		"/run/alpha/fix-it",
		"/run/alpha/fix-it/canvas/design",
		"/run/alpha/my-idea/promote",
		"/run/alpha/my-idea/edit",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
			}
			assertThemeToggleInHeader(t, rr.Body.String())
		})
	}
}

func assertThemeToggleInHeader(t *testing.T, body string) {
	t.Helper()
	button := `<button id="theme-toggle" class="theme-toggle" type="button" aria-label="toggle theme" title="toggle theme">◐</button>`
	if got := strings.Count(body, button); got != 1 {
		t.Fatalf("want one icon theme toggle, got %d\n%s", got, body)
	}
	headerEnd := strings.Index(body, "</header>")
	buttonIdx := strings.Index(body, button)
	if headerEnd < 0 || buttonIdx < 0 || buttonIdx > headerEnd {
		t.Fatalf("theme toggle must render inside page header\n%s", body)
	}
	footerIdx := strings.Index(body, `<footer class="page-footer">`)
	if footerIdx >= 0 && strings.Index(body[footerIdx:], `id="theme-toggle"`) >= 0 {
		t.Fatalf("theme toggle must not render in footer\n%s", body)
	}
}

func TestDashRouteWithoutGatherReturns500(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDashShowAllStripsCap(t *testing.T) {
	now := time.Now().UTC()
	rows := []dash.Row{}
	for i := 0; i < dash.CompletedCap+5; i++ {
		rows = append(rows, dash.Row{
			Project: "p",
			Run:     fmt.Sprintf("r%d", i),
			Bucket:  dash.BucketCompletedRuns,
			When:    now,
		})
	}
	gather := func(showAll bool) ([]dash.Row, int, int, error) {
		return rows, 1, 0, nil
	}
	s := newTestServer(t, Options{
		Addr:       "127.0.0.1:0",
		Root:       t.TempDir(),
		GatherDash: gather,
	})

	// Default: capped at CompletedCap, header says "(cap of total)".
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	wantCap := fmt.Sprintf("(%d of %d)", dash.CompletedCap, len(rows))
	if !strings.Contains(body, wantCap) {
		t.Errorf("default render missing %q\n%s", wantCap, body)
	}

	// ?all=1: uncapped, header says total only.
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", "/?all=1", nil))
	body2 := rr2.Body.String()
	wantAll := fmt.Sprintf("(%d)", len(rows))
	if !strings.Contains(body2, wantAll) {
		t.Errorf("all=1 render missing %q\n%s", wantAll, body2)
	}
}

// TestDashLiveBadgeReflectsExitState: an active-bucket row whose
// child is still in the registry but has exited (done closed) must
// not render the "live" badge — registry presence alone overstates
// the state, since natural exit leaves the *child behind.
func TestDashLiveBadgeReflectsExitState(t *testing.T) {
	now := time.Now().UTC()
	gather := func(showAll bool) ([]dash.Row, int, int, error) {
		return []dash.Row{
			{Project: "p", Run: "running", Note: "sdlc:code", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Hour)},
			{Project: "p", Run: "exited", Note: "sdlc:code", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Hour)},
		}, 1, 1, nil
	}
	s := newTestServer(t, Options{
		Addr:       "127.0.0.1:0",
		Root:       t.TempDir(),
		GatherDash: gather,
	})

	running := &child{id: "p/running", started: now, done: make(chan struct{})}
	exited := &child{id: "p/exited", started: now, done: make(chan struct{})}
	close(exited.done)
	s.children.all["p/running"] = running
	s.children.all["p/exited"] = exited

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// One live badge — for p/running, anchored at p/exited's row breaks.
	// Anchor on the row hrefs to read presence per-row.
	runningIdx := strings.Index(body, `/run/p/running`)
	exitedIdx := strings.Index(body, `/run/p/exited`)
	if runningIdx < 0 || exitedIdx < 0 {
		t.Fatalf("both row anchors must render; body=%s", body)
	}
	// Find the next "live" badge anchored after each row label.
	if got := strings.Count(body, `class="badge live"`); got != 1 {
		t.Fatalf("want exactly one live badge, got %d\n%s", got, body)
	}
	// The single live badge must belong to the running row (i.e. appear
	// after the running anchor and before the exited anchor, since
	// active rows render in order).
	badgeIdx := strings.Index(body, `class="badge live"`)
	if !(runningIdx < badgeIdx && badgeIdx < exitedIdx) {
		t.Errorf("live badge not anchored to running row: running=%d badge=%d exited=%d", runningIdx, badgeIdx, exitedIdx)
	}
}

func TestDashRouteNotFoundForUnknownPath(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestResolveAddrEmptyOverrideDefaultsToLoopback(t *testing.T) {
	got := resolveAddr("", 4242)
	if got != "127.0.0.1:4242" {
		t.Errorf("got %s, want 127.0.0.1:4242", got)
	}
}

func TestResolveAddrOverrideWithoutPort(t *testing.T) {
	got := resolveAddr("127.0.0.1", 4242)
	if got != "127.0.0.1:4242" {
		t.Errorf("got %s, want 127.0.0.1:4242", got)
	}
}

func TestResolveAddrOverrideWithPort(t *testing.T) {
	got := resolveAddr("127.0.0.1:9999", 4242)
	if got != "127.0.0.1:9999" {
		t.Errorf("got %s, want 127.0.0.1:9999", got)
	}
}

func TestStaticAssetServed(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/static/style.css", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "font-family") {
		t.Errorf("style.css missing expected content:\n%s", rr.Body.String())
	}
}

func TestFaviconServed(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/static/favicon.svg", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	// Pin the content-type so a build host with a missing mime registration
	// fails loudly instead of silently serving application/octet-stream.
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("want image/svg+xml content-type, got %q", ct)
	}
}

func TestNewRunFormEmptyRoot(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "no projects registered") {
		t.Errorf("empty-root form should suggest `moe project add`, got:\n%s", body)
	}
	if strings.Contains(body, "<form") {
		t.Errorf("empty-root form should hide the form entirely")
	}
}

func TestNewRunFormWithProjects(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	seedProject(t, root, "beta")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<form`, `name="project"`, `name="slug"`, `name="agent"`,
		`>alpha<`, `>beta<`,
		`>claude<`, `>codex<`,
		`(default)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q\n%s", want, body)
		}
	}
}

func TestNewRunMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("PUT", "/run/new", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
	if got := rr.Header().Get("Allow"); !strings.Contains(got, "GET") || !strings.Contains(got, "POST") {
		t.Errorf("Allow header should list GET and POST, got %q", got)
	}
}

// TestNewRunSlugHasMobileAttrs: the slug input carries the mobile-
// keyboard attributes that disable initial-caps / autocorrect. Without
// these, a phone keyboard fights the kebab-case pattern regex.
func TestNewRunSlugHasMobileAttrs(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/new", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`autocapitalize="none"`,
		`autocorrect="off"`,
		`spellcheck="false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("slug input missing %q\n%s", want, body)
		}
	}
}

func TestNewIdeaFormEmptyRoot(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/idea/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "no projects registered") {
		t.Errorf("empty-root form should suggest `moe project add`, got:\n%s", body)
	}
	if strings.Contains(body, "<form") {
		t.Errorf("empty-root form should hide the form entirely")
	}
}

func TestNewIdeaFormWithProjects(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	seedProject(t, root, "beta")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/idea/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<form`, `action="/idea/new"`,
		`name="project"`, `name="slug"`, `name="body"`,
		`>alpha<`, `>beta<`,
		`<textarea`,
		// Mobile keyboard attrs on slug — the whole point of the
		// secondary "phone fights the kebab-case pattern" fix.
		`autocapitalize="none"`,
		`autocorrect="off"`,
		`spellcheck="false"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q\n%s", want, body)
		}
	}
	// No workspace/agent dropdowns — idea runs have neither.
	for _, banned := range []string{
		`name="workspace"`, `name="agent"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("idea form must not have %q\n%s", banned, body)
		}
	}
}

func TestNewIdeaMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("PUT", "/idea/new", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
	if got := rr.Header().Get("Allow"); !strings.Contains(got, "GET") || !strings.Contains(got, "POST") {
		t.Errorf("Allow header should list GET and POST, got %q", got)
	}
}

// TestNewIdeaSubmitInvalidSlug: POST with a slug that doesn't match
// the kebab-case pattern re-renders the form with the inline banner
// at 422, no run gets opened on disk. Mirrors the validation shape of
// handleNewRunSubmit.
func TestNewIdeaSubmitInvalidSlug(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	form := "project=alpha&slug=Bad_Slug&body=hello"
	req := httptest.NewRequest("POST", "/idea/new", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "slug:") {
		t.Errorf("body should carry an inline 'slug:' banner, got:\n%s", body)
	}
	// No run should have been opened.
	if _, err := os.Stat(filepath.Join(root, "projects", "alpha", "runs")); !os.IsNotExist(err) {
		t.Errorf("validation failure must not create runs dir (stat err=%v)", err)
	}
}

// TestNewIdeaSubmitInvalidProject: same shape as the slug test, for
// the project field. Closes the form-redirect loop on validation.
func TestNewIdeaSubmitInvalidProject(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	form := "project=&slug=valid-slug&body=hello"
	req := httptest.NewRequest("POST", "/idea/new", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "project:") {
		t.Errorf("body should carry an inline 'project:' banner, got:\n%s", rr.Body.String())
	}
}

// TestRunPageReadOnlyForNonParented: a run that exists on disk but
// isn't currently parented by this serve must render the per-run
// page (no 404) with canvas links pointing at the canvas route.
// This is the "view canvases from a phone for an SSH-launched run"
// path the design names.
func TestRunPageReadOnlyForNonParented(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeCanvas(t, root, "alpha", "fix-it", "design", "# design body\n")
	writeCanvas(t, root, "alpha", "fix-it", "code", "# code body\n")

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(p, r, stage string) (string, error) {
			return filepath.Join(root, "projects", p, "runs", r,
				"documents", stage, "content.md"), nil
		},
		RunStages: func(_, _ string) ([]string, error) {
			return []string{"design", "code", "test", "push"}, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`href="/run/alpha/fix-it/canvas/design"`,
		`href="/run/alpha/fix-it/canvas/code"`,
		"alpha/fix-it",
		"in_progress",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// Collapsed page shape: no end-agent button, no activity log,
	// no chain-prompt section, no key/end-agent POST routes.
	for _, banned := range []string{
		"End Agent", "<h2>agent</h2>", "<h2>activity</h2>", "<h2>chain prompt</h2>",
		"/key", "/end-agent",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("read-only page must not contain %q\n%s", banned, body)
		}
	}
	// Ladder ordering: design appears before code in the rendered
	// list (the test-stage canvas isn't on disk so its absence is
	// fine). A strict before-check would over-constrain; positional
	// check on just the two we wrote is enough.
	iDesign := strings.Index(body, `canvas/design`)
	iCode := strings.Index(body, `canvas/code`)
	if iDesign < 0 || iCode < 0 || iDesign > iCode {
		t.Errorf("canvas links not in ladder order; design=%d code=%d\n%s",
			iDesign, iCode, body)
	}
}

// TestRunPageInProgressRunSurfacesWorktreeCanvas: the bug fix.
// For an in-progress run, projects/<p>/runs/<r>/ holds only run.json
// — the documents/ tree lives under .moe/worktrees/<id>/… and is
// not yet committed to the canonical root. canvasLinks must route
// through ResolveCanvas (which returns the worktree path) instead
// of doing its own ReadDir on the canonical docs dir; otherwise the
// per-run page emits no links and the operator can't reach the
// canvas they're actively editing.
func TestRunPageInProgressRunSurfacesWorktreeCanvas(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")

	// Canonical root deliberately has no documents/ — emulates the
	// in-progress state where edits only exist in the session
	// worktree.
	docsDir := filepath.Join(root, "projects", "alpha", "runs", "fix-it", "documents")
	if _, err := os.Stat(docsDir); !os.IsNotExist(err) {
		t.Fatalf("test fixture: canonical documents/ should not exist (err=%v)", err)
	}

	// Stand-in for the worktree: a tmp dir with one stage's canvas.
	worktree := t.TempDir()
	wtCanvas := filepath.Join(worktree, "documents", "design", "content.md")
	if err := os.MkdirAll(filepath.Dir(wtCanvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wtCanvas, []byte("# live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		ResolveCanvas: func(_, _, stage string) (string, error) {
			// Mirrors the production resolver: route the design
			// stage to the worktree, leave others pointing at the
			// (missing) canonical path so Stat fails and they're
			// skipped.
			if stage == "design" {
				return wtCanvas, nil
			}
			return filepath.Join(root, "projects", "alpha", "runs", "fix-it",
				"documents", stage, "content.md"), nil
		},
		RunStages: func(_, _ string) ([]string, error) {
			return []string{"design", "code", "test", "push"}, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/run/alpha/fix-it/canvas/design"`) {
		t.Errorf("in-progress run page must link to the worktree canvas, body:\n%s", body)
	}
	// The other ladder stages have no canvas yet (worktree or canonical) —
	// they should not get links. Asserting absence keeps the test honest
	// about the "stat-driven" gating.
	for _, banned := range []string{
		`href="/run/alpha/fix-it/canvas/code"`,
		`href="/run/alpha/fix-it/canvas/test"`,
		`href="/run/alpha/fix-it/canvas/push"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("page emitted link for a stage with no canvas yet: %q", banned)
		}
	}
}

// TestRunPageMissingRun404: a slug that doesn't exist on disk and
// isn't parented returns 404, not a render with an empty header.
func TestRunPageMissingRun404(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/ghost/run/", nil))
	// ServeMux pattern routing strips trailing slash redirects;
	// the substantive check is the no-slash form.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/ghost/run", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such run") {
		t.Errorf("body should say 'no such run', got:\n%s", rr.Body.String())
	}
}

// TestIdeaPageRendersActionsForInProgressIdea: when the loaded run is
// an in-progress idea, the per-run page renders edit + promote peer
// affordances (links to /edit and /promote). The actual forms live on
// those sub-pages — not inline on the idea page.
func TestIdeaPageRendersActionsForInProgressIdea(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")

	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<section class="actions">`,
		`href="/run/alpha/my-idea/edit"`,
		`href="/run/alpha/my-idea/promote"`,
		`>edit idea<`,
		`>promote to sdlc<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// The page must not embed the form chrome any more; that lives on
	// the dedicated /promote page.
	for _, banned := range []string{
		`<h2>promote to sdlc</h2>`,
		`name="agent"`,
		`name="workspace"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("idea page must not inline promote form (found %q)\n%s", banned, body)
		}
	}
}

// TestPromotePageRendersForm: the dedicated /promote page renders the
// workspace + agent dropdowns and a POST action back to the same path.
func TestPromotePageRendersForm(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea/promote", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`action="/run/alpha/my-idea/promote"`,
		`name="agent"`,
		`>claude<`, `>codex<`, `(default)`,
		`type="submit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

// TestPromotePageRefusesNonIdea: GET on a non-idea run is 409, no
// rendered form. Same gate POST applies, so a stale bookmark fails the
// same way at either method.
func TestPromotePageRefusesNonIdea(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/promote", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSDLCPageRendersCloseChipNotIdeaAffordances: an in-progress sdlc
// run surfaces the close-run chip (POST /close) but none of the
// idea-only affordances (edit/promote) — those must not leak across
// workflows.
func TestSDLCPageRendersCloseChipNotIdeaAffordances(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<section class="actions">`,
		`<form method="post" action="/run/alpha/fix-it/close"`,
		`>close run</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sdlc page missing %q\n%s", want, body)
		}
	}
	for _, banned := range []string{
		`href="/run/alpha/fix-it/promote"`,
		`href="/run/alpha/fix-it/edit"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("sdlc page must not render idea affordance %q\n%s", banned, body)
		}
	}
}

// TestSDLCPageHidesCloseChipForTerminalRun: a merged (or otherwise
// terminal) sdlc run is past closing, so the chip drops — the action
// builder gates on in_progress.
func TestSDLCPageHidesCloseChipForTerminalRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "shipped", "sdlc")
	md, err := run.Load(root, "alpha", "shipped")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusMerged
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/shipped", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, banned := range []string{
		`<section class="actions">`,
		`action="/run/alpha/shipped/close"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("terminal sdlc page must not render the close chip (found %q)\n%s", banned, body)
		}
	}
}

// TestLiveParentedSDLCRunShowsCloseChip: even on the live-parented
// render path (an exited child still in the registry), the per-run page
// gates the close-run chip on the on-disk metadata rather than assuming
// "parented ⇒ no actions".
func TestLiveParentedSDLCRunShowsCloseChip(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	exited := &child{id: "alpha/fix-it", started: time.Now(), done: make(chan struct{})}
	close(exited.done)
	s.children.all["alpha/fix-it"] = exited

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `>close run</button>`) {
		t.Errorf("live-parented sdlc page should show the close-run chip:\n%s", rr.Body.String())
	}
}

// TestCloseRouteClosesSDLCRunAndRedirects: an in-progress sdlc run
// closes through the CloseRun callback and redirects, dropping the
// lingering exited-child registry entry so the dash badge clears.
func TestCloseRouteClosesSDLCRunAndRedirects(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")

	var gotProject, gotRun string
	called := 0
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		CloseRun: func(project, runID string) error {
			called++
			gotProject, gotRun = project, runID
			return nil
		},
	})
	// An exited child sitting idle in the registry — the common
	// grooming state. Close must drop it on success.
	exited := &child{id: "alpha/fix-it", started: time.Now(), done: make(chan struct{})}
	close(exited.done)
	s.children.all["alpha/fix-it"] = exited

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/fix-it" {
		t.Fatalf("Location=%q", got)
	}
	if called != 1 || gotProject != "alpha" || gotRun != "fix-it" {
		t.Fatalf("CloseRun called=%d project=%q run=%q", called, gotProject, gotRun)
	}
	if _, ok := s.children.get("alpha/fix-it"); ok {
		t.Errorf("exited child entry should be dropped after a successful close")
	}
}

// TestCloseRouteRefusesPushedSDLCRun: a *runopen.NotClosableError from
// the callback (pushed / terminal / wrong workflow) maps to 409 and
// surfaces the steering message in the body.
func TestCloseRouteRefusesPushedSDLCRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		CloseRun: func(project, runID string) error {
			return &runopen.NotClosableError{Reason: "sdlc alpha/fix-it is pushed — close the PR on GitHub and run `moe sync` to reconcile"}
		},
	})

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "moe sync") {
		t.Errorf("409 body should carry the steering message, got: %s", rr.Body.String())
	}
}

// TestCloseRouteRefusesLiveChild: a live PTY child means the agent is
// mid-turn; close refuses with 409 and never reaches the callback, and
// the registry entry survives.
func TestCloseRouteRefusesLiveChild(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	called := false
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		CloseRun: func(project, runID string) error {
			called = true
			return nil
		},
	})
	live := &child{id: "alpha/fix-it", started: time.Now(), done: make(chan struct{})} // done open → live
	s.children.all["alpha/fix-it"] = live

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if called {
		t.Errorf("CloseRun must not run while a live child exists")
	}
	if _, ok := s.children.get("alpha/fix-it"); !ok {
		t.Errorf("live child entry must survive a refused close")
	}
}

// TestCloseRouteCanvasEmptyMapsTo500: the canvas-empty gate (and any
// other non-state failure) comes back as a plain error from the
// callback, which the route maps to 500 — distinct from the 409 state
// refusals.
func TestCloseRouteCanvasEmptyMapsTo500(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		CloseRun: func(project, runID string) error {
			return errors.New("sdlc alpha/fix-it: canvas projects/alpha/runs/fix-it/documents/code/content.md is empty")
		},
	})

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "is empty") {
		t.Errorf("500 body should carry the canvas-empty message, got: %s", rr.Body.String())
	}
}

// TestCloseRouteSDLCWithoutCallbackIs500: an sdlc close on a server with
// no CloseRun wired can't proceed — 500 rather than a silent no-op
// redirect.
func TestCloseRouteSDLCWithoutCallbackIs500(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestIdeaPageHiddenActionsForPromotedIdea: once an idea has been
// promoted, its status is no longer in_progress and the actions block
// goes away — re-promoting or editing a hand-off idea would be a foot-
// gun (the destination run already owns the canvas).
func TestIdeaPageHiddenActionsForPromotedIdea(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	md := &run.Metadata{
		ID:        "old-idea",
		Project:   "alpha",
		Status:    run.StatusPromoted,
		Workflow:  "idea",
		Created:   "2026-04-01",
		Documents: map[string]*run.Document{},
	}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/old-idea", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, banned := range []string{
		`href="/run/alpha/old-idea/edit"`,
		`href="/run/alpha/old-idea/promote"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("promoted idea must not render actions (found %q)\n%s", banned, body)
		}
	}
}

// TestRunPageRendersDashRowMeta: when GatherRunRow returns a row, the
// per-run page renders RowNote and RowWhen instead of the older
// "started X · in_progress" line. The note carries whatever the dash
// would have shown ("sdlc:design @workspace" etc.) verbatim.
func TestRunPageRendersDashRowMeta(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	now := time.Now().UTC()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			if p != "alpha" || slug != "fix-it" {
				return dash.Row{}, false, nil
			}
			return dash.Row{
				Project: "alpha",
				Run:     "fix-it",
				Note:    "sdlc:design @workspace-name",
				When:    now.Add(-3 * time.Minute),
				Bucket:  dash.BucketActiveRuns,
			}, true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"sdlc:design @workspace-name",
		"3m ago",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// Fallback "started …" line must not render when a row is on hand.
	if strings.Contains(body, "started ") {
		t.Errorf("page rendered fallback meta despite row being available:\n%s", body)
	}
}

// TestRunPageFallbackMetaWhenNoRow: when GatherRunRow is unset (or
// returns ok=false), the per-run page falls back to the older
// "started Xm ago · status" line so it still renders something useful.
func TestRunPageFallbackMetaWhenNoRow(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "in_progress") {
		t.Errorf("fallback meta should carry run.Status, got:\n%s", body)
	}
}

// TestIdeaEditPageRendersBody: GET /run/{p}/{s}/edit seeds the textarea
// with the on-disk canvas body.
func TestIdeaEditPageRendersBody(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")
	writeCanvas(t, root, "alpha", "my-idea", "idea", "# my idea\n\nbody text\n")

	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea/edit", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<textarea`,
		`action="/run/alpha/my-idea/edit"`,
		"body text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

// TestIdeaEditPageRefusesNonIdea: GET on a non-idea returns 409, same
// shape as the promote page's gate.
func TestIdeaEditPageRefusesNonIdea(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it/edit", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestPromoteRefusesNonIdeaRun: POSTing to the promote URL of a
// non-idea run is the operator (or a stale form) calling the wrong
// surface. 409 with a clear body, no spawn.
func TestPromoteRefusesNonIdeaRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/alpha/fix-it/promote", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(s.children.all) != 0 {
		t.Errorf("no child should have been spawned for non-idea run; registry has %d", len(s.children.all))
	}
}

// TestPromoteRefusesMissingRun: a slug that doesn't exist on disk
// returns 404 from the load step.
func TestPromoteRefusesMissingRun(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/ghost/ghost/promote", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestDashRowsRenderAnchors: every row — active (live + resumable),
// backlog, completed — wraps its slug in an <a> pointing at the
// per-run page, so the operator can read any run's canvases without
// the row needing a separate "view" affordance.
func TestDashRowsRenderAnchors(t *testing.T) {
	now := time.Now().UTC()
	gather := func(showAll bool) ([]dash.Row, int, int, error) {
		return []dash.Row{
			{Project: "p", Run: "live-run", Note: "sdlc:code", Bucket: dash.BucketActiveRuns, When: now.Add(-time.Hour)},
			{Project: "p", Run: "done", Note: "sdlc:merged", Bucket: dash.BucketCompletedRuns, When: now.Add(-24 * time.Hour)},
			{Project: "p", Run: "later", Note: "idea:capture", Bucket: dash.BucketBacklog, When: now.Add(-1 * time.Hour)},
		}, 1, 1, nil
	}
	s := newTestServer(t, Options{
		Addr:       "127.0.0.1:0",
		Root:       t.TempDir(),
		GatherDash: gather,
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<a class="slug" href="/run/p/live-run">`,
		`<a class="slug" href="/run/p/done">`,
		`<a class="slug" href="/run/p/later">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestMakeNotifierPostsJSON(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
	}))
	defer srv.Close()

	notify := makeNotifier(srv.URL, io.Discard)
	notify("alpha/foo", nil)

	select {
	case body := <-gotBody:
		if !strings.Contains(string(body), `"id":"alpha/foo"`) {
			t.Errorf("payload missing id: %s", string(body))
		}
		if !strings.Contains(string(body), `"ok":true`) {
			t.Errorf("payload missing ok=true: %s", string(body))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier never POSTed")
	}
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = io.Discard
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// seedProject lays down projects/<id>/project.json with a placeholder
// remote so project.List picks it up — same shape as the project
// package's own test fixture, just inlined here.
func seedProject(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, "projects", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := project.Metadata{
		ID:            id,
		Remote:        "git@example.test:" + id + ".git",
		DefaultBranch: "main",
		Submodule:     "modules/" + id,
	}
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIdeaPageRendersCloseAndReopenActions(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`href="/run/alpha/my-idea/edit"`,
		`href="/run/alpha/my-idea/promote"`,
		`<form method="post" action="/run/alpha/my-idea/close"`,
		`>close idea</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "confirm(") || strings.Contains(body, "data-confirm") {
		t.Errorf("close action should not carry browser confirmation\n%s", body)
	}

	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusClosed
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	for _, want := range []string{
		`<form method="post" action="/run/alpha/my-idea/reopen"`,
		`>reopen idea</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	for _, banned := range []string{
		`href="/run/alpha/my-idea/edit"`,
		`href="/run/alpha/my-idea/promote"`,
		`action="/run/alpha/my-idea/close"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("closed idea page must not render %q\n%s", banned, body)
		}
	}
}

func TestIdeaCloseRouteClosesAndRedirects(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	gittest.Commit(t, root, "seed idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	req := httptest.NewRequest("POST", "/run/alpha/my-idea/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/my-idea" {
		t.Fatalf("Location=%q", got)
	}
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want closed", md.Status)
	}
}

func TestIdeaReopenRouteReopensAndRedirects(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusClosed
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "seed closed idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	req := httptest.NewRequest("POST", "/run/alpha/my-idea/reopen", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/my-idea" {
		t.Fatalf("Location=%q", got)
	}
	md, err = run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Fatalf("status=%q, want in_progress", md.Status)
	}
}

func TestIdeaCloseAndReopenRoutesReturnConflictForStalePosts(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusPromoted
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "seed promoted idea")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	for _, path := range []string{
		"/run/alpha/my-idea/close",
		"/run/alpha/my-idea/reopen",
	} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader("")))
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s: want 409, got %d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func newGitServeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}
