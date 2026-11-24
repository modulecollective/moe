package serve

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
		name string
		live bool
	}{
		{"killed while live", true},
		{"killed with retained failure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
			fake.reply("wait", `{"exit_code":1}`, 1)
			fake.hold("wait")
			moe, localArgv := argvRecorder(t, 0)
			gate := &fakeHeartbeat{due: []string{"_cabinet"}}
			var log syncBuf
			s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate, Logger: &log})

			s.heartbeatTick()
			first, ok := s.children.get("heartbeat:_cabinet")
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
			waitRexWorkers(t, testRex(s.children))
			if tc.live && !errors.Is(first.exitErr, errRexBlockGone) {
				t.Fatalf("killed sweep exit = %v", first.exitErr)
			}
			if gate.cleanList()[0] {
				t.Fatal("killed/failed sweep was acknowledged as clean")
			}
			waitFor(t, "failure backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.skip["_cabinet"] == 1
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
			fake.reply("run@session:NEW", `{"block_ids":["block:B2"]}`, 0)
			fake.reply("wait", `{"exit_code":1}`, 1)
			s.heartbeatTick()
			recovered, ok := s.children.get("heartbeat:_cabinet")
			if !ok || recovered == first || testBlock(s.children, recovered) != "block:B2" {
				t.Fatalf("replacement sweep = %#v", recovered)
			}
			waitFor(t, "recovered sweep completion", func() bool { return len(gate.sweptList()) == 2 })
			waitRexWorkers(t, testRex(s.children))
			if body, err := os.ReadFile(rexSessionPath(s.opts.Root)); err != nil || string(body) != "session:NEW\n" {
				t.Fatalf("replacement session file = %q, %v", body, err)
			}

			// Another ordinary sweep uses the replacement session.
			waitFor(t, "second failure backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.fails["_cabinet"] == 2
			})
			s.heartbeat.record("_cabinet", false)
			fake.reply("run@session:NEW", `{"block_ids":["block:B3"]}`, 0)
			s.heartbeatTick()
			waitFor(t, "third sweep completion", func() bool { return len(gate.sweptList()) == 3 })
			waitRexWorkers(t, testRex(s.children))
			news := 0
			for _, call := range fake.calls() {
				if strings.HasPrefix(call, "new ") {
					news++
				}
				// The session serve replaced is the sweep's; nothing else
				// is launched into it.
				if strings.HasPrefix(call, "run ") && !strings.Contains(call, " --label heartbeat:_cabinet ") {
					t.Fatalf("rex launched %q beside the sweep", call)
				}
			}
			if news != 1 || localArgv() != "" {
				t.Fatalf("new = %d, local argv = %q; calls = %q", news, localArgv(), fake.calls())
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
	fake.reply("run@session:NEW", `{"block_ids":["block:B1"]}`, 0)
	argv := []string{"/bin/echo", "two words"}
	block, err := h.launch("heartbeat:_cabinet", argv)
	if err != nil || block != "block:B1" {
		t.Fatalf("launch = %q, %v", block, err)
	}
	want := []string{
		"run --json --focus=false --keep-open -s session:S --label heartbeat:_cabinet --cwd " + root + " -- /bin/echo two words",
		"ls --json",
		"new moe --json --keep-open --cwd " + root + " --window bootstrap -- /usr/bin/true",
		"block close block:B1",
		"run --json --focus=false --keep-open -s session:NEW --label heartbeat:_cabinet --cwd " + root + " -- /bin/echo two words",
	}
	if !slices.Equal(fake.calls(), want) {
		t.Errorf("calls = %q; want %q", fake.calls(), want)
	}
	body, err := os.ReadFile(rexSessionPath(root))
	if err != nil || string(body) != "session:NEW\n" || h.sessionID() != "session:NEW" {
		t.Fatalf("session = %q, file = %q, err = %v", h.sessionID(), body, err)
	}
	fake.reply("run@session:NEW", `{"block_ids":["block:B2"]}`, 0)
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
			if block, err := h.launch("heartbeat:_cabinet", []string{"/bin/true"}); err == nil || block != "" {
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
	fake.reply("block-ls", "", 1)
	cs := newChildren()
	installTestRex(cs, newRexHost(fake.bin, t.TempDir(), io.Discard))
	t.Cleanup(func() { testRex(cs).cancel(); waitRexWorkers(t, testRex(cs)) })
	testRex(cs).session = "session:S"
	c, err := cs.spawn("heartbeat:_cabinet", "/bin/true", nil, testRex(cs).root, io.Discard)
	if c != nil || err == nil || testRex(cs).sessionID() != "session:S" {
		t.Fatalf("spawn = %v, %v, session = %q", c, err, testRex(cs).sessionID())
	}
	if _, ok := cs.get("heartbeat:_cabinet"); ok {
		t.Fatal("registered a phantom child")
	}
	if got := fake.calls(); len(got) != 6 || got[0] != "block ls --json -s session:S" || got[1] != "ls --json" || !strings.HasPrefix(got[4], "new moe ") || got[5] != "block ls --json -s session:S" {
		t.Fatalf("calls = %q; want cleanup listing/probe, run, ls, one new, label reconciliation", got)
	}
	// A watcher that had not observed the disappearance can still
	// finish after the failed replacement rather than retry forever.
	fake.reply("wait", "", 1)
	fake.reply("block-call", "", 1)
	if _, err := testRex(cs).waitExit("block:old-live"); !errors.Is(err, errRexBlockGone) {
		t.Fatalf("old watcher after failed replacement = %v", err)
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
