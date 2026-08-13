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

// panelServer is an armed serve with projects registered on disk (so the
// hub route resolves) and an activity record staged the way a live
// heartbeat would leave it: one project mid-sweep, one held by the gate,
// one with nothing to do, and a failed sweep in the ring carrying its
// output tail.
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
		{Project: "beta", Held: true, Reason: "somebody is already inside the project"},
		{Project: "gamma", Reason: "a sweep already surveyed the current tip"},
	})
	s.activity.recordSweepStart("alpha", now.Add(-3*time.Minute))
	s.activity.recordChildExit(heartbeatChildPrefix+"beta", now.Add(-40*time.Minute),
		errors.New("exit status 1"), "credit limit reached", "" /*run*/)
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
		clusterMarkup := `<div class="banner-serve"><a href="/serve">` + cluster + `</a></div>`
		if !strings.Contains(body, clusterMarkup) {
			t.Errorf("%s is missing the header cluster %q", path, cluster)
		}
		sub := strings.Index(body, `<div class="banner-sub">`)
		clusterPos := strings.Index(body, clusterMarkup)
		menu := strings.Index(body, `class="menu"`)
		if sub < 0 || clusterPos < 0 || menu < 0 || !(sub < clusterPos && clusterPos < menu) ||
			strings.Count(body[sub:clusterPos], `</div>`) != 2 {
			t.Errorf("%s header order is banner, serve cluster, menu", path)
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
// went — status, every project worth a verdict, the ring, and a failed
// child's output tail behind its details.
func TestServePageCarriesTheWholeTrace(t *testing.T) {
	body := getBody(t, panelServer(t), "/serve")
	for _, want := range []string{
		"serve-panel",
		"armed",
		"next sweep in",
		"last tick 3m ago",
		"sweeping",
		"the journal moved",
		"held",
		"somebody is already inside the project",
		"<summary>output</summary>",
		"credit limit reached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/serve is missing %q", want)
		}
	}
}

// TestServePageSummarisesTheTrivial: the page that owns the trace is
// also the page that has to stay readable. A project with nothing to do
// is a tally mark, not a row restating "nothing to do", and its reason
// doesn't reach the tick line either.
func TestServePageSummarisesTheTrivial(t *testing.T) {
	body := getBody(t, panelServer(t), "/serve")
	if !strings.Contains(body, "1 quiet") {
		t.Error("/serve should collapse the quiet projects to a count")
	}
	if strings.Contains(body, "a sweep already surveyed the current tip") {
		t.Error("/serve still spells out a quiet project's reason")
	}
	if strings.Contains(body, "gamma") {
		t.Error("/serve still names a project with nothing to report")
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

// TestServePageRendersAnAllQuietBoard: the near-idle case the design
// signed up for — "last tick 4m ago · 12 quiet" and little else. It must
// not read as the empty state, which means something different: no tick
// has ever run.
func TestServePageRendersAnAllQuietBoard(t *testing.T) {
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), Dynamic: true})
	s.activity.recordTick(time.Now().Add(-4*time.Minute), []HeartbeatDecision{
		{Project: "alpha", Reason: "a sweep already surveyed the current tip"},
		{Project: "beta", Reason: "no journal history yet"},
	})

	body := getBody(t, s, "/serve")
	if !strings.Contains(body, "last tick 4m ago") || !strings.Contains(body, "2 quiet") {
		t.Error("/serve should say the heartbeat is alive and how much of the board is idle")
	}
	if strings.Contains(body, "nothing yet — no tick has run") {
		t.Error("/serve read as never-ticked on a board that ticked and found nothing")
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

// TestServePanelLinksSweepsToTheirRuns: the panel's job here is to stop
// the operator leaving /serve to hunt the dash for a `pulse-*` slug whose
// date roughly matches. Both halves render as links — the project row's
// sweep, and a ring event whose subject already names a run.
func TestServePanelLinksSweepsToTheirRuns(t *testing.T) {
	s := panelServer(t)
	s.activity.recordSweepRun("alpha", "pulse-2026-04-01")
	s.activity.recordChildSpawn("alpha/fix-it", time.Now().Add(-2*time.Minute))

	body := getBody(t, s, "/serve")
	for _, want := range []string{
		`<a class="slug" href="/run/alpha/pulse-2026-04-01">pulse-2026-04-01</a>`,
		`<a class="slug" href="/run/alpha/fix-it">alpha/fix-it</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/serve is missing %s", want)
		}
	}
	// A heartbeat subject serve knows no run for stays plain text: the
	// ring is append-only and a spawn event is pushed before the run
	// exists, so there is nothing to backfill.
	if strings.Contains(body, `href="/run/beta/`) {
		t.Errorf("/serve linked a heartbeat subject with no run behind it")
	}
	if !strings.Contains(body, "heartbeat:beta exited") {
		t.Errorf("/serve lost the unlinked subject's own line")
	}
}
