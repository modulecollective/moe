package serve

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// TestPromoteParksThenRunPageRidesIt drives the whole "decide later"
// loop the park-by-default design rests on: promote with the bare
// submit (no agent), follow the redirect, and start the agent from the
// destination page's own cascade chip.
//
// The unit tests around it cover the halves — the bare POST parks
// (TestSafeModePromoteParks), and the trio renders for a run at a
// declared stage (TestServeRunPageChipsComposeWithRealLookup, cli-side).
// Neither joins them, so nothing pinned the design's claim that
// parking leaves the ride one click away on the page it redirects to.
//
// The stage the trio keys off comes from GatherRunRow; the stub here
// returns what the real cli.GatherRunRow returns for a freshly parked
// sdlc run, which TestParkedRunRowResolvesToFirstStage (internal/cli)
// pins against the real gatherer.
func TestPromoteParksThenRunPageRidesIt(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
	seedIdeaRun(t, root, "alpha", "my-idea")
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "sdlc:design", Stage: "design",
				Bucket: dash.BucketActiveRuns, When: time.Now().UTC()}, true, nil
		},
	})

	form := url.Values{}
	form.Set("agent", "claude")
	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("park promote: want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(s.children.all) != 0 {
		t.Fatalf("the bare submit must not spawn; registry has %d", len(s.children.all))
	}
	dated := "my-idea-" + time.Now().Local().Format("2006-01-02")
	loc := rr.Header().Get("Location")
	if loc != "/run/alpha/"+dated {
		t.Fatalf("Location = %q, want /run/alpha/%s", loc, dated)
	}

	// The page the operator lands on must offer the ride.
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", loc, nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("destination page: want 200, got %d", rr2.Code)
	}
	body := rr2.Body.String()
	advance := "/run/alpha/" + dated + "/advance"
	if !strings.Contains(body, advance) || !strings.Contains(body, "→ design") {
		t.Fatalf("parked destination page must render the '→ design' chip\n%s", body)
	}

	// Ride it later: the chip starts the first-stage agent.
	rr3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr3, httptest.NewRequest("POST", advance, strings.NewReader("")))
	if rr3.Code != http.StatusSeeOther {
		t.Fatalf("advance: want 303, got %d body=%s", rr3.Code, rr3.Body.String())
	}
	if _, ok := s.children.get("alpha/" + dated); !ok {
		t.Fatal("the destination page's chip should have spawned the design agent")
	}
}

// TestSafeModeParkedPageOffersNoRide is the safe-mode half: parking
// still works, but the destination page withholds the trio. The row is
// wired to a real stage so the absence is attributable to safe mode
// rather than to a run with no next stage.
func TestSafeModeParkedPageOffersNoRide(t *testing.T) {
	root := seedBureaucracy(t, "alpha")
	seedIdeaRun(t, root, "alpha", "my-idea")
	s := newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "sdlc:design", Stage: "design",
				Bucket: dash.BucketActiveRuns, When: time.Now().UTC()}, true, nil
		},
	})

	form := url.Values{}
	form.Set("agent", "claude")
	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("safe-mode park: want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")

	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest("GET", loc, nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("destination page: want 200, got %d", rr2.Code)
	}
	body := rr2.Body.String()
	for _, banned := range []string{"/advance", "/ship", "/chain"} {
		if strings.Contains(body, loc+banned) {
			t.Errorf("safe-mode destination page must not render %q\n%s", banned, body)
		}
	}
}
