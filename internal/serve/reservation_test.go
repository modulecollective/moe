package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/session"
)

func TestStaleMutationPostsRefuseSessionRefs(t *testing.T) {
	for _, action := range []string{"advance", "close", "capture-close"} {
		t.Run(action, func(t *testing.T) {
			root := newGitServeRoot(t)
			workflow := "sdlc"
			if action == "capture-close" {
				workflow = "idea"
			}
			seedRun(t, root, "alpha", "fix-it", workflow)
			gittest.Commit(t, root, "seed run")
			branch := "session/alpha/fix-it/retired-doc"
			gittest.Run(t, root, "branch", branch)
			s := newTestServer(t, Options{Root: root,
				CloseRun: func(p, r string) error { return session.RequireUnreserved(root, p, r) },
				GatherRunRow: func(p, r string) (dash.Row, bool, error) {
					t.Fatal("stage lookup preceded occupancy refusal")
					return dash.Row{}, false, nil
				},
			})
			before := gittest.Output(t, root, "rev-parse", "HEAD")
			route := action
			if route == "capture-close" {
				route = "close"
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/alpha/fix-it/"+route, nil))
			if rr.Code != http.StatusConflict {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			if before != gittest.Output(t, root, "rev-parse", "HEAD") {
				t.Fatal("refusal committed")
			}
			if status := gittest.Output(t, root, "status", "--porcelain"); status != "" {
				t.Fatal(status)
			}
			if len(s.children.all) != 0 {
				t.Fatal("mutation launched a child")
			}
		})
	}
}
