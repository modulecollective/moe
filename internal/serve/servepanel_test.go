package serve

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
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

// postForm drives a POST route through the handler the way a browser
// would submit one of the page's own forms.
func postForm(t *testing.T, s *Server, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// modeServer is panelServer over a git-backed root, so the mode route's
// commit lands. Separate because the panel's read-only tests have no use
// for a repo and the init costs a fork.
func modeServer(t *testing.T) *Server {
	t.Helper()
	gittest.SetupEnv(t)
	s := panelServer(t)
	gittest.InitAt(t, s.opts.Root)
	gittest.Commit(t, s.opts.Root, "seed")
	gittest.Run(t, s.opts.Root, "add", "-A")
	gittest.Commit(t, s.opts.Root, "seed projects")
	return s
}

// TestProjectModeRouteSetsAndBadgesTheProject: the hub's switch is the
// whole of the web-side brake, and what it writes is the same file the
// gate reads.
func TestProjectModeRouteSetsAndBadgesTheProject(t *testing.T) {
	s := modeServer(t)
	if rec := postForm(t, s, "/projects/alpha/mode", "mode=safe"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /projects/alpha/mode = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	mode, err := project.ReadMode(s.opts.Root, "alpha")
	if err != nil || mode != project.ModeSafe {
		t.Fatalf("ReadMode after the click = %q, %v; want safe", mode, err)
	}
	// The hub renders the mode it now sits at as inert, and still offers
	// the other two — the switch shows the state as much as it sets it.
	body := getBody(t, s, "/projects/alpha")
	if !strings.Contains(body, `aria-disabled="true">safe<`) {
		t.Errorf("hub should render safe as the current, inert choice:\n%s", body)
	}
	for _, want := range []string{`value="paused"`, `value="auto"`} {
		if !strings.Contains(body, want) {
			t.Errorf("hub missing the %s submit:\n%s", want, body)
		}
	}
	// And back to auto, which is stored as absent rather than as a word.
	if rec := postForm(t, s, "/projects/alpha/mode", "mode=auto"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST mode=auto = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if mode, _ := project.ReadMode(s.opts.Root, "alpha"); mode != project.ModeAuto {
		t.Errorf("ReadMode after auto = %q, want auto", mode)
	}
}

// TestProjectModeRouteRejectsAnUnknownMode: a mode nobody recognises is
// exactly the case where guessing would arm the machine, so it 400s
// rather than normalizing.
func TestProjectModeRouteRejectsAnUnknownMode(t *testing.T) {
	s := modeServer(t)
	for _, form := range []string{"mode=", "mode=snooze", "mode=SAFE", "mode=off"} {
		if rec := postForm(t, s, "/projects/alpha/mode", form); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /projects/alpha/mode %q = %d, want 400", form, rec.Code)
		}
	}
	if mode, _ := project.ReadMode(s.opts.Root, "alpha"); mode != project.ModeAuto {
		t.Errorf("a rejected mode must not have been written: %q", mode)
	}
}

// TestProjectModeNeedsNoSpawnConsent: braking is not motion. The route
// writes config and spawns nothing, so an unarmed serve answers it like
// any other journal-write route rather than 403ing through spawnAllowed.
func TestProjectModeNeedsNoSpawnConsent(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed projects")
	s := newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return nil, 1, 0, nil, nil
		}})
	if rec := postForm(t, s, "/projects/alpha/mode", "mode=paused"); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST mode on an unarmed serve = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if mode, _ := project.ReadMode(root, "alpha"); mode != project.ModePaused {
		t.Errorf("ReadMode = %q, want paused", mode)
	}
}

// TestModeCountsRideTheHeaderCluster: the boards' whole spend on the
// modes is a count in the line they already carry, and only when
// something deviates.
func TestModeCountsRideTheHeaderCluster(t *testing.T) {
	s := modeServer(t)
	for _, path := range []string{"/", "/projects/alpha", "/serve"} {
		if body := getBody(t, s, path); strings.Contains(body, " paused") ||
			strings.Contains(body, "· 1 safe") {
			t.Errorf("an all-auto board must spend nothing on modes: %s\n%s", path, body)
		}
	}
	if rec := postForm(t, s, "/projects/alpha/mode", "mode=paused"); rec.Code != http.StatusSeeOther {
		t.Fatal(rec.Body.String())
	}
	if rec := postForm(t, s, "/projects/beta/mode", "mode=safe"); rec.Code != http.StatusSeeOther {
		t.Fatal(rec.Body.String())
	}
	for _, path := range []string{"/", "/projects/alpha", "/serve"} {
		body := getBody(t, s, path)
		if !strings.Contains(body, "1 paused · 1 safe") {
			t.Errorf("%s header missing the mode counts:\n%s", path, body)
		}
	}
}

// TestServePanelBadgesADeviantProject: a project that earns a row says
// what mode it is in; an auto one says nothing, so an all-auto board
// reads exactly as it did before modes existed.
func TestServePanelBadgesADeviantProject(t *testing.T) {
	s := modeServer(t)
	if body := getBody(t, s, "/serve"); strings.Contains(body, `class="badge mode"`) {
		t.Errorf("an all-auto board should carry no mode badge:\n%s", body)
	}
	// beta is the panel's held row, so it earns a line to badge.
	if rec := postForm(t, s, "/projects/beta/mode", "mode=safe"); rec.Code != http.StatusSeeOther {
		t.Fatal(rec.Body.String())
	}
	if body := getBody(t, s, "/serve"); !strings.Contains(body, `<span class="badge mode">safe</span>`) {
		t.Errorf("/serve should badge beta as safe:\n%s", body)
	}
}
