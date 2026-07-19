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
// callback returns the given rows and live-parent key.
func chainHeadServer(t *testing.T, members []dash.Row, chainedUnder string, insecure bool) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "batch", dash.ChainWorkflow)
	opts := Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		ChainMembers: func(string, string) ([]dash.Row, string, error) {
			return members, chainedUnder, nil
		},
	}
	if insecure {
		return newTestServer(t, opts)
	}
	return newSafeTestServer(t, opts)
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
	s := chainHeadServer(t, twoMembers, "", true)
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
		ChainMembers: func(string, string) ([]dash.Row, string, error) {
			called = true
			return twoMembers, "", nil
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

// TestChainHeadKickChipGating walks every reason the kick chip does or
// doesn't render. The chip is the first web surface for an action the
// dash has been naming since chain heads existed, so what it refuses
// matters as much as what it offers.
func TestChainHeadKickChipGating(t *testing.T) {
	for _, tc := range []struct {
		name         string
		members      []dash.Row
		chainedUnder string
		insecure     bool
		want         bool
	}{
		{name: "parked head with a batch", members: twoMembers, insecure: true, want: true},
		{name: "safe mode drops it", members: twoMembers, insecure: false, want: false},
		// `moe chain kick` would accept both of these; the page offers
		// neither. Chained under a live parent is the CLI's own "kick the
		// head" refusal. An empty head is what the dash calls `done ·
		// close?` — a chip labelled "kick" that silently closed a
		// placeholder would not be the action it names.
		{name: "chained under a live parent", members: twoMembers, chainedUnder: "alpha/topic", insecure: true, want: false},
		{name: "empty head", members: nil, insecure: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := chainHeadServer(t, tc.members, tc.chainedUnder, tc.insecure)
			body := getRunPage(t, s, "/run/alpha/batch")
			got := strings.Contains(body, `action="/run/alpha/batch/kick"`)
			if got != tc.want {
				t.Errorf("kick chip rendered=%v, want %v\n%s", got, tc.want, body)
			}
		})
	}
}

// TestChainMembersErrorDegradesGracefully: a journal replay that fails
// must cost the members section and the kick chip, not the page. The
// canvas link and the meta line are still worth serving — same posture
// fillRunRow takes on a row-gather hiccup.
func TestChainMembersErrorDegradesGracefully(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "batch", dash.ChainWorkflow)
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		ChainMembers: func(string, string) ([]dash.Row, string, error) {
			return nil, "", errors.New("git log exploded")
		},
	})
	body := getRunPage(t, s, "/run/alpha/batch")
	for _, unwanted := range []string{"<h2>chained</h2>", `action="/run/alpha/batch/kick"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("failed gather should suppress %q\n%s", unwanted, body)
		}
	}
}

// TestKickPOSTRefusesNonChainRun: the route is chain-only. A forged or
// stale POST at an sdlc run gets a 409, not a `moe chain kick` spawn
// that would refuse a beat later with a worse message.
func TestKickPOSTRefusesNonChainRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/alpha/fix-it/kick", strings.NewReader("")))
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(s.children.all) != 0 {
		t.Errorf("refusal must not spawn; registry has %d", len(s.children.all))
	}
}
