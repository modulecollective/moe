package serve

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
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

func TestResolveAddrOverrideWithoutPort(t *testing.T) {
	got, err := resolveAddr("127.0.0.1", 4242)
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:4242" {
		t.Errorf("got %s, want 127.0.0.1:4242", got)
	}
}

func TestResolveAddrOverrideWithPort(t *testing.T) {
	got, err := resolveAddr("127.0.0.1:9999", 4242)
	if err != nil {
		t.Fatal(err)
	}
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
