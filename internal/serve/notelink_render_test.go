package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// TestDashRendersLinkifiedNotes drives the linkified note through the
// real template, not just noteHTML in isolation: it proves html/template
// emits the pre-built anchor verbatim (Note is template.HTML) instead of
// re-escaping it, and that the qualified targets dash emits resolve as
// designed.
func TestDashRendersLinkifiedNotes(t *testing.T) {
	now := time.Now().UTC()
	gather := func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			// Qualified spawned target (what dash emits) → links as-is.
			{Project: "moe", Run: "head", Note: "sdlc:merged · spawned → moe/pulse-child",
				Bucket: dash.BucketCompletedRuns, When: now.Add(-time.Hour)},
			// Qualified chained target → links as-is.
			{Project: "alpha", Run: "worker", Note: "sdlc:code · chained → beta/child-run",
				Bucket: dash.BucketActiveRuns, When: now},
			// Free text with HTML metachars → escaped, no anchor.
			{Project: "moe", Run: "plain", Note: "pull: <b>rebalance</b> now",
				Bucket: dash.BucketActiveRuns, When: now.Add(-2 * time.Hour)},
		}, 1, 1, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		// Qualified target links as-is; anchor text is the qualified target.
		`spawned → <a href="/run/moe/pulse-child">moe/pulse-child</a>`,
		// Qualified target links as-is (not double-qualified).
		`chained → <a href="/run/beta/child-run">beta/child-run</a>`,
		// Free-text HTML metachars stay escaped — no raw <b> from note text.
		`pull: &lt;b&gt;rebalance&lt;/b&gt; now`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dash body missing %q\n%s", want, body)
		}
	}
	// The double-qualified href is the regression this guards against.
	if strings.Contains(body, "/run/moe/beta/child-run") {
		t.Errorf("qualified target must not be re-qualified with the row project\n%s", body)
	}
	// Note text must not smuggle a live <b> tag past the escaper.
	if strings.Contains(body, "<b>rebalance</b>") {
		t.Errorf("note metachars leaked as live HTML\n%s", body)
	}
}

// TestProjectHubRendersLinkifiedNotes drives the browse.go note path:
// a hub row's lineage note linkifies the same way as the home dash.
func TestProjectHubRendersLinkifiedNotes(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	gather := func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			{Project: "alpha", Run: "fix-1", Note: "sdlc:code · chained → alpha/downstream",
				Bucket: dash.BucketActiveRuns, When: time.Now()},
		}, 1, 1, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, GatherDash: gather})

	rr := get(t, s, "/projects/alpha")
	mustContain(t, rr, `chained → <a href="/run/alpha/downstream">alpha/downstream</a>`)
}

// TestRunPageRendersLinkifiedRowNote drives the fillRunRow path: the
// per-run fragment linkifies its dash-row note, and the {{if .RowNote}}
// guard still hides the span when the note is empty (empty template.HTML
// is falsey).
func TestRunPageRendersLinkifiedRowNote(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	now := time.Now().UTC()

	// Populated note → linkified in the fragment.
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "sdlc:merged · spawned → alpha/pulse-tail",
				Bucket: dash.BucketCompletedRuns, When: now}, true, nil
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if want := `spawned → <a href="/run/alpha/pulse-tail">alpha/pulse-tail</a>`; !strings.Contains(rr.Body.String(), want) {
		t.Errorf("run page missing linkified row note %q\n%s", want, rr.Body.String())
	}

	// Empty note → the {{if .RowNote}} span is hidden, the meta line
	// falls back to the status render.
	s2 := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "", Bucket: dash.BucketActiveRuns, When: now}, true, nil
		},
	})
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if strings.Contains(rr2.Body.String(), `<span class="note">`) {
		t.Errorf("empty row note must hide the note span\n%s", rr2.Body.String())
	}
}
