package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

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
