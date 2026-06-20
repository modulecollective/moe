package serve

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
)

// dueChoreState is a minimal openable chore.State for the render/open
// happy paths.
func dueChoreState() chore.State {
	return chore.State{
		Definition: chore.Definition{
			Project:  "alpha",
			Name:     "readme-refresh",
			Trigger:  "README.md",
			Workflow: "sdlc",
			Cadence:  24 * time.Hour,
			Prompt:   "# refresh the readme\n",
		},
		Due:     true,
		Reasons: []string{"changed paths"},
	}
}

// TestChorePageRendersDefinitionAndOpenAffordance: GET on a due chore
// renders the definition (workflow, trigger, prompt) and a live open
// button posting to the open route.
func TestChorePageRendersDefinitionAndOpenAffordance(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherChore: func(project, name string) (chore.State, bool, error) {
			if project != "alpha" || name != "readme-refresh" {
				return chore.State{}, false, nil
			}
			return dueChoreState(), true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/readme-refresh", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"alpha/readme-refresh",
		"workflow sdlc",
		"README.md",
		"refresh the readme",
		`<form method="post" action="/chore/alpha/readme-refresh/open"`,
		`>open</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	assertThemeToggleInHeader(t, body)
}

// TestChorePageRendersDisabledOpenWhenBlocked: a chore with an open run
// is not openable — the page shows the disabled state with the block
// reason and a link to the open run, not a live open button.
func TestChorePageRendersDisabledOpenWhenBlocked(t *testing.T) {
	st := dueChoreState()
	st.Due = false
	st.OpenRun = "readme-refresh-2026-05-20"
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherChore: func(project, name string) (chore.State, bool, error) {
			return st, true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/readme-refresh", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"open run readme-refresh-2026-05-20",
		`href="/run/alpha/readme-refresh-2026-05-20"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `<button class="action" type="submit">open</button>`) {
		t.Errorf("blocked chore must not render a live open button\n%s", body)
	}
}

// TestChorePageMissingChore404: GatherChore ok=false → 404, not an empty
// render.
func TestChorePageMissingChore404(t *testing.T) {
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherChore: func(project, name string) (chore.State, bool, error) {
			return chore.State{}, false, nil
		},
	})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/ghost", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such chore") {
		t.Errorf("body should say 'no such chore', got:\n%s", rr.Body.String())
	}
}

// TestChorePageWithoutCallback500: a server with no GatherChore wired
// can't render the page — 500.
func TestChorePageWithoutCallback500(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir()})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/readme-refresh", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestChoreOpenSpawnsAndRedirects: POST /open on an openable chore runs
// OpenChore, spawns the configured workflow's first stage as a PTY child,
// and redirects to the new run's page.
func TestChoreOpenSpawnsAndRedirects(t *testing.T) {
	var gotProject, gotName string
	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   t.TempDir(),
		MoeBin: "/bin/echo", // spawn something harmless
		OpenChore: func(project, name string) (ChoreOpen, error) {
			gotProject, gotName = project, name
			return ChoreOpen{
				Project:    "alpha",
				Slug:       "readme-refresh-2026-05-29",
				Workflow:   "sdlc",
				FirstStage: "design",
			}, nil
		},
	})

	req := httptest.NewRequest("POST", "/chore/alpha/readme-refresh/open", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/run/alpha/readme-refresh-2026-05-29" {
		t.Fatalf("Location=%q", got)
	}
	if gotProject != "alpha" || gotName != "readme-refresh" {
		t.Fatalf("OpenChore called with project=%q name=%q", gotProject, gotName)
	}
	if _, ok := s.children.get("alpha/readme-refresh-2026-05-29"); !ok {
		t.Errorf("open should have spawned a child for the dest run")
	}
}

// TestChoreOpenMissingChore404: OpenChore wrapping ErrChoreNotFound maps
// to 404, no spawn.
func TestChoreOpenMissingChore404(t *testing.T) {
	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   t.TempDir(),
		MoeBin: "/bin/echo",
		OpenChore: func(project, name string) (ChoreOpen, error) {
			return ChoreOpen{}, fmt.Errorf("%w: chore open: alpha/ghost not found", ErrChoreNotFound)
		},
	})
	req := httptest.NewRequest("POST", "/chore/alpha/ghost/open", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(s.children.all) != 0 {
		t.Errorf("404 open must not spawn; registry has %d", len(s.children.all))
	}
}

// TestChoreOpenNotOpenable409RerendersBanner: OpenChore wrapping
// ErrChoreNotOpenable (a raced/stale open) maps to 409 and re-renders the
// chore page with an inline error banner reflecting the current block
// reason.
func TestChoreOpenNotOpenable409RerendersBanner(t *testing.T) {
	st := dueChoreState()
	st.Due = false
	st.OpenRun = "readme-refresh-2026-05-20"
	s := newTestServer(t, Options{
		Addr:   "127.0.0.1:0",
		Root:   t.TempDir(),
		MoeBin: "/bin/echo",
		GatherChore: func(project, name string) (chore.State, bool, error) {
			return st, true, nil
		},
		OpenChore: func(project, name string) (ChoreOpen, error) {
			return ChoreOpen{}, fmt.Errorf("%w: chore open: alpha/readme-refresh already has open run alpha/readme-refresh-2026-05-20", ErrChoreNotOpenable)
		},
	})
	req := httptest.NewRequest("POST", "/chore/alpha/readme-refresh/open", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "open failed") || !strings.Contains(body, "open run readme-refresh-2026-05-20") {
		t.Errorf("409 body should re-render the chore page with a block-reason banner, got:\n%s", body)
	}
	if len(s.children.all) != 0 {
		t.Errorf("409 open must not spawn; registry has %d", len(s.children.all))
	}
}

// TestChoreOpenWithoutCallback500: no OpenChore wired → 500.
func TestChoreOpenWithoutCallback500(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/echo"})
	req := httptest.NewRequest("POST", "/chore/alpha/readme-refresh/open", strings.NewReader(""))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestNewChoreVMBlockReasonPrecedence: an open run beats a cooldown beats
// plain not-due, matching the dash/CLI precedence.
func TestNewChoreVMBlockReasonPrecedence(t *testing.T) {
	now := time.Now()
	next := now.Add(2 * time.Hour)
	cases := []struct {
		name string
		mut  func(*chore.State)
		want string
	}{
		{"open-run", func(st *chore.State) { st.OpenRun = "r1" }, "open run r1"},
		{"cooldown", func(st *chore.State) { st.CooldownBlocking = true; st.NextEligible = next }, "cooling down until "},
		{"not-due", func(st *chore.State) {}, "not due"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := dueChoreState()
			st.Due = false
			tc.mut(&st)
			vm := newChoreVM(now, st, false)
			if vm.Openable {
				t.Fatalf("not-due chore must not be openable")
			}
			if !strings.HasPrefix(vm.BlockReason, tc.want) {
				t.Errorf("BlockReason=%q, want prefix %q", vm.BlockReason, tc.want)
			}
		})
	}
}

// TestDashChoreRowsLinkToChorePage: the dash CHORES rows wrap their slug
// in an <a> pointing at the chore detail page (the dead-text fix).
func TestDashChoreRowsLinkToChorePage(t *testing.T) {
	now := time.Now().UTC()
	gather := func() ([]dash.Row, int, int, []int, error) {
		return []dash.Row{
			{Project: "alpha", Run: "readme-refresh", Note: "chore", Bucket: dash.BucketChores, When: now},
		}, 1, 1, nil, nil
	}
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), GatherDash: gather})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if want := `<a class="slug" href="/chore/alpha/readme-refresh">`; !strings.Contains(rr.Body.String(), want) {
		t.Errorf("dash chore row should link to chore page (%q)\n%s", want, rr.Body.String())
	}
}

// guard: ErrChoreNotFound and ErrChoreNotOpenable are distinct so the
// route can branch 404 vs 409.
func TestChoreSentinelsDistinct(t *testing.T) {
	if errors.Is(ErrChoreNotFound, ErrChoreNotOpenable) || errors.Is(ErrChoreNotOpenable, ErrChoreNotFound) {
		t.Fatal("chore sentinels must be distinct")
	}
}
