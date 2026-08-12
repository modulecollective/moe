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

// panelServer is an armed serve with a project registered on disk (so the
// hub route resolves) and an activity record staged the way a live
// heartbeat would leave it: one project mid-sweep, one quiet, and a failed
// sweep in the ring carrying its output tail.
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
		{Project: "beta", Reason: "a sweep already surveyed the current tip"},
	})
	s.activity.recordSweepStart("alpha", now.Add(-3*time.Minute))
	s.activity.recordChildExit(heartbeatChildPrefix+"beta", now.Add(-40*time.Minute),
		errors.New("exit status 1"), "credit limit reached")
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

// TestServePanelRendersOnTheDash: the operator's question — is serve going
// to pulse, is one running now, what happened last tick — has to be
// answerable from the page they already open.
func TestServePanelRendersOnTheDash(t *testing.T) {
	body := getBody(t, panelServer(t), "/")
	for _, want := range []string{
		"serve-panel",
		"armed",
		"next sweep in",
		"sweeping",
		"the journal moved",
		"a sweep already surveyed the current tip",
		"credit limit reached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dash is missing %q from the serve panel", want)
		}
	}
}

// TestServePanelScopesOnTheProjectHub: the hub embeds the same partial, and
// another project's sweeps there would be noise.
func TestServePanelScopesOnTheProjectHub(t *testing.T) {
	body := getBody(t, panelServer(t), "/projects/alpha")
	if !strings.Contains(body, "the journal moved") {
		t.Error("the hub is missing this project's verdict")
	}
	if strings.Contains(body, "a sweep already surveyed the current tip") {
		t.Error("the hub leaked another project's verdict")
	}
	if strings.Contains(body, "credit limit reached") {
		t.Error("the hub leaked another project's failed sweep")
	}
}

// TestServePanelRendersUnarmed: "it's up but will never pulse" is exactly
// the confusion the panel exists to fix, so an unarmed serve renders the
// strip rather than hiding it.
func TestServePanelRendersUnarmed(t *testing.T) {
	s := newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0",
		Root: t.TempDir(),
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return nil, 0, 0, nil, nil
		},
	})
	body := getBody(t, s, "/")
	if !strings.Contains(body, "browse-only") {
		t.Error("an unarmed serve should say so on the dash")
	}
	if strings.Contains(body, "next sweep in") {
		t.Error("an unarmed serve promised a sweep it will never run")
	}
}

// TestServePanelDoesNotReadTheStateFile: serve holds the record, so a round
// trip through its own snapshot would add a beat of lag for nothing. Proven
// by rendering with no file on disk at all.
func TestServePanelDoesNotReadTheStateFile(t *testing.T) {
	s := panelServer(t)
	if _, ok, _ := ReadActivitySnapshot(s.opts.Root); ok {
		t.Fatal("fixture wrote a state file; this test needs none")
	}
	if body := getBody(t, s, "/"); !strings.Contains(body, "the journal moved") {
		t.Error("the panel rendered nothing without a state file — it should render from memory")
	}
}
