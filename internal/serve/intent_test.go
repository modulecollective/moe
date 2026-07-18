package serve

import (
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestIntentRowsLinkToRunPage: the INTENTS section on both the dash and
// the project hub renders its slug as an anchor into the run page —
// the entry point the whole read-and-edit loop hangs off. Before this,
// the slug was an inert span and intents were CLI-only from the browser.
func TestIntentRowsLinkToRunPage(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	gather := func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			{Project: "alpha", Run: "ship-faster", Bucket: dash.BucketIntents, Note: "ship faster", When: time.Now()},
		}, 0, 0, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, GatherDash: gather})

	for _, path := range []string{"/", "/projects/alpha"} {
		mustContain(t, get(t, s, path), `href="/run/alpha/ship-faster"`)
	}
}

// TestIntentPageRendersEditAndCloseChips: an in-progress intent gets
// the same journal-only chips an idea does. Promote stays idea-only
// even here, where the server runs insecure — an intent is never
// promoted.
func TestIntentPageRendersEditAndCloseChips(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/run/alpha/ship-faster")
	mustContain(t, rr,
		`href="/run/alpha/ship-faster/edit"`,
		`>edit intent<`,
		`action="/run/alpha/ship-faster/close"`,
		`>close intent<`,
	)
	if strings.Contains(rr.Body.String(), `/promote`) {
		t.Errorf("intent page must not offer promote\n%s", rr.Body.String())
	}
}

// TestClosedIntentPageHasNoChips: reopen is idea-only — `moe intent`
// has no reopen verb, and the web must not exceed the CLI's verb set.
func TestClosedIntentPageHasNoChips(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	setStatus(t, root, "alpha", "ship-faster", run.StatusClosed)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := get(t, s, "/run/alpha/ship-faster")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `<section class="actions">`) {
		t.Errorf("closed intent must render no action chips\n%s", rr.Body.String())
	}
}

// TestIntentEditPageRendersBodyAndKind: the shared edit page seeds the
// textarea from the intent's canvas and words its chrome for intents.
func TestIntentEditPageRendersBodyAndKind(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	writeCanvas(t, root, "alpha", "ship-faster", dash.IntentDocID, "# ship faster\n\nstanding direction\n")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	mustContain(t, get(t, s, "/run/alpha/ship-faster/edit"),
		`action="/run/alpha/ship-faster/edit"`,
		"standing direction",
		"edit intent",
	)
}

// TestIntentEditSubmitWritesCanvasAndRedirects: the POST round-trip —
// the operator's textarea lands on disk under the intent doc id and
// commits, then bounces back to the run page.
func TestIntentEditSubmitWritesCanvasAndRedirects(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	writeCanvas(t, root, "alpha", "ship-faster", dash.IntentDocID, "# ship faster\n")
	gittest.Commit(t, root, "seed intent")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	form := strings.NewReader("body=" + neturl.QueryEscape("# ship faster\r\n\r\nsharpened\r\n"))
	req := httptest.NewRequest("POST", "/run/alpha/ship-faster/edit", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/ship-faster" {
		t.Fatalf("Location=%q", got)
	}
	got, err := os.ReadFile(filepath.Join(root, run.ContentPath("alpha", "ship-faster", dash.IntentDocID)))
	if err != nil {
		t.Fatal(err)
	}
	// CRLF from the browser is normalised to LF, same as the idea path.
	if string(got) != "# ship faster\n\nsharpened\n" {
		t.Fatalf("canvas=%q", got)
	}
}

// TestIntentEditRefusesClosedIntent: a replayed POST landing on a
// retired intent is refused at the handler, not silently applied.
func TestIntentEditRefusesClosedIntent(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	setStatus(t, root, "alpha", "ship-faster", run.StatusClosed)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	if rr := get(t, s, "/run/alpha/ship-faster/edit"); rr.Code != http.StatusConflict {
		t.Errorf("GET edit: want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest("POST", "/run/alpha/ship-faster/edit", strings.NewReader("body=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("POST edit: want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestIntentCloseRouteClosesAndRedirects: the close chip's POST retires
// the intent in-process, no close registration needed — before this the
// same POST 409'd with "workflow intent has no close pipeline".
func TestIntentCloseRouteClosesAndRedirects(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	gittest.Commit(t, root, "seed intent")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	req := httptest.NewRequest("POST", "/run/alpha/ship-faster/close", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	md, err := run.Load(root, "alpha", "ship-faster")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want closed", md.Status)
	}
}

// TestSafeModeKeepsIntentEditAndClose: edit and close are journal-only,
// so they survive safe mode exactly as the idea pair does.
func TestSafeModeKeepsIntentEditAndClose(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "ship-faster", dash.IntentWorkflow)
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	mustContain(t, get(t, s, "/run/alpha/ship-faster"), `>edit intent<`, `>close intent<`)
}

func setStatus(t *testing.T, root, projectID, slug, status string) {
	t.Helper()
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		t.Fatal(err)
	}
	md.Status = status
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
}
