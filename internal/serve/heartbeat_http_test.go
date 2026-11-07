package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// Exercise HTTP consumers with a child dispatched by the real production
// caller. A sweep owns a project job, not the survey or any ridden run.
func TestHeartbeatKeepsRunPagesAndCloseIndependent(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "survey", "pulse")
	seedRun(t, root, "alpha", "fix-it", "sdlc")
	writeCanvas(t, root, "alpha", "survey", "pulse", "# survey findings\n")
	bin := filepath.Join(t.TempDir(), "fake-moe")
	// Honor heartbeatTick's actual emit-file argument. The release file
	// keeps the sweep live until all requests have completed.
	script := `#!/bin/sh
set -eu
emit=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--emit-run" ]; then emit="$2"; fi
  shift
done
test -n "$emit"
printf 'survey\n' > "$emit"
while [ ! -f "$emit.release" ]; do sleep 0.01; done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	row := dash.Row{Project: "alpha", Run: "survey", Note: "pulse:survey",
		Bucket: dash.BucketActiveRuns, When: time.Now()}
	calls := 0
	s := newTestServer(t, Options{
		Root: root, Dynamic: true, Heartbeat: gate, MoeBin: bin,
		GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
			return []dash.Row{row}, 1, 1, nil, nil
		},
		GatherRunRow: func(p, slug string) (dash.Row, bool, error) {
			return row, p == "alpha" && slug == "survey", nil
		},
		ResolveCanvas: func(p, slug, stage string) (string, error) {
			return filepath.Join(root, "projects", p, "runs", slug, "documents", stage, "content.md"), nil
		},
		RunStages: func(_, slug string) ([]string, error) {
			if slug == "survey" {
				return []string{"pulse"}, nil
			}
			return []string{"design", "code", "test", "review", "push"}, nil
		},
		CloseRun: func(p, slug string) error {
			calls++
			if p != "alpha" || slug != "fix-it" {
				t.Errorf("CloseRun(%q, %q), want alpha/fix-it", p, slug)
			}
			return nil
		},
	})
	release := func() {
		t.Helper()
		if err := os.WriteFile(sweepRunPath(root, "alpha")+".release", nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		release()
		if c, ok := s.children.get("heartbeat:alpha"); ok {
			waitFor(t, "final child settlement", c.isSettled)
		}
	})

	s.heartbeatTick()
	c, ok := s.children.get("heartbeat:alpha")
	if !ok {
		t.Fatal("heartbeatTick did not register heartbeat:alpha")
	}
	waitFor(t, "the child to emit its survey", func() bool {
		body, err := os.ReadFile(sweepRunPath(root, "alpha"))
		return err == nil && string(body) == "survey\n"
	})
	if exited, _ := c.snapshot(); exited || c.isSettled() {
		t.Fatal("sweep exited before the HTTP requests")
	}
	s.heartbeatTick()
	if current, _ := s.children.get("heartbeat:alpha"); current != c {
		t.Fatal("a second tick replaced the live sweep")
	}

	get := func(path string) string {
		t.Helper()
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	body := get("/")
	if !strings.Contains(body, `href="/serve"`) || !strings.Contains(body, "serve armed") {
		t.Errorf("dash header lost the serve status: %s", body)
	}
	start := strings.Index(body, `href="/run/alpha/survey"`)
	if start < 0 {
		t.Fatal("dash lost the survey row")
	}
	end := strings.Index(body[start:], "</li>")
	if end < 0 || strings.Contains(body[start:start+end], `class="badge live"`) {
		t.Error("dash row should render without a child live badge")
	}
	body = get("/serve")
	for _, want := range []string{`class="serve-state">sweeping`, `href="/run/alpha/survey"`} {
		if !strings.Contains(body, want) {
			t.Errorf("activity panel missing %q", want)
		}
	}
	body = get("/run/alpha/survey")
	for _, want := range []string{"pulse:survey", "<h1>survey findings</h1>", `href="/run/alpha/survey/canvas/pulse"`} {
		if !strings.Contains(body, want) {
			t.Errorf("survey page missing %q", want)
		}
	}
	start = strings.Index(body, `<p class="meta">`)
	if start < 0 {
		t.Fatal("survey page lost the metadata region")
	}
	end = strings.Index(body[start:], "</p>")
	if end < 0 || strings.Contains(body[start:start+end], `class="badge live"`) {
		t.Error("run metadata should render without a child live badge")
	}
	if body := get("/run/alpha/fix-it"); !strings.Contains(body, `class="status exited">in_progress`) {
		t.Error("run without a dash row should show its stored status")
	}

	// Closing another run must not retire or refuse for the project's
	// host job. This does not establish active-session close safety.
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("POST", "/run/alpha/fix-it/close", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/run/alpha/fix-it" || calls != 1 {
		t.Fatalf("close: status=%d location=%q calls=%d body=%s", rr.Code, rr.Header().Get("Location"), calls, rr.Body.String())
	}
	if current, _ := s.children.get("heartbeat:alpha"); current != c {
		t.Fatal("close removed the heartbeat job")
	}
	if exited, _ := c.snapshot(); exited {
		t.Fatal("close interrupted the heartbeat job")
	}

	release()
	waitFor(t, "sweep settlement", c.isSettled)
	if got := gate.cleanList(); len(got) != 1 || !got[0] {
		t.Fatalf("completed sweep callbacks=%v, want one clean sweep", got)
	}
	body = get("/serve")
	if !strings.Contains(body, `class="serve-kind">exit`) || !strings.Contains(body, `href="/run/alpha/survey"`) {
		t.Error("settled sweep lost its exit event or survey link")
	}
	s.heartbeatTick()
	next, ok := s.children.get("heartbeat:alpha")
	if !ok || next == c {
		t.Fatal("a settled sweep blocked the next dispatch")
	}
	waitFor(t, "replacement sweep settlement", next.isSettled)
	if got := gate.cleanList(); len(got) != 2 || !got[1] {
		t.Fatalf("replacement sweep callbacks=%v, want two clean sweeps", got)
	}
}
