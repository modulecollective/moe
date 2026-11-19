package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// Unarmed is the production default, and after the spawn cull it is
// barely a distinction: nothing serve routes starts an agent, so every
// POST here works armed or not. What Dynamic gates is the heartbeat —
// unarmed, what the web wrote waits for the next hand-run sweep or
// interactive stage instead of the next tick.

// TestServeHasNoSpawnRoutes: the routes that used to run agent
// subprocesses are gone, not gated. A bookmark or a forged POST at any
// of them falls through the mux and spawns nothing, armed or not.
func TestServeHasNoSpawnRoutes(t *testing.T) {
	for _, path := range []string{
		"/run/new",
		"/run/alpha/x/promote",
		"/run/alpha/x/ship",
		"/run/alpha/x/chain",
		"/run/alpha/x/kick",
	} {
		t.Run(path, func(t *testing.T) {
			// Armed on purpose: the claim is that the route is absent, not
			// that a flag hides it.
			s := newTestServer(t, Options{
				Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/echo", Dynamic: true,
			})
			req := httptest.NewRequest("POST", path, strings.NewReader("spawn=1"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
			}
			if len(s.children.all) != 0 {
				t.Errorf("nothing may spawn; registry has %d", len(s.children.all))
			}
		})
	}
}

// TestUnarmedServeAllowsIdeaCapture: the journal-write surface the
// operator actually uses stays open on an unarmed serve — POST
// /idea/new opens a run and redirects, no flag required.
func TestUnarmedServeAllowsIdeaCapture(t *testing.T) {
	root := newGitServeRoot(t)
	seedProject(t, root, "alpha")
	gittest.Commit(t, root, "seed project")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	form := "id=alpha/new-idea&body=capture+this"
	req := httptest.NewRequest("POST", "/idea/new", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/new-idea" {
		t.Fatalf("Location=%q", got)
	}
	if _, err := run.Load(root, "alpha", "new-idea"); err != nil {
		t.Fatalf("run.Load after capture: %v", err)
	}
}

// TestUnarmedServeAllowsIdeaClose: closing a run is journal-only (no
// agent), so it works on an unarmed serve too.
func TestUnarmedServeAllowsIdeaClose(t *testing.T) {
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
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status=%q, want closed", md.Status)
	}
}

// TestUnarmedServeAllowsTheAdvanceMark is the one that matters most: the
// mark is the phone's approval verb, and without it a project the
// operator has braked has no web path to motion at all. It writes a
// journal commit and starts nothing, so being unarmed can't be a reason
// to refuse it.
func TestUnarmedServeAllowsTheAdvanceMark(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.Commit(t, root, "seed run")
	trailerstest.CommitWorkTurnAt(t, root, "alpha", "fix-it", "sdlc", "design",
		time.Now().Add(-time.Hour))
	now := time.Now().UTC()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "sdlc:design", Stage: "design",
				Bucket: dash.BucketActiveRuns, When: now}, true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/alpha/fix-it/advance", strings.NewReader("")))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	sha, _, err := run.LatestAdvanceSHA(root, "alpha", "fix-it", "design")
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Error("the mark should have landed a marker commit")
	}
	if len(s.children.all) != 0 {
		t.Errorf("a mark must not spawn; registry has %d", len(s.children.all))
	}
}

// TestUnarmedServeDashKeepsTheIdeaLink: the idea door is journal-only,
// so it survives being unarmed. Its intent sibling is pinned alongside
// it in TestDashRendersTheIntentCaptureLink.
func TestUnarmedServeDashKeepsTheIdeaLink(t *testing.T) {
	gather := func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/idea/new"`) {
		t.Errorf("unarmed dash should render the idea capture link\n%s", body)
	}
	if strings.Contains(body, `href="/run/new"`) {
		t.Errorf("the new-run door is gone; every web-initiated piece of work is an idea first\n%s", body)
	}
}

// TestUnarmedServeIdeaPageShowsTagChips: an in-progress idea keeps its
// journal-only chips. Tagging is the licence a sweep spends, which is
// what replaced the promote page — so the chips must be there whether or
// not this process is the one that will spend it.
func TestUnarmedServeIdeaPageShowsTagChips(t *testing.T) {
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
		`/run/alpha/my-idea/tag?workflow=sdlc`,
		`action="/run/alpha/my-idea/close"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unarmed idea page missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "/run/alpha/my-idea/promote") {
		t.Errorf("promotion is the sweep's move now, not a button's\n%s", body)
	}
}

// TestUnarmedServeSDLCPageOffersTheMarkAndClose: an in-progress sdlc run
// parked at a worked stage renders the advance mark and the close chip,
// and nothing that spawns.
func TestUnarmedServeSDLCPageOffersTheMarkAndClose(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.Commit(t, root, "seed run")
	trailerstest.CommitWorkTurnAt(t, root, "alpha", "fix-it", "sdlc", "code",
		time.Now().Add(-time.Hour))
	now := time.Now().UTC()
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root,
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return dash.Row{Project: p, Run: slug, Note: "sdlc:code", Stage: "code",
				Bucket: dash.BucketActiveRuns, When: now}, true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/fix-it", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`action="/run/alpha/fix-it/advance"`,
		`/run/alpha/fix-it/close`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unarmed sdlc page missing %q\n%s", want, body)
		}
	}
	for _, banned := range []string{`/run/alpha/fix-it/ship`, `/run/alpha/fix-it/chain`} {
		if strings.Contains(body, banned) {
			t.Errorf("unarmed sdlc page must not render %q\n%s", banned, body)
		}
	}
}

// TestChoreOpenSpawnsNothing: "open now" writes the open commit through
// the callback and redirects to the run. Armed on purpose: the claim is
// that the route itself starts no agent, not that a flag hides it — an
// armed heartbeat rides the commit on its own next tick.
func TestChoreOpenSpawnsNothing(t *testing.T) {
	calls := 0
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/echo", Dynamic: true,
		OpenChore: func(project, name string) (string, error) {
			calls++
			return name + "-1", nil
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/chore/alpha/readme-refresh/open", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/run/alpha/readme-refresh-1" {
		t.Fatalf("got %d Location=%q body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	if calls != 1 {
		t.Fatalf("OpenChore called %d times", calls)
	}
	if len(s.children.all) != 0 {
		t.Errorf("nothing may spawn; registry has %d", len(s.children.all))
	}
}
