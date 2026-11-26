package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/servepause"
)

func TestPauseHTTPControlsAndRemoteRefresh(t *testing.T) {
	root := gittest.InitSeeded(t)
	s := newTestServer(t, Options{Root: root, GatherDash: func(string) ([]dash.Row, int, int, []int, error) { return nil, 0, 0, nil, nil }})
	for _, path := range []string{"/", "/?project=missing", "/serve"} {
		body := getBody(t, s, path)
		for _, want := range []string{"new automatic sweeps unpaused", "Pause for 24 hours", `action="/serve/pause"`, `name="revision" value="0"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s lacks %s", path, want)
			}
		}
	}
	for _, form := range []string{"", "revision=x", "revision=-1"} {
		if r := postForm(t, s, "/serve/pause", form); r.Code != http.StatusBadRequest {
			t.Fatalf("bad form: %d %s", r.Code, r.Body.String())
		}
	}
	if r := postForm(t, s, "/serve/pause", "revision=0"); r.Code != http.StatusSeeOther {
		t.Fatalf("pause: %d %s", r.Code, r.Body.String())
	}
	state, err := servepause.Read(root)
	if err != nil || !state.Active(time.Now()) {
		t.Fatalf("state: %+v %v", state, err)
	}
	head := gittest.HeadSHA(t, root)
	if r := postForm(t, s, "/serve/pause", "revision=0"); r.Code != http.StatusSeeOther || gittest.HeadSHA(t, root) != head {
		t.Fatal("duplicate pause extended/committed")
	}
	if r := postForm(t, s, "/serve/unpause", "revision=0"); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "refresh and try again") || !strings.Contains(r.Body.String(), `value="1"`) {
		t.Fatalf("stale: %d %s", r.Code, r.Body.String())
	}
	for _, path := range []string{"/", "/serve"} {
		body := getBody(t, s, path)
		if !strings.Contains(body, "new automatic sweeps paused until") || !strings.Contains(body, `action="/serve/unpause"`) {
			t.Fatalf("paused view %s", path)
		}
	}
	if r := postForm(t, s, "/serve/unpause", "revision=1"); r.Code != http.StatusSeeOther {
		t.Fatalf("unpause: %d %s", r.Code, r.Body.String())
	}
	// A remote write and read-time expiry appear without a new server or commit.
	state, err = servepause.Set(root, true, nil, func() time.Time { return time.Now().Add(-25 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	head = gittest.HeadSHA(t, root)
	var refresh map[string]string
	if err := json.Unmarshal([]byte(getBody(t, s, "/run-status")), &refresh); err != nil {
		t.Fatal(err)
	}
	if refresh["__pause_active"] != "false" || refresh["__pause_revision"] != fmt.Sprint(state.Revision) || refresh["__pause_summary"] != "new automatic sweeps unpaused" {
		t.Fatalf("expired refresh: %v", refresh)
	}
	if node, err := exec.LookPath("node"); err == nil {
		for _, path := range []string{"/", "/serve"} {
			cmd := exec.Command(node, "testdata/agent_refresh.js")
			cmd.Stdin = strings.NewReader(getBody(t, s, path))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("refresh %s: %v %s", path, err, output)
			}
		}
	} else {
		t.Fatal("node is required for the pause refresh probe")
	}
	getBody(t, s, "/serve")
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("observation committed")
	}
	if err := os.WriteFile(filepath.Join(root, servepause.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := getBody(t, s, "/serve"); !strings.Contains(body, "pause unavailable") || !strings.Contains(body, "disabled") {
		t.Fatal("malformed state hidden")
	}
	if r := postForm(t, s, "/serve/unpause", "revision=3"); r.Code != http.StatusInternalServerError {
		t.Fatalf("malformed write: %d", r.Code)
	}
	if gate := s.children.sweepActive(); gate {
		t.Fatal("control spawned a child")
	}
}

type pauseDuringDue struct {
	*fakeHeartbeat
	root string
	t    *testing.T
}

func (g *pauseDuringDue) Due(log io.Writer) []HeartbeatDecision {
	decisions := g.fakeHeartbeat.Due(log)
	if _, err := servepause.Set(g.root, true, nil, time.Now); err != nil {
		g.t.Fatal(err)
	}
	return decisions
}

func TestPauseDuringDueWinsFinalAdmission(t *testing.T) {
	root := gittest.InitSeeded(t)
	bin, argv := argvRecorder(t, 0)
	gate := &pauseDuringDue{fakeHeartbeat: &fakeHeartbeat{due: []string{"alpha"}}, root: root, t: t}
	s := newTestServer(t, Options{Root: root, MoeBin: bin, Heartbeat: gate})
	s.heartbeatTick()
	if gate.dispatchingCount() != 0 || argv() != "" || len(gate.sweptList()) != 0 {
		t.Fatal("pause during evaluation admitted a child")
	}
	if lines := tickLines(s); !strings.Contains(strings.Join(lines, " "), "automatic sweeps paused until") {
		t.Fatalf("pause verdict missing: %v", lines)
	}
	// Paused ticks are normal pacing and continue read-only evaluation.
	s.heartbeatTick()
	if gate.passCount() != 2 {
		t.Fatal("pause prevented due evaluation")
	}
	s.heartbeat.mu.Lock()
	defer s.heartbeat.mu.Unlock()
	if len(s.heartbeat.fails) != 0 || len(s.heartbeat.skip) != 0 {
		t.Fatal("paused ticks changed failure accounting")
	}
}

func TestPauseLetsAdmittedSweepFinishAndHoldsNext(t *testing.T) {
	root := gittest.InitSeeded(t)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	// The child writes its start before waiting on an explicit release. No
	// guessed timing or wall-clock runtime stands in for dispatch evidence.
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	bin := filepath.Join(dir, "fake-moe")
	body := fmt.Sprintf("#!/bin/sh\ntouch '%s'\nwhile [ ! -e '%s' ]; do sleep 0.01; done\n", started, release)
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, Options{Root: root, MoeBin: bin, Heartbeat: gate})
	// Ensure teardown also releases the child if an assertion fails.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644); s.children.shutdown(context.Background(), io.Discard) })
	s.heartbeatTick()
	waitFor(t, "admitted child", func() bool { _, err := os.Stat(started); return err == nil })
	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.heartbeatTick()
	}
	if gate.dispatchingCount() != 1 || len(gate.sweptList()) != 0 {
		t.Fatal("paused running sweep replaced/cancelled")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "clean settlement through pause", func() bool { return len(gate.sweptList()) == 1 })
	if !gate.cleanList()[0] {
		t.Fatal("pause turned completion into failure")
	}
	s.heartbeatTick()
	if gate.dispatchingCount() != 1 {
		t.Fatal("paused next child admitted")
	}
	if _, err := servepause.Set(root, false, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	if gate.dispatchingCount() != 1 {
		t.Fatal("unpause forced dispatch")
	}
	s.heartbeatTick()
	waitFor(t, "ordinary unpaused tick", func() bool { return len(gate.sweptList()) == 2 })
	if gate.dispatchingCount() != 2 {
		t.Fatal("unpause did not permit next tick")
	}
}

func TestPauseExpiryAndBackoffRemainIndependent(t *testing.T) {
	root := gittest.InitSeeded(t)
	bin, _ := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{Root: root, MoeBin: bin, Heartbeat: gate})
	if _, err := servepause.Set(root, true, nil, func() time.Time { return time.Now().Add(-25 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	s.heartbeat.record(project.RootID, true)
	s.heartbeat.record(project.RootID, true) // two cooling ticks
	s.heartbeatTick()
	if gate.dispatchingCount() != 0 {
		t.Fatal("expiry bypassed backoff")
	}
	if _, err := servepause.Set(root, false, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	s.heartbeatTick()
	if gate.dispatchingCount() != 0 {
		t.Fatal("unpause reset backoff")
	}
	s.heartbeatTick()
	waitFor(t, "eligible expiry tick", func() bool { return len(gate.sweptList()) == 1 })
	// A malformed record also holds final admission without changing completion.
	if err := os.WriteFile(filepath.Join(root, servepause.FileName), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.heartbeatTick()
	if gate.dispatchingCount() != 1 {
		t.Fatal("malformed state admitted sweep")
	}
}

func TestPausePreservesAdoptedRexSweep(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("whoami", `{"instance_id":"i-1"}`, 0)
	fake.reply("version", `{"api_revision":8}`, 0)
	fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:_cabinet"}]}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	fake.hold("wait")
	t.Cleanup(func() { fake.release("wait") })
	useRexBin(t, fake.bin)
	root := gittest.InitSeeded(t)
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rexSessionPath(root), []byte("session:OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, argv := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"_cabinet"}}
	s := newTestServer(t, Options{Root: root, MoeBin: bin, Heartbeat: gate})
	s.selectHost()
	c, ok := s.children.get("heartbeat:_cabinet")
	if !ok {
		t.Fatal("sweep not adopted")
	}
	if _, err := servepause.Set(root, true, nil, time.Now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.heartbeatTick()
	}
	if gate.dispatchingCount() != 0 || fake.called("run ") || fake.called("new") || argv() != "" {
		t.Fatalf("paused adopted sweep replaced: %v", fake.calls())
	}
	if child, ok := s.children.get("heartbeat:_cabinet"); !ok || child != c {
		t.Fatal("adopted identity lost")
	}
	fake.release("wait")
	waitFor(t, "adopted completion during pause", c.isSettled)
	waitRexWorkers(t, testRex(s.children))
	if len(gate.sweptList()) != 0 {
		t.Fatal("adoption acknowledged a dispatch it never observed")
	}
	s.heartbeatTick()
	if gate.dispatchingCount() != 0 || fake.called("run ") || argv() != "" {
		t.Fatal("next child launched through pause")
	}
}
