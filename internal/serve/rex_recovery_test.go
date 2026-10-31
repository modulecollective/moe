package serve

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRexSessionPresent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		status  int
		present bool
		wantErr bool
	}{
		{"present", `{"sessions":[{"session_id":"session:S","label":"moe"}],"revision":2}`, 0, true, false},
		{"empty", `{"sessions":[]}`, 0, false, false},
		{"unrelated same label", `{"sessions":[{"session_id":"session:OTHER","label":"moe"}]}`, 0, false, false},
		{"partial ID", `{"sessions":[{"session_id":"session:S2"}]}`, 0, false, false},
		{"unreachable", "", 1, false, true},
		{"malformed", "{", 0, false, true},
		{"missing array", "{}", 0, false, true},
		{"null array", `{"sessions":null}`, 0, false, true},
		{"object", `{"sessions":{}}`, 0, false, true},
		{"null row", `{"sessions":[null]}`, 0, false, true},
		{"missing ID", `{"sessions":[{"label":"moe"}]}`, 0, false, true},
		{"empty ID", `{"sessions":[{"session_id":""}]}`, 0, false, true},
		{"invalid ID type", `{"sessions":[{"session_id":1}]}`, 0, false, true},
		{"invalid row after match", `{"sessions":[{"session_id":"session:S"},{}]}`, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("ls", tc.reply, tc.status)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			t.Cleanup(h.cancel)
			present, err := h.sessionPresent("session:S")
			if present != tc.present || (err != nil) != tc.wantErr {
				t.Fatalf("present = %v, err = %v; want %v, error %v", present, err, tc.present, tc.wantErr)
			}
			if !slices.Equal(fake.calls(), []string{"ls --json"}) {
				t.Errorf("calls = %q", fake.calls())
			}
		})
	}
}

func TestRexHeartbeatRecoversKilledSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     bool
		dashFail bool
	}{
		{"killed while live", true, false},
		{"killed with retained failure", false, false},
		{"dash failure preserves sweep", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
			fake.reply("wait", `{"exit_code":1}`, 1)
			fake.hold("wait")
			moe, localArgv := argvRecorder(t, 0)
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			var log syncBuf
			s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate, Logger: &log})

			// Join every watcher after its exit hooks. All test sweeps
			// fail, so no asynchronous successful-block close follows.
			var watchers sync.WaitGroup
			onSpawn, onExit := s.children.onSpawn, s.children.onExit
			s.children.onSpawn = func(id string, at time.Time, detail string) {
				watchers.Add(1)
				if onSpawn != nil {
					onSpawn(id, at, detail)
				}
			}
			s.children.onExit = func(id string, at time.Time, err error, tail string) {
				defer watchers.Done()
				if onExit != nil {
					onExit(id, at, err, tail)
				}
			}
			t.Cleanup(func() {
				// Let a watcher finish even if an assertion failed
				// while it was retrying an unreadable session.
				fake.reply("wait", `{"exit_code":1}`, 1)
				fake.reply("block-call", `{"last_exit":{"exit_code":1}}`, 0)
				fake.release("wait")
				watchers.Wait()
			})

			s.heartbeatTick()
			first, ok := s.children.get("heartbeat:alpha")
			if !ok {
				t.Fatal("initial sweep was not registered")
			}
			waitFor(t, "live watcher", func() bool { return fake.called("wait --json block:B1") })
			before := len(fake.calls())
			s.heartbeatTick()
			if len(fake.calls()) != before {
				t.Fatalf("single-flight dispatched beside live sweep: %q", fake.calls())
			}

			if tc.live {
				fake.reply("wait", "", 1)
				fake.reply("block-call", "", 1)
				fake.reply("block-ls", "", 1)
				fake.reply("ls", `{"sessions":[]}`, 0)
			}
			fake.release("wait")
			waitFor(t, "first sweep completion", func() bool { return len(gate.sweptList()) == 1 })
			watchers.Wait()
			if tc.live && !errors.Is(first.exitErr, errRexBlockGone) {
				t.Fatalf("killed sweep exit = %v", first.exitErr)
			}
			if gate.cleanList()[0] {
				t.Fatal("killed/failed sweep was acknowledged as clean")
			}
			waitFor(t, "failure backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.skip["alpha"] == 1
			})
			before = len(fake.calls())
			s.heartbeatTick()
			if len(fake.calls()) != before {
				t.Fatalf("recovery bypassed cool-off: %q", fake.calls())
			}

			fake.reply("block-close", "", 1)
			fake.reply("block-ls", "", 1)
			fake.reply("run", "", 1)
			// During an outage the retained handle must still hold
			// dispatch. Only a later valid listing licenses recovery.
			fake.reply("ls", "", 1)
			before = len(fake.calls())
			s.heartbeatTick()
			if fake.called("new") || len(gate.sweptList()) != 1 {
				t.Fatalf("outage dispatched replacement: %q", fake.calls())
			}
			for _, call := range fake.calls()[before:] {
				if strings.HasPrefix(call, "run ") {
					t.Fatalf("outage escaped retained-block cleanup: %q", fake.calls())
				}
			}

			fake.reply("ls", `{"sessions":[]}`, 0)
			fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"block_ids":["block:B2"]}]}`, 0)
			fake.reply("block-ls@session:NEW", `{"blocks":[]}`, 0)
			fake.reply("run@dash", `{"block_ids":["block:D"]}`, 0)
			if tc.dashFail {
				fake.reply("run@dash", "", 1)
				// Even a failed dash launch must preserve the newly
				// registered sweep, never create a second session.
				fake.reply("ls", `{"sessions":[{"session_id":"session:NEW"}]}`, 0)
			}
			fake.reply("wait", `{"exit_code":1}`, 1)
			s.heartbeatTick()
			recovered, ok := s.children.get("heartbeat:alpha")
			if !ok || recovered == first || recovered.block != "block:B2" {
				t.Fatalf("replacement sweep = %#v", recovered)
			}
			waitFor(t, "recovered sweep completion", func() bool { return len(gate.sweptList()) == 2 })
			watchers.Wait()
			wantDash := "run --json --focus=false --keep-open -s session:NEW --label dash --cwd " + s.opts.Root +
				" -- /usr/bin/env MOE_HOME=" + s.opts.Root + " " + moe + " dash --watch"
			if !slices.Contains(fake.calls(), wantDash) {
				t.Fatalf("dash argv missing: %q; want %q", fake.calls(), wantDash)
			}
			if tc.dashFail && !strings.Contains(log.String(), "rex dash not opened") {
				t.Fatalf("dash failure not logged: %q", log.String())
			}
			if body, err := os.ReadFile(rexSessionPath(s.opts.Root)); err != nil || string(body) != "session:NEW\n" {
				t.Fatalf("replacement session file = %q, %v", body, err)
			}

			// Another ordinary sweep uses the replacement session
			// without reopening or replacing the dash.
			waitFor(t, "second failure backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.fails["alpha"] == 2
			})
			s.heartbeat.record("alpha", false)
			fake.reply("run", `{"block_ids":["block:B3"]}`, 0)
			s.heartbeatTick()
			waitFor(t, "third sweep completion", func() bool { return len(gate.sweptList()) == 3 })
			watchers.Wait()
			news, dashes := 0, 0
			for _, call := range fake.calls() {
				if strings.HasPrefix(call, "new ") {
					news++
				}
				if call == wantDash {
					dashes++
				}
			}
			if news != 1 || dashes != 1 || localArgv() != "" {
				t.Fatalf("new = %d, dash = %d, local argv = %q; calls = %q", news, dashes, localArgv(), fake.calls())
			}
		})
	}
}

func TestRexLaunchRecoversMissingSession(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", "", 1)
	fake.reply("ls", `{"sessions":[{"session_id":"session:OTHER","label":"moe"}]}`, 0)
	fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"block_ids":["block:B1"]}]}`, 0)
	root := t.TempDir()
	h := newRexHost(fake.bin, root, io.Discard)
	t.Cleanup(h.cancel)
	h.session = "session:S"
	argv := []string{"/bin/echo", "two words"}
	block, err := h.launch("heartbeat:alpha", argv)
	if err != nil || block != "block:B1" {
		t.Fatalf("launch = %q, %v", block, err)
	}
	want := []string{
		"run --json --focus=false --keep-open -s session:S --label heartbeat:alpha --cwd " + root + " -- /bin/echo two words",
		"ls --json",
		"new moe --json --keep-open --cwd " + root + " --window heartbeat:alpha -- /bin/echo two words",
		"block rename -s session:NEW block:B1 heartbeat:alpha",
	}
	if !slices.Equal(fake.calls(), want) {
		t.Errorf("calls = %q; want %q", fake.calls(), want)
	}
	body, err := os.ReadFile(rexSessionPath(root))
	if err != nil || string(body) != "session:NEW\n" || h.sessionID() != "session:NEW" {
		t.Fatalf("session = %q, file = %q, err = %v", h.sessionID(), body, err)
	}
	fake.reply("run", `{"block_ids":["block:B2"]}`, 0)
	if block, err := h.launch("heartbeat:beta", argv); err != nil || block != "block:B2" {
		t.Fatalf("later launch = %q, %v", block, err)
	}
	if !fake.called("run --json --focus=false --keep-open -s session:NEW --label heartbeat:beta") {
		t.Errorf("later launch did not use replacement: %q", fake.calls())
	}
}

func TestRexLaunchUncertaintyPreservesSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listing   string
		status    int
		runReply  string
		runStatus int
	}{
		{"present", `{"sessions":[{"session_id":"session:S"}]}`, 0, "", 1},
		{"unreachable", "", 1, "", 1},
		{"malformed listing", "{", 0, "", 1},
		{"missing array", "{}", 0, "", 1},
		{"malformed successful run", `{"sessions":[]}`, 0, "{}", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("run", tc.runReply, tc.runStatus)
			fake.reply("ls", tc.listing, tc.status)
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(rexSessionPath(root)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rexSessionPath(root), []byte("session:S\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h := newRexHost(fake.bin, root, io.Discard)
			t.Cleanup(h.cancel)
			h.session = "session:S"
			if block, err := h.launch("heartbeat:alpha", []string{"/bin/true"}); err == nil || block != "" {
				t.Fatalf("launch = %q, %v; want unresolved", block, err)
			}
			body, err := os.ReadFile(rexSessionPath(root))
			if err != nil || string(body) != "session:S\n" || h.sessionID() != "session:S" || fake.called("new") {
				t.Fatalf("session = %q, file = %q, err = %v, calls = %q", h.sessionID(), body, err, fake.calls())
			}
			if tc.runStatus == 0 && fake.called("ls") {
				t.Error("malformed successful run was treated as absence evidence")
			}
		})
	}
}

func TestRexRecoveryCreationFailureIsBounded(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", "", 1)
	fake.reply("ls", `{"sessions":[]}`, 0)
	fake.reply("new", "", 1)
	cs := newChildren()
	cs.rex = newRexHost(fake.bin, t.TempDir(), io.Discard)
	t.Cleanup(cs.rex.cancel)
	cs.rex.session = "session:S"
	c, err := cs.spawnRex("heartbeat:alpha", "/bin/true", nil, cs.rex.root, io.Discard)
	if c != nil || err == nil || cs.rex.sessionID() != "" {
		t.Fatalf("spawn = %v, %v, session = %q", c, err, cs.rex.sessionID())
	}
	if _, ok := cs.get("heartbeat:alpha"); ok {
		t.Fatal("registered a phantom child")
	}
	if got := fake.calls(); len(got) != 3 || !strings.HasPrefix(got[2], "new moe ") {
		t.Fatalf("calls = %q; want run, ls, one new", got)
	}
}

func TestRexWaitMissingSessionAndOutage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listing string
		status  int
		gone    bool
	}{
		{"gone", `{"sessions":[]}`, 0, true},
		{"present", `{"sessions":[{"session_id":"session:S"}]}`, 0, false},
		{"outage", "", 1, false},
		{"invalid listing", "{}", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("wait", "", 1)
			fake.reply("block-call", "", 1)
			fake.reply("block-ls", "", 1)
			fake.reply("ls", tc.listing, tc.status)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			h.session = "session:S"
			result := make(chan error, 1)
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				_, err := h.waitExit("block:B1")
				result <- err
			}()
			t.Cleanup(func() { h.cancel(); <-joined })
			if tc.gone {
				waitFor(t, "vanished watcher", func() bool { return len(result) == 1 })
				if err := <-result; !errors.Is(err, errRexBlockGone) {
					t.Fatalf("wait = %v; want vanished block", err)
				}
			} else {
				waitFor(t, "session probe", func() bool { return fake.called("ls --json") })
				h.cancel()
				waitFor(t, "cancelled watcher", func() bool { return len(result) == 1 })
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("wait = %v; want uncertainty held until cancellation", err)
				}
			}
			if h.sessionID() != "session:S" || fake.called("new") {
				t.Fatalf("watcher changed identity: %q, %q", h.sessionID(), fake.calls())
			}
		})
	}
}
