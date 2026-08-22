package serve

import (
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

// TestChorePageRendersDefinition: GET on a due chore renders the
// definition (workflow, trigger, prompt) and the due badge — and no
// open button, because opening a due chore is the heartbeat's job now.
func TestChorePageRendersDefinition(t *testing.T) {
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
		`<span class="badge live">due</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "/chore/alpha/readme-refresh/open") {
		t.Errorf("the chore page must offer no open route\n%s", body)
	}
	assertSharedHead(t, body)
}

// TestChorePageNamesWhatAChoreIsWaitingOn: a chore with an open run
// isn't going to fire — the page says so, and links to the run that is
// already carrying it.
func TestChorePageNamesWhatAChoreIsWaitingOn(t *testing.T) {
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
		"waiting — open run readme-refresh-2026-05-20",
		`href="/run/alpha/readme-refresh-2026-05-20"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
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

// TestNewChoreVMWaitingPrecedence: an open run beats a cooldown beats
// plain not-due, matching the dash/CLI precedence.
func TestNewChoreVMWaitingPrecedence(t *testing.T) {
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
			vm := newChoreVM(now, st)
			if !strings.HasPrefix(vm.Waiting, tc.want) {
				t.Errorf("Waiting=%q, want prefix %q", vm.Waiting, tc.want)
			}
		})
	}
}

// TestDashChoreRowsLinkToChorePage: a chore row wraps its slug in an <a>
// pointing at the chore detail page (the dead-text fix) — including now
// that the row renders inside the backlog section beside idea rows,
// which link at /run/.
func TestDashChoreRowsLinkToChorePage(t *testing.T) {
	now := time.Now().UTC()
	gather := func(string) ([]dash.Row, int, int, []int, error) {
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

// The chores page already used the friendly zone-marked format but fed
// it a UTC instant, so it honestly printed "UTC" at an operator who
// wanted the box's clock. The waiting line reuses vm.NextEligible, so
// both move together.
//
// time.Local is swapped rather than TZ set: the runtime resolves TZ
// once, long before a test runs. Safe here because this test is
// sequential and the package's only parallel file (process_linux_test)
// resumes after the sequential tests finish.
func TestChorePageNextEligibleIsLocal(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	// An implausible fractional offset, so a UTC render can't pass by
	// coincidence, and a zone name no tzdata zone carries.
	fz := time.FixedZone("MOETEST", 7*3600+1800)
	time.Local = fz

	next := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherChore: func(project, name string) (chore.State, bool, error) {
			st := dueChoreState()
			st.Due = false
			st.CooldownBlocking = true
			st.NextEligible = next
			return st, true, nil
		},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/chore/alpha/readme-refresh", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	want := next.In(fz).Format("2006-01-02 15:04 MST")
	// Both the next-eligible row and the disabled-open banner, which
	// reuses the same string.
	if !strings.Contains(body, want) || !strings.Contains(body, "cooling down until "+want) {
		t.Errorf("page missing local stamp %q\n%s", want, body)
	}
	if strings.Contains(body, next.Format("2006-01-02 15:04 MST")) {
		t.Errorf("page still renders the UTC stamp\n%s", body)
	}
}
