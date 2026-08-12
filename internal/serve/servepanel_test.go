package serve

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// panelServer is an armed serve with a project registered on disk (so the
// hub route resolves) and an activity record staged the way a live
// heartbeat would leave it: one project mid-sweep, one quiet, and a failed
// sweep in the ring carrying its output tail.
func panelServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	seedRun(t, root, "beta", "other", "sdlc")
	now := time.Now()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return nil, 2, 0, nil, nil
		},
	})
	s.activity.recordTick(now.Add(-3*time.Minute), []HeartbeatDecision{
		{Project: "alpha", Sweep: true, Reason: "the journal moved"},
		{Project: "beta", Reason: "a sweep already surveyed the current tip"},
	})
	s.activity.recordSweepStart("alpha", now.Add(-3*time.Minute))
	s.activity.recordChildExit(heartbeatChildPrefix+"beta", now.Add(-40*time.Minute),
		errors.New("exit status 1"), "credit limit reached")
	return s
}

func getBody(t *testing.T, s *Server, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	return rec.Body.String()
}

// TestServeClusterRidesTheBoardHeaders: the boards spend one muted line
// on serve — the same cluster the CLI banner carries — and link it to the
// page that owns the trace. A day of ticks on every dash load was the
// noise this change exists to stop.
func TestServeClusterRidesTheBoardHeaders(t *testing.T) {
	s := panelServer(t)
	for _, path := range []string{"/", "/projects/alpha"} {
		body := getBody(t, s, path)
		cluster := s.activity.panel(time.Now()).Cluster
		if !strings.Contains(body, `<div class="banner-serve"><a href="/serve">`+cluster+`</a></div>`) {
			t.Errorf("%s is missing the header cluster %q", path, cluster)
		}
		for _, unwanted := range []string{"serve-panel", "the journal moved", "credit limit reached"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s still carries %q — the trace belongs on /serve", path, unwanted)
			}
		}
	}
}

// TestServeClusterRendersUnarmed: "it's up but will never pulse" is
// exactly the confusion this exists to fix, so an unarmed serve says so
// in the header rather than going quiet.
func TestServeClusterRendersUnarmed(t *testing.T) {
	s := newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return nil, 0, 0, nil, nil
		},
	})
	body := getBody(t, s, "/")
	if !strings.Contains(body, "serve browse-only · up") {
		t.Error("an unarmed serve should say so in the dash header")
	}
	if strings.Contains(body, "· next ") {
		t.Error("an unarmed serve promised a sweep it will never run")
	}
}

// TestServeMenuReachesTheServePage: the cluster only renders on the two
// boards, and the menu is on every page — so the menu is what makes the
// trace reachable from anywhere.
func TestServeMenuReachesTheServePage(t *testing.T) {
	body := getBody(t, panelServer(t), "/lore")
	if !strings.Contains(body, `<a href="/serve">serve</a>`) {
		t.Error("the hamburger menu is missing its serve entry")
	}
}

// TestServePageCarriesTheWholeTrace: /serve is where the former panel
// went — status, every project the heartbeat has a verdict for, the ring,
// and a failed child's output tail behind its details.
func TestServePageCarriesTheWholeTrace(t *testing.T) {
	body := getBody(t, panelServer(t), "/serve")
	for _, want := range []string{
		"serve-panel",
		"armed",
		"next sweep in",
		"sweeping",
		"the journal moved",
		"a sweep already surveyed the current tip",
		"<summary>output</summary>",
		"credit limit reached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/serve is missing %q", want)
		}
	}
}

// TestServePageIsBoardWide: the hub's scoped trace is gone, so a project
// that only shows up in another project's tick still has to be findable
// here.
func TestServePageIsBoardWide(t *testing.T) {
	body := getBody(t, panelServer(t), "/serve")
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(body, want) {
			t.Errorf("/serve is missing project %q", want)
		}
	}
}

// TestServePageEmptyState: a serve that has never ticked renders a page
// that says so rather than a blank one.
func TestServePageEmptyState(t *testing.T) {
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	if body := getBody(t, s, "/serve"); !strings.Contains(body, "nothing yet — no tick has run") {
		t.Error("/serve should name its empty state")
	}
}

// TestServePageDoesNotReadTheStateFile: serve holds the record, so a round
// trip through its own snapshot would add a beat of lag for nothing. Proven
// by rendering with no file on disk at all.
func TestServePageDoesNotReadTheStateFile(t *testing.T) {
	s := panelServer(t)
	if _, ok, _ := ReadActivitySnapshot(s.opts.Root); ok {
		t.Fatal("fixture wrote a state file; this test needs none")
	}
	if body := getBody(t, s, "/serve"); !strings.Contains(body, "the journal moved") {
		t.Error("the page rendered nothing without a state file — it should render from memory")
	}
}
