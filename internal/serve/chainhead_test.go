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

// chainHeadServer wires a chain head at alpha/batch whose ChainMembers
// callback returns the given rows.
func chainHeadServer(t *testing.T, members []dash.Row) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "batch", dash.ChainWorkflow)
	return newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		ChainMembers: func(string, string) ([]dash.Row, error) {
			return members, nil
		},
	})
}

func getRunPage(t *testing.T, s *Server, path string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d body=%s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

var twoMembers = []dash.Row{
	{Project: "alpha", Run: "fix-one", Note: "sdlc:code", When: time.Now().Add(-2 * time.Hour)},
	{Project: "beta", Run: "fix-two", Note: "sdlc:design", When: time.Now().Add(-time.Hour)},
}

// TestChainHeadRendersLiveMembers: the head page shows the batch, in
// chain order, with each member's dash note. This is the page the
// dash's `parked · kick?` hint sends the operator to — before this it
// showed only the canvas link, and the canvas said nothing about
// membership at all for an operator-minted head.
//
// Cross-project members link correctly: a chain edit is global, so the
// second member's link must be /run/beta/..., not /run/alpha/....
func TestChainHeadRendersLiveMembers(t *testing.T) {
	s := chainHeadServer(t, twoMembers)
	body := getRunPage(t, s, "/run/alpha/batch")

	for _, want := range []string{
		`<h2>chained</h2>`,
		`href="/run/alpha/fix-one">alpha/fix-one</a>`,
		`sdlc:code`,
		`href="/run/beta/fix-two">beta/fix-two</a>`,
		`sdlc:design`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("head page missing %q\n%s", want, body)
		}
	}
	// Head→tail order, not whatever the map iteration gave.
	if i, j := strings.Index(body, "fix-one"), strings.Index(body, "fix-two"); i < 0 || j < 0 || i > j {
		t.Errorf("members should render head→tail: fix-one=%d fix-two=%d", i, j)
	}
}

// TestNonChainRunSkipsChainMembers: the callback is chain-only. Every
// other workflow's page must not pay a journal replay it has no use
// for — and must not sprout a members section.
func TestNonChainRunSkipsChainMembers(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	called := false
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		ChainMembers: func(string, string) ([]dash.Row, error) {
			called = true
			return twoMembers, nil
		},
	})
	body := getRunPage(t, s, "/run/alpha/fix-it")
	if called {
		t.Error("ChainMembers should only be consulted for chain heads")
	}
	if strings.Contains(body, "<h2>chained</h2>") {
		t.Errorf("sdlc run page should render no members section\n%s", body)
	}
}

// TestChainHeadOffersNoKick: a chain head has no stages to mark and no
// kick to click. Kicking one is a terminal act — a hand-staged head is a
// deliberate staging fence, and staging is where the operator already
// is — so the page's whole job is to show the batch `moe chain kick`
// would ride, honestly, and offer nothing.
func TestChainHeadOffersNoKick(t *testing.T) {
	s := chainHeadServer(t, twoMembers)
	body := getRunPage(t, s, "/run/alpha/batch")

	for _, banned := range []string{`/run/alpha/batch/kick`, ">kick<"} {
		if strings.Contains(body, banned) {
			t.Errorf("head page still offers %q\n%s", banned, body)
		}
	}
	// The members section is the point of the page and stays.
	if !strings.Contains(body, "<h2>chained</h2>") {
		t.Errorf("head page must still render the batch\n%s", body)
	}
}

// TestChainMembersErrorDegradesGracefully: a journal replay that fails
// must cost the members section, not the page. The canvas link and the
// meta line are still worth serving — same posture fillRunRow takes on
// a row-gather hiccup.
func TestChainMembersErrorDegradesGracefully(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "batch", dash.ChainWorkflow)
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		ChainMembers: func(string, string) ([]dash.Row, error) {
			return nil, errors.New("git log exploded")
		},
	})
	body := getRunPage(t, s, "/run/alpha/batch")
	if strings.Contains(body, "<h2>chained</h2>") {
		t.Errorf("failed gather should suppress the members section\n%s", body)
	}
}

// TestKickRouteIsGone: the web has no spawn routes at all now, so a
// forged or bookmarked POST at the old kick path falls through the mux
// rather than reaching a handler that would refuse it politely.
func TestKickRouteIsGone(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "batch", dash.ChainWorkflow)
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/alpha/batch/kick", strings.NewReader("")))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(s.children.all) != 0 {
		t.Errorf("nothing may spawn; registry has %d", len(s.children.all))
	}
}
