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
//
// /run/new and /promote are dual-submit: the bare submit parks (no
// spawn, allowed in safe mode) and only `spawn=1` reaches the gate, so
// those two carry the form body that asks for the agent.
func TestSafeModeRefusesSpawnRoutes(t *testing.T) {
	for path, body := range map[string]string{
		"/run/new":                  "spawn=1",
		"/run/alpha/x/promote":      "spawn=1",
		"/run/alpha/x/advance":      "",
		"/run/alpha/x/ship":         "",
		"/run/alpha/x/chain":        "",
		"/run/alpha/x/kick":         "",
		"/run/alpha/x/kick-dynamic": "",
		"/chore/alpha/x/open":       "",
	} {
		t.Run(path, func(t *testing.T) {
			s := newSafeTestServer(t, Options{
				Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/echo",
			})
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
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

// TestSafeModeDashKeepsNewLinks: both dash "new" links survive safe
// mode. Capturing an idea was always journal-only; opening a run is
// too now that the new-run form's bare submit parks. The spawning
// submit on the form itself is what safe mode hides.
func TestSafeModeDashKeepsNewLinks(t *testing.T) {
	gather := func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`href="/run/new"`, `href="/idea/new"`} {
		if !strings.Contains(body, want) {
			t.Errorf("safe-mode dash should render %q\n%s", want, body)
		}
	}
}

// TestSafeModeIdeaPageShowsPromote: an in-progress idea keeps all three
// journal-only chips. Promote parks the destination run by default, so
// it's the same class as edit and close; only the promote page's
// "promote & run" submit is gated.
func TestSafeModeIdeaPageShowsPromote(t *testing.T) {
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
		`href="/run/alpha/my-idea/promote"`,
		`action="/run/alpha/my-idea/close"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("safe-mode idea page missing %q\n%s", want, body)
		}
	}
}

// TestSafeModeFormsHideSpawnButton: the promote and new-run forms each
// render their parking submit in safe mode and drop the spawning one —
// safe mode never offers a button the POST handler would refuse.
func TestSafeModeFormsHideSpawnButton(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	gittest.Commit(t, root, "seed idea")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})

	for _, tc := range []struct{ path, park string }{
		{"/run/alpha/my-idea/promote", ">promote<"},
		{"/run/new", ">open run<"},
	} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d body=%s", tc.path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, tc.park) {
			t.Errorf("GET %s: missing the parking submit %q\n%s", tc.path, tc.park, body)
		}
		if strings.Contains(body, `name="spawn"`) {
			t.Errorf("GET %s: safe mode must not render the spawning submit\n%s", tc.path, body)
		}
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

// TestSafeModePromoteParks: the bare promote submit is journal-only —
// it opens the destination run queued at its first stage, marks the
// idea promoted, and redirects, all without the --insecure flag. This
// is the operator's usual move; riding the run is the rarer one.
func TestSafeModePromoteParks(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	writeCanvas(t, root, "alpha", "my-idea", "idea", "park me\n")
	gittest.Commit(t, root, "seed idea")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	dated := "my-idea-" + time.Now().Local().Format("2006-01-02")
	if got := rr.Header().Get("Location"); got != "/run/alpha/"+dated {
		t.Fatalf("Location = %q, want /run/alpha/%s", got, dated)
	}
	dest, err := run.Load(root, "alpha", dated)
	if err != nil {
		t.Fatalf("destination run.Load: %v", err)
	}
	if dest.Status != run.StatusInProgress {
		t.Errorf("destination status = %q, want in-progress", dest.Status)
	}
	src, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != run.StatusPromoted {
		t.Errorf("source idea status = %q, want promoted", src.Status)
	}
	if len(s.children.all) != 0 {
		t.Errorf("parking must not spawn; registry has %d", len(s.children.all))
	}
}

// TestSafeModePromoteSpawnRefusesBeforePromote pins the handler's
// ordering: the spawn gate fires before runopen.Promote, so a refused
// "promote & run" click leaves the idea untouched. Gating after the
// open would half-promote — destination run on disk, idea marked, no
// agent — from a click safe mode was supposed to refuse outright.
func TestSafeModePromoteSpawnRefusesBeforePromote(t *testing.T) {
	root := newGitServeRoot(t)
	seedRun(t, root, "alpha", "my-idea", "idea")
	writeCanvas(t, root, "alpha", "my-idea", "idea", "park me\n")
	gittest.Commit(t, root, "seed idea")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/alpha/my-idea/promote", strings.NewReader("spawn=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	dated := "my-idea-" + time.Now().Local().Format("2006-01-02")
	if _, err := run.Load(root, "alpha", dated); err == nil {
		t.Error("refused promote must not open the destination run")
	}
	src, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if src.Status != run.StatusInProgress {
		t.Errorf("source idea status = %q, want in-progress (untouched)", src.Status)
	}
	if len(s.children.all) != 0 {
		t.Errorf("refusal must not spawn; registry has %d", len(s.children.all))
	}
}

// TestSafeModeNewRunParks: the new-run form's bare submit opens a run
// with no agent, the same journal-only write promote's does.
func TestSafeModeNewRunParks(t *testing.T) {
	root := newGitServeRoot(t)
	seedProject(t, root, "alpha")
	gittest.Commit(t, root, "seed project")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/new", strings.NewReader("id=alpha/first-thing"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/first-thing" {
		t.Fatalf("Location = %q", got)
	}
	if _, err := run.Load(root, "alpha", "first-thing"); err != nil {
		t.Fatalf("run.Load after park: %v", err)
	}
	if len(s.children.all) != 0 {
		t.Errorf("parking must not spawn; registry has %d", len(s.children.all))
	}
}

// TestSafeModeNewRunSpawnRefusesBeforeOpen: the /run/new mirror of the
// ordering probe above — a refused "open & run" leaves no run on disk.
func TestSafeModeNewRunSpawnRefusesBeforeOpen(t *testing.T) {
	root := newGitServeRoot(t)
	seedProject(t, root, "alpha")
	gittest.Commit(t, root, "seed project")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})

	req := httptest.NewRequest("POST", "/run/new", strings.NewReader("id=alpha/first-thing&spawn=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := run.Load(root, "alpha", "first-thing"); err == nil {
		t.Error("refused open must not leave a run on disk")
	}
	if len(s.children.all) != 0 {
		t.Errorf("refusal must not spawn; registry has %d", len(s.children.all))
	}
}
