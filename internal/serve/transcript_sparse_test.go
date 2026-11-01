package serve

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func TestSparseTranscriptRoute(t *testing.T) {
	root := gittest.Init(t)
	seedRun(t, root, "alpha", "history", "sdlc")
	writeThread(t, root, "alpha", "history", "design", "claude", claudeSample)
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed")
	sess, err := session.Open(root, "beta", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Root: sess.WorktreePath, RunStages: ladderStages})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/run/alpha/history/transcript/design", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "All done here.") {
		t.Fatalf("%d: %s", rr.Code, rr.Body.String())
	}
	links := s.transcriptLinks(run.NewArtifactReader(sess.WorktreePath), "alpha", "history", "design")
	if len(links) != 1 || links[0].Agent != "claude" {
		t.Fatalf("links=%v", links)
	}
	if _, err := os.Stat(filepath.Join(sess.WorktreePath, run.ThreadPathFor("claude", "alpha", "history", "design"))); !os.IsNotExist(err) {
		t.Fatal("serve hydrated transcript")
	}
}
