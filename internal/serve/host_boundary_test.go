package serve

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRexExitEvidenceRequiresPresence(t *testing.T) {
	for _, tc := range []struct {
		reply         string
		valid, exited bool
		code          int
	}{
		{`{}`, false, false, 0}, {`null`, false, false, 0}, {`{"last_exit":{}}`, false, false, 0},
		{`{"last_exit":{"exit_code":null}}`, false, false, 0}, {`{"last_exit":{"exit_code":"0"}}`, false, false, 0},
		{`{"last_exit":false}`, false, false, 0}, {`{"last_exit":null}`, true, false, 0},
		{`{"last_exit":{"exit_code":0,"extra":true}}`, true, true, 0}, {`{"last_exit":{"exit_code":7}}`, true, true, 7},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-call", tc.reply, 0)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			defer h.cancel()
			code, exited, err := h.lastExit("block:A")
			if (err == nil) != tc.valid || exited != tc.exited || code != tc.code {
				t.Fatalf("lastExit = %d, %t, %v", code, exited, err)
			}
		})
	}
}

func TestRexInvalidEvidenceCannotAcknowledge(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{}`, 0)
	fake.reply("block-call", `{"last_exit":{}}`, 0)
	fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"}]}`, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
	s.heartbeatTick()
	waitFor(t, "invalid exit readback", func() bool { return fake.called("block call") })
	testRex(s.children).cancel()
	waitRexWorkers(t, testRex(s.children))
	c, _ := s.children.get("heartbeat:alpha")
	if exited, _ := c.snapshot(); exited || len(gate.sweptList()) != 0 || fake.called("block rename") || fake.called("block close") {
		t.Fatal("incomplete exit became a result")
	}
}

func TestRexOwnedLabelsValidateBeforeAdoption(t *testing.T) {
	for _, label := range []string{"heartbeat:alpha", "heartbeat:", "heartbeat:../outside", "heartbeat:UPPER", "done:heartbeat:../outside"} {
		t.Run(label, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"},{"block_id":"block:B","label":"`+label+`"}]}`, 0)
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true"})
			if err := testRex(s.children).adopt(); err == nil {
				t.Fatal("invalid or duplicate owned label accepted")
			}
			if len(s.children.all) != 0 || fake.called("wait ") {
				t.Fatal("listing partially adopted")
			}
			if _, err := testRex(s.children).findLabel("heartbeat:alpha"); err == nil {
				t.Fatal("launch readback accepted ambiguous listing")
			}
		})
	}
}

func TestRexCommandIDsRequired(t *testing.T) {
	for _, reply := range []string{`{}`, `null`, `{"block_ids":[]}`, `{"block_ids":[""]}`, `{"block_ids":[null]}`, `{"block_ids":["block:A",""]}`} {
		t.Run("run "+reply, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("run", reply, 0)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			defer h.cancel()
			h.session = "session:S"
			if block, err := h.launch("heartbeat:alpha", []string{"/bin/true"}); block != "" || err == nil {
				t.Fatalf("invalid reply accepted: %q, %v", block, err)
			}
			if fake.called("new ") || fake.called("ls ") {
				t.Fatal("invalid reply became absence evidence")
			}
		})
	}
	for _, reply := range []string{`{}`, `{"session_id":null}`, `{"session_id":"session:S","initial_windows":[{"block_ids":[null]}]}`, `{"session_id":"session:S","initial_windows":[{"block_ids":[""]}]}`} {
		t.Run("new "+reply, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("new", reply, 0)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			defer h.cancel()
			if _, err := h.launch("heartbeat:alpha", []string{"/bin/echo", "agent-work"}); err == nil {
				t.Fatal("invalid creation accepted")
			}
			if fake.called("run ") || h.sessionID() != "" {
				t.Fatal("invalid creation launched work or published identity")
			}
			calls := fake.calls()
			if len(calls) != 1 || !strings.HasSuffix(calls[0], "--window bootstrap -- /usr/bin/true") {
				t.Fatalf("unsafe creation: %q", calls)
			}
		})
	}
}

func TestRexBootstrapPersistencePrecedesWork(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"block_ids":["block:BOOT"]}]}`, 0)
	fake.reply("run", `{"block_ids":["block:JOB"]}`, 0)
	root := t.TempDir()
	// A directory makes rename fail even as root; the candidate must survive.
	if err := os.MkdirAll(rexSessionPath(root), 0755); err != nil {
		t.Fatal(err)
	}
	h := newRexHost(fake.bin, root, io.Discard)
	defer h.cancel()
	h.session = "session:OLD"
	fake.reply("run@session:OLD", "", 1)
	fake.reply("ls", `{"sessions":[]}`, 0)
	if _, err := h.launch("heartbeat:alpha", []string{"/bin/echo", "agent-work"}); err == nil {
		t.Fatal("failed persistence accepted")
	}
	if h.sessionID() != "session:OLD" || h.candidate != "session:NEW" || fake.called("run --json --focus=false --keep-open -s session:NEW") {
		t.Fatalf("unrecorded session dispatched or replaced identity: %q", fake.calls())
	}
	if err := os.Remove(rexSessionPath(root)); err != nil {
		t.Fatal(err)
	}
	fake.hold("run@session:NEW")
	result := make(chan error, 1)
	go func() { _, err := h.launch("heartbeat:alpha", []string{"/bin/echo", "agent-work"}); result <- err }()
	t.Cleanup(func() { fake.release("run@session:NEW"); <-result })
	waitFor(t, "labelled run after persistence", func() bool { return fake.called("run --json --focus=false --keep-open -s session:NEW") })
	if body, _ := os.ReadFile(rexSessionPath(root)); string(body) != "session:NEW\n" {
		t.Fatalf("run before durable identity: %q", body)
	}
	if fake.count("new ") != 1 || fake.called("block rename") {
		t.Fatalf("candidate recreated or first job renamed: %q", fake.calls())
	}
}

func TestRexIdentityReadFailuresHoldAndRecover(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "directory"}[directory], func(t *testing.T) {
			fake := rexStartupFake(t)
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(rexSessionPath(root)), 0755); err != nil {
				t.Fatal(err)
			}
			if directory {
				if err := os.Mkdir(rexSessionPath(root), 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(rexSessionPath(root), nil, 0600); err != nil {
				t.Fatal(err)
			}
			gate := &fakeHeartbeat{quiet: []string{"alpha"}}
			s := newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			s.selectHost()
			s.heartbeatTick()
			if s.activity.snapshot(time.Now()).HostError == "" || fake.called("run ") || fake.called("new ") {
				t.Fatal("unresolved identity did not hold idle host")
			}
			if err := os.Remove(rexSessionPath(root)); err != nil {
				t.Fatal(err)
			}
			seedStartupRexSession(t, root)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"}]}`, 0)
			fake.hold("wait")
			fake.reply("run@dash", `{"block_ids":["block:D"]}`, 0)
			s.heartbeatTick()
			if _, ok := s.children.get("heartbeat:alpha"); !ok || s.activity.snapshot(time.Now()).HostError != "" {
				t.Fatal("idle recovery did not adopt and clear hold")
			}
		})
	}
}

// A controlled listener exercises Serve's return paths without local sockets.
type lifecycleListener struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	err     error
}

func (l *lifecycleListener) Accept() (net.Conn, error) {
	close(l.entered)
	if l.err != nil {
		return nil, l.err
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *lifecycleListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *lifecycleListener) Addr() net.Addr { return &net.TCPAddr{} }

type lifecycleHost struct {
	cs      *children
	stopped bool
}

func (h *lifecycleHost) reconcile() error { return nil }
func (h *lifecycleHost) spawn(string, string, []string, string, io.Writer) (*child, error) {
	panic("unexpected spawn")
}
func (h *lifecycleHost) shutdown(ctx context.Context, _ io.Writer) {
	h.cs.mu.RLock()
	defer h.cs.mu.RUnlock()
	h.stopped = h.cs.stopping && ctx.Err() == nil
}
func TestServerCleansHostOnEitherListenerReturn(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "listener failure"}[failure], func(t *testing.T) {
			s := newTestServer(t, Options{Root: t.TempDir()})
			host := &lifecycleHost{cs: s.children}
			s.children.host = host
			listener := &lifecycleListener{entered: make(chan struct{}), closed: make(chan struct{})}
			sentinel := errors.New("listener failed")
			if failure {
				listener.err = sentinel
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- s.serve(ctx, listener) }()
			<-listener.entered
			if !failure {
				cancel()
			}
			select {
			case err := <-result:
				if failure && !errors.Is(err, sentinel) {
					t.Fatalf("lost listener error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not stop")
			}
			if !host.stopped {
				t.Fatal("host cleanup skipped or stopping flag late")
			}
			if _, err := os.Stat(filepath.Join(s.opts.Root, ".moe", "serve.json")); !os.IsNotExist(err) {
				t.Fatalf("activity survived: %v", err)
			}
		})
	}
}

func TestRexRecordedIdentityRetriesMissingDiscovery(t *testing.T) {
	for _, missingCLI := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing CLI", false: "whoami failure"}[missingCLI], func(t *testing.T) {
			fake := rexStartupFake(t)
			if missingCLI {
				useRexBin(t, "moe-test-no-rex")
			} else {
				fake.reply("whoami", "", 1)
			}
			root := t.TempDir()
			seedStartupRexSession(t, root)
			gate := &fakeHeartbeat{quiet: []string{"alpha"}}
			s := newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			s.selectHost()
			s.heartbeatTick()
			if testRex(s.children) == nil || s.activity.snapshot(time.Now()).HostError == "" {
				t.Fatal("recorded identity selected local or hid discovery hold")
			}
			useRexBin(t, fake.bin)
			fake.reply("whoami", `{"instance_id":"i-1"}`, 0)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"}]}`, 0)
			fake.reply("run@dash", `{"block_ids":["block:D"]}`, 0)
			fake.hold("wait")
			s.heartbeatTick()
			if _, ok := s.children.get("heartbeat:alpha"); !ok || s.activity.snapshot(time.Now()).HostError != "" {
				t.Fatal("idle discovery recovery did not adopt existing work")
			}
		})
	}
}

func TestRexLostRunReplyRecovery(t *testing.T) {
	for _, mode := range []string{"readback", "later tick", "duplicate label"} {
		t.Run(mode, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.trackBlocks()
			fake.reply("ls", `{"sessions":[{"session_id":"session:S"}]}`, 0)
			fake.reply("run", `{"block_ids":["block:A"]}`, 1)
			if err := os.WriteFile(filepath.Join(fake.dir, "run-lost"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			fake.hold("run")
			fake.hold("wait")
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
			tickDone := make(chan struct{})
			go func() { s.heartbeatTick(); close(tickDone) }()
			t.Cleanup(func() { fake.release("run"); <-tickDone })
			waitFor(t, "in-flight launch", func() bool { return fake.called("run ") })
			switch mode {
			case "later tick":
				fake.reply("block-ls", "", 1)
			case "duplicate label":
				if err := os.WriteFile(filepath.Join(fake.dir, "blocks", "block:B"), []byte("heartbeat:alpha"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fake.release("run")
			<-tickDone
			if mode != "readback" {
				if _, ok := s.children.get("heartbeat:alpha"); ok {
					t.Fatal("uncertain readback registered a child")
				}
			}
			if mode == "duplicate label" {
				s.heartbeatTick()
				if len(s.children.all) != 0 || fake.count("run ") != 1 {
					t.Fatal("ambiguous readback dispatched or partially registered")
				}
				return
			}
			fake.reply("block-ls", `{"blocks":[]}`, 0)
			path := sweepRunPath(s.opts.Root, "alpha")
			if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			s.heartbeatTick()
			if body, _ := os.ReadFile(path); string(body) != "sentinel" {
				t.Fatal("recovery erased the running job emit file")
			}
			c, ok := s.children.get("heartbeat:alpha")
			if !ok || testBlock(s.children, c) != "block:A" || fake.count("run ") != 1 {
				t.Fatalf("lost reply duplicated or lost job: %q", fake.calls())
			}
			waitFor(t, "recovered observer", func() bool { return fake.called("wait --json block:A") })
		})
	}
}

func TestRexBootstrapDirectoryFailureRetainsCandidate(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"block_ids":["block:BOOT"]}]}`, 0)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".moe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	h := newRexHost(fake.bin, root, io.Discard)
	defer h.cancel()
	if err := h.establish(); err == nil || h.candidate != "session:NEW" || h.sessionID() != "" {
		t.Fatalf("persistence failure discarded or published candidate: %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".moe")); err != nil {
		t.Fatal(err)
	}
	if err := h.establish(); err != nil {
		t.Fatal(err)
	}
	if fake.count("new ") != 1 || fake.called("run ") || h.sessionID() != "session:NEW" {
		t.Fatalf("retry recreated session or started work: %q", fake.calls())
	}
}
