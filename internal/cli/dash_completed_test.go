package cli

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/serve"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// Exercise journal scan -> shared rows -> run list and the real web wiring.
// The old fold hid this fresh completion beneath a three-day-old pulse.
func TestCompletedChildVisibleAcrossDashSurfaces(t *testing.T) {
	root := newMoeHome(t)
	t.Setenv("NO_COLOR", "1")
	base := time.Now().UTC()
	trailerstest.SeedRun(t, root, "p", "pulse", "pulse", run.StatusClosed)
	trailerstest.CommitTrailer(t, root, "close pulse", "MoE-Run: pulse\nMoE-Project: p\nMoE-Workflow: pulse\nMoE-Closed: true", base.Add(-72*time.Hour))
	for i := range 6 {
		id := fmt.Sprintf("other-%d", i)
		trailerstest.SeedRun(t, root, "p", id, "sdlc", run.StatusClosed)
		trailerstest.CommitTrailer(t, root, "close "+id, "MoE-Run: "+id+"\nMoE-Project: p\nMoE-Workflow: sdlc\nMoE-Closed: true", base.Add(-time.Duration(i+1)*time.Hour))
	}
	trailerstest.SeedRun(t, root, "p", "child", "sdlc", run.StatusClosed)
	trailerstest.CommitTrailer(t, root, "open child", "MoE-Run: child\nMoE-Project: p\nMoE-Workflow: sdlc\nMoE-Spawned-By: p/pulse", base.Add(-48*time.Hour))
	trailerstest.CommitTrailer(t, root, "close child", "MoE-Run: child\nMoE-Project: p\nMoE-Workflow: sdlc\nMoE-Closed: true", base)
	for _, args := range [][]string{{"run", "list", "--all"}, {"run", "list", "--all", "--project", "p"}} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errb.String())
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != 8 || !strings.HasPrefix(lines[0], "p/child\t") || !strings.HasSuffix(lines[0], "· from p/pulse") || strings.Contains(out.String(), "↳") {
			t.Fatalf("%v: child must be listed first, flat, with lineage, among all 8:\n%s", args, out.String())
		}
	}
	s, err := serve.New(serveOptions(root, &bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/projects/p", "/?completed=7&fragment=1", "/projects/p?completed=7&fragment=1", "/?completed=30", "/projects/p?completed=30"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		first := strings.Index(body, `class="slug" href="/run/p/child"`)
		other := strings.Index(body, `class="slug" href="/run/p/other-0"`)
		if first < 0 || other < first || !strings.Contains(body, "· from p/pulse") || strings.Contains(body, `class="row nested`) || strings.Contains(body, "↳") || !strings.Contains(body, `badge agent`) {
			t.Fatalf("GET %s: child must be first, flat, with lineage and badge:\n%s", path, body)
		}
		want := dash.CompletedCap
		if strings.Contains(path, "completed=7") {
			want = 7
		}
		if strings.Contains(path, "completed=30") {
			want = 8
		}
		if got := strings.Count(body, `class="slug" href="/run/p/`); got != want {
			t.Fatalf("GET %s: rows=%d, want %d", path, got, want)
		}
	}
}
