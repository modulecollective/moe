package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// TestSafeModeRefusesSpawnRoutes: in the production-default safe mode
// every spawn-bucket POST refuses with 403 and never spawns a child.
// The guard fires before any load, so the routes refuse even for runs
// that don't exist — exactly the point: no reachable path to code exec.
func TestSafeModeRefusesSpawnRoutes(t *testing.T) {
	for _, path := range []string{
		"/run/new",
		"/run/alpha/x/promote",
		"/run/alpha/x/advance",
		"/run/alpha/x/ship",
		"/run/alpha/x/chain",
		"/run/alpha/x/stage/code",
		"/chore/alpha/x/open",
	} {
		t.Run(path, func(t *testing.T) {
			s := newSafeTestServer(t, Options{
				Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/echo",
			})
			req := httptest.NewRequest("POST", path, strings.NewReader(""))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "safe mode") {
				t.Errorf("403 body should name safe mode, got:\n%s", rr.Body.String())
			}
			if len(s.children.all) != 0 {
				t.Errorf("safe-mode refusal must not spawn; registry has %d", len(s.children.all))
			}
		})
	}
}

// TestSafeModeAllowsIdeaCapture: the journal-write surface the operator
// actually uses stays open in safe mode — POST /idea/new opens a run and
// redirects, no flag required.
func TestSafeModeAllowsIdeaCapture(t *testing.T) {
	root := newGitServeRoot(t)
	seedProject(t, root, "alpha")
	gittest.Commit(t, root, "seed project")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

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

// TestSafeModeAllowsIdeaClose: closing a run is journal-only (no agent),
// so it works in safe mode too.
func TestSafeModeAllowsIdeaClose(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	gittest.Commit(t, root, "seed idea")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

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

// TestSafeModeDashHidesSpawnLinks: the dash drops the "new run" / "new
// plan" links (both spawn an agent) but keeps "new idea" — safe mode
// never offers a link the server would refuse.
func TestSafeModeDashHidesSpawnLinks(t *testing.T) {
	gather := func() ([]dash.Row, int, int, error) { return nil, 0, 0, nil }
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, banned := range []string{`href="/run/new"`, `href="/run/new?workflow=pdlc"`} {
		if strings.Contains(body, banned) {
			t.Errorf("safe-mode dash must not render %q\n%s", banned, body)
		}
	}
	if !strings.Contains(body, `href="/idea/new"`) {
		t.Errorf("safe-mode dash should still render the new-idea link\n%s", body)
	}
}

// TestSafeModeIdeaPageHidesPromote: an in-progress idea keeps its
// journal-only chips (edit, close) but drops promote, which spawns the
// destination run's agent.
func TestSafeModeIdeaPageHidesPromote(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "my-idea", "idea")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/my-idea", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`href="/run/alpha/my-idea/edit"`,
		`action="/run/alpha/my-idea/close"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("safe-mode idea page missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/run/alpha/my-idea/promote"`) {
		t.Errorf("safe-mode idea page must not render the promote chip\n%s", body)
	}
}

// TestSafeModeSDLCPageHidesSpawnChips: an in-progress sdlc run drops the
// advance/ship/chain trio (all spawn) but keeps the close chip (journal-
// only via the CloseRun callback).
func TestSafeModeSDLCPageHidesSpawnChips(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	now := time.Now().UTC()
	s := newSafeTestServer(t, Options{
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
	for _, banned := range []string{
		`/run/alpha/fix-it/advance`,
		`/run/alpha/fix-it/ship`,
		`/run/alpha/fix-it/chain`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("safe-mode sdlc page must not render %q\n%s", banned, body)
		}
	}
	if !strings.Contains(body, `/run/alpha/fix-it/close`) {
		t.Errorf("safe-mode sdlc page should still show the close chip\n%s", body)
	}
}

// TestSafeModeChorePageHidesOpen: a due chore renders no open affordance
// in safe mode — open spawns an agent. The schedule detail still shows.
func TestSafeModeChorePageHidesOpen(t *testing.T) {
	s := newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(),
		GatherChore: func(project, name string) (chore.State, bool, error) {
			return dueChoreState(), true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/readme-refresh", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, `action="/chore/alpha/readme-refresh/open"`) {
		t.Errorf("safe-mode chore page must not render the open form\n%s", body)
	}
	if !strings.Contains(body, "schedule") {
		t.Errorf("safe-mode chore page should still render the schedule detail\n%s", body)
	}
}
