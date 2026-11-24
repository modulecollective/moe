package serve

import (
	"encoding/json"
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
	s := newTestServer(t, Options{Root: root, GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }})
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
