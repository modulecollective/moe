package serve

import (
	"encoding/json"
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
	"github.com/modulecollective/moe/internal/project"
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

func TestDetectPromptHappyPath(t *testing.T) {
	tail := []byte("blah blah\nnext: moe sdlc design alpha/foo — run now? [Y/n/x/b/!]\n  Y = run · n = decline · x = scuttle (close) · b = back to stage · ! = cascade one stage\n  !<stage> = cascade to gate · !! = cascade and ship\n")
	got := detectPrompt(tail)
	if !got.Active {
		t.Fatalf("expected active prompt, got %+v", got)
	}
	if got.Options != "Ynxb!" {
		t.Errorf("Options = %q, want Ynxb!", got.Options)
	}
}

func TestDetectPromptClosePrompt(t *testing.T) {
	// The close-prompt phrasing printed at stage_next.go:444.
	tail := []byte("design sealed — close run now? [Y/n/x]\n  Y = close · n = decline · x = close (alias)\n")
	got := detectPrompt(tail)
	if !got.Active {
		t.Fatalf("expected active prompt, got %+v", got)
	}
	if got.Options != "Ynx" {
		t.Errorf("Options = %q, want Ynx", got.Options)
	}
}

func TestDetectPromptStaleMatch(t *testing.T) {
	prompt := "next: moe sdlc design alpha/foo — run now? [Y/n/!]\n"
	// Pad with > promptWindow bytes of post-prompt progress so the
	// match falls outside the live window.
	tail := []byte(prompt + strings.Repeat("progress\n", 200))
	got := detectPrompt(tail)
	if got.Active {
		t.Errorf("prompt is stale, expected !Active; got %+v", got)
	}
}

func TestDetectPromptNoMatch(t *testing.T) {
	got := detectPrompt([]byte("just some normal output\n"))
	if got.Active {
		t.Errorf("expected !Active, got %+v", got)
	}
}

func TestKeyAllowed(t *testing.T) {
	cases := []struct {
		key, opts string
		want      bool
	}{
		{"Y", "Yn!", true},
		{"n", "Yn!", true},
		{"x", "Yn!", false},
		{"!", "Yn!", true},
		{"!!", "Yn!", true}, // !! permitted when ! is in options
		{"!!", "Yn", false}, // and refused when it isn't
		{"YY", "Yn!", false},
		{"", "Yn!", false},
	}
	for _, c := range cases {
		got := keyAllowed(c.key, c.opts)
		if got != c.want {
			t.Errorf("keyAllowed(%q, %q) = %v, want %v", c.key, c.opts, got, c.want)
		}
	}
}

func TestButtonsForOrdering(t *testing.T) {
	got := buttonsFor("Yn!xb")
	want := []string{"Y", "n", "!", "!!", "x", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d buttons, want %d: %+v", len(got), len(want), got)
	}
	for i, b := range got {
		if b.Key != want[i] {
			t.Errorf("button[%d] = %q, want %q", i, b.Key, want[i])
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
