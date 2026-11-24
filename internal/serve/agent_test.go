package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/modulecollective/moe/internal/agent/claude"
	_ "github.com/modulecollective/moe/internal/agent/codex"
	"github.com/modulecollective/moe/internal/agentdefault"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
)

func TestAgentControlAndRefreshWithoutArming(t *testing.T) {
	root := gittest.InitSeeded(t)
	s := newTestServer(t, Options{Root: root, GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }})
	body := getBody(t, s, "/")
	for _, want := range []string{"Default: stylesheet", "Use Codex for 24h", "Use Claude for 24h", "Return to stylesheet", `name="revision" value="0"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if r := postForm(t, s, "/agent", "agent=codex&revision=0"); r.Code != http.StatusSeeOther {
		t.Fatalf("set %d %s", r.Code, r.Body.String())
	}
	head := gittest.HeadSHA(t, root)
	if r := postForm(t, s, "/agent", "agent=codex&revision=0"); r.Code != http.StatusSeeOther || gittest.HeadSHA(t, root) != head {
		t.Fatal("duplicate committed")
	}
	if r := postForm(t, s, "/agent", "agent=claude&revision=0"); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "Default: Codex until") || !strings.Contains(r.Body.String(), `value="1"`) {
		t.Fatalf("stale %d %s", r.Code, r.Body.String())
	}
	if r := postForm(t, s, "/agent", "agent=reset&revision=1"); r.Code != http.StatusSeeOther {
		t.Fatalf("reset %d %s", r.Code, r.Body.String())
	}
	state, err := agentdefault.Set(root, "claude", nil, func() time.Time { return time.Now().Add(-25 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	head = gittest.HeadSHA(t, root)
	var refresh map[string]string
	if err := json.Unmarshal([]byte(getBody(t, s, "/run-status")), &refresh); err != nil {
		t.Fatal(err)
	}
	if refresh["__agent_summary"] != "Default: stylesheet" || refresh["__agent_revision"] != "3" || state.Revision != 3 {
		t.Fatalf("expiry refresh: %v", refresh)
	}
	getBody(t, s, "/")
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("read or expiry committed")
	}
	if err := os.WriteFile(filepath.Join(root, agentdefault.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	body = getBody(t, s, "/")
	if !strings.Contains(body, "Default: unavailable") || !strings.Contains(body, "agent-default.json") {
		t.Fatal("malformed record hidden")
	}
	if c, _ := s.children.get("heartbeat:_cabinet"); c != nil {
		t.Fatal("setting spawned a child")
	}
}

func TestAgentBadRequests(t *testing.T) {
	s := newTestServer(t, Options{Root: gittest.InitSeeded(t)})
	for _, form := range []string{"agent=codex", "agent=codex&revision=x", "agent=other&revision=0", "agent=&revision=0", "agent=reset&revision=-1"} {
		if r := postForm(t, s, "/agent", form); r.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", form, r.Code, r.Body.String())
		}
	}
}

func TestAgentDashboardRefreshKeepsFocusedControls(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed to exercise served JavaScript")
	}
	root := gittest.InitSeeded(t)
	s := newTestServer(t, Options{Root: root, GatherDash: func(string) ([]dash.Row, int, int, []int, error) {
		return []dash.Row{{Project: "p", Run: "r", Note: "sdlc:design", StageBlocked: true, Bucket: dash.BucketActiveRuns}}, 1, 1, nil, nil
	}})
	cmd := exec.Command(node, "testdata/agent_refresh.js")
	cmd.Stdin = strings.NewReader(getBody(t, s, "/"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("refresh: %v %s", err, output)
	}
}

func TestTemporaryAgentDoesNotFreezeRexLaunchArguments(t *testing.T) {
	root := gittest.InitSeeded(t)
	if _, err := agentdefault.Set(root, "codex", nil, time.Now); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	s := rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: &fakeHeartbeat{due: []string{"_cabinet"}}})
	s.heartbeatTick()
	c, ok := s.children.get("heartbeat:_cabinet")
	if !ok {
		t.Fatal("Rex child missing")
	}
	waitFor(t, "Rex settlement", c.isSettled)
	launches := 0
	for _, call := range fake.calls() {
		if strings.HasPrefix(call, "run ") {
			launches++
			if strings.Contains(call, "--agent") || strings.Contains(call, "MOE_FORCE_AGENT=") {
				t.Fatalf("temporary choice frozen into host arguments: %s", call)
			}
			if !strings.Contains(call, " pulse new --dynamic --heartbeat --emit-run ") {
				t.Fatalf("wrong child: %s", call)
			}
		}
	}
	if launches != 1 {
		t.Fatalf("Rex launches=%d", launches)
	}
}

func TestObservationRefreshOnEveryHeaderPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is needed to exercise served JavaScript")
	}
	s := panelServer(t)
	gittest.InitAt(t, s.opts.Root)
	gittest.Commit(t, s.opts.Root, "seed")
	for _, path := range []string{"/", "/projects/alpha", "/input", "/usage"} {
		t.Run(path, func(t *testing.T) {
			cmd := exec.Command(node, "testdata/agent_refresh.js")
			cmd.Stdin = strings.NewReader(getBody(t, s, path))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("refresh: %v %s", err, output)
			}
		})
	}
}

func TestAgentMenuStatesAndConflictRecovery(t *testing.T) {
	root := gittest.InitSeeded(t)
	s := newTestServer(t, Options{Root: root, GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }})
	t.Setenv("MOE_FORCE_AGENT", "claude")
	for _, name := range []string{"", "codex", "claude", ""} {
		if _, err := agentdefault.Set(root, name, nil, time.Now); err != nil {
			t.Fatal(err)
		}
		body := getBody(t, s, "/")
		start, end := strings.Index(body, `<details class="menu">`), strings.Index(body, `</details>`)
		if start < 0 || end < start {
			t.Fatal("native menu missing")
		}
		outside := body[:start] + body[end:]
		if strings.Contains(outside, `id="agent-default"`) || strings.Contains(outside, "Default:") {
			t.Fatal("agent state outside menu")
		}
		menu := body[start:end]
		for _, want := range []string{`role="group" aria-label="Global default agent"`, "Agent default", "this process forces claude", `method="post" action="/agent"`, "Use Codex for 24h", "Use Claude for 24h", "Return to stylesheet"} {
			if !strings.Contains(menu, want) {
				t.Fatalf("menu missing %q", want)
			}
		}
		want := "Default: stylesheet"
		if name != "" {
			want = "Default: " + strings.ToUpper(name[:1]) + name[1:] + " until"
		}
		if !strings.Contains(menu, want) {
			t.Fatal(menu)
		}
	}
	state, err := agentdefault.Set(root, "codex", nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	r := postForm(t, s, "/agent", "agent=claude&revision=0")
	body := r.Body.String()
	for _, want := range []string{"<!doctype html>", `/static/style.css`, "agent default changed; refresh and try again", "Default: Codex until", fmt.Sprintf(`name="revision" value="%d"`, state.Revision), `href="/">Return to dashboard`} {
		if !strings.Contains(body, want) {
			t.Fatalf("conflict missing %q: %s", want, body)
		}
	}
	if r.Code != http.StatusConflict || strings.Index(body, `id="agent-default"`) < strings.Index(body, `</details>`) {
		t.Fatal("retry hidden in menu")
	}
	// Retry the actual form's revision, without JavaScript.
	if r := postForm(t, s, "/agent", fmt.Sprintf("agent=claude&revision=%d", state.Revision)); r.Code != http.StatusSeeOther {
		t.Fatal(r.Body.String())
	}
}

// The endpoint observes current modes and Git queue after initial page render.
func TestHeaderObservationTracksExceptions(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(fmt.Sprint(armed), func(t *testing.T) {
			root, _ := pusherFixture(t)
			seedProject(t, root, "alpha")
			gittest.Run(t, root, "add", "-A")
			gittest.Commit(t, root, "project")
			gittest.Run(t, root, "push", "origin", "main")
			s := newTestServer(t, Options{Root: root, Dynamic: armed, GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 1, 0, nil, nil }})
			getBody(t, s, "/")
			observe := func() string {
				t.Helper()
				head := gittest.HeadSHA(t, root)
				var m map[string]string
				if err := json.Unmarshal([]byte(getBody(t, s, "/run-status")), &m); err != nil {
					t.Fatal(err)
				}
				if gittest.HeadSHA(t, root) != head {
					t.Fatal("observation wrote")
				}
				if m["__serve_cluster"] == "" {
					t.Fatal("cluster missing from refresh")
				}
				return m["__serve_cluster"]
			}
			if err := project.SetMode(root, "alpha", project.ModeSafe); err != nil {
				t.Fatal(err)
			}
			if got := observe(); strings.Contains(got, " safe") || !strings.Contains(got, "1 unpushed") {
				t.Fatal(got)
			}
			if err := project.SetMode(root, "alpha", project.ModePaused); err != nil {
				t.Fatal(err)
			}
			if got := observe(); !strings.Contains(got, "1 paused") || !strings.Contains(got, "2 unpushed") {
				t.Fatal(got)
			}
			now := time.Now()
			s.activity.recordPushFail(now.Add(-12*time.Minute), fmt.Errorf("refused"), now.Add(time.Minute))
			if got := observe(); !strings.Contains(got, "push failing 12m · 2 unpushed") {
				t.Fatal(got)
			}
			// A manual sync empties the queue before activity has recorded recovery.
			gittest.Run(t, root, "push", "origin", "main")
			if got := observe(); strings.Contains(got, "push") {
				t.Fatal(got)
			}
			s.activity.recordPush(now, 2)
			if got := observe(); strings.Contains(got, "push") {
				t.Fatal(got)
			}
		})
	}
}
