package serve

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These files are the fake server's state, shared by successive serve
// instances. The CLI fake mutates them on successful rename and close;
// a restart cannot see a done label unless production wrote it.
func (f *fakeRex) trackBlocks(blocks ...rexBlock) {
	f.t.Helper()
	dir := filepath.Join(f.dir, "blocks")
	if err := os.Mkdir(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for _, b := range blocks {
		if err := os.WriteFile(filepath.Join(dir, b.ID), []byte(b.Label), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeRex) blockLabel(id string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "blocks", id))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fakeRex) count(prefix string) int {
	n := 0
	for _, c := range f.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestRexAcknowledgedFailureSurvivesRestarts(t *testing.T) {
	fake := newFakeRex(t)
	fake.trackBlocks(
		rexBlock{ID: "block:B", Label: "done:heartbeat:beta"},
		rexBlock{ID: "block:X", Label: "shell"},
		rexBlock{ID: "block:D", Label: "done:other"},
	)
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{"exit_code":1}`, 1)
	fake.reply("capture", "failure screen", 0)
	root := t.TempDir()
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	var log syncBuf
	s := rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate, Logger: &log})
	var notifications, exits atomic.Int32
	s.children.notify = func(string, error) { notifications.Add(1) }
	onExit := s.children.onExit
	s.children.onExit = func(id string, at time.Time, err error, tail string) { exits.Add(1); onExit(id, at, err, tail) }
	s.heartbeatTick()
	waitRexWorkers(t, testRex(s.children))
	c, ok := s.children.get("heartbeat:alpha")
	if !ok || c.tail() != "failure screen\n" || s.heartbeat.fails["alpha"] != 1 || s.heartbeat.skip["alpha"] != 1 || !slices.Equal(gate.cleanList(), []bool{false}) {
		t.Fatalf("failure was not reported once: child %v, fails %v, skips %v, clean %v", c, s.heartbeat.fails, s.heartbeat.skip, gate.cleanList())
	}
	if fake.blockLabel("block:A") != "done:heartbeat:alpha" || fake.called("block close") {
		t.Fatal("failed screen was not acknowledged and retained")
	}
	testRex(s.children).cancel()
	sentinel := sweepRunPath(root, "alpha")
	if err := os.WriteFile(sentinel, []byte("sentinel\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var restarted *Server
	for range 2 {
		freshGate := &fakeHeartbeat{due: []string{"alpha"}}
		restarted = rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: freshGate, Logger: &log})
		restarted.children.notify = func(string, error) { notifications.Add(1) }
		exit := restarted.children.onExit
		restarted.children.onExit = func(id string, at time.Time, err error, tail string) { exits.Add(1); exit(id, at, err, tail) }
		before := fake.count("wait ")
		if err := testRex(restarted.children).adopt(); err != nil {
			t.Fatal(err)
		}
		waitRexWorkers(t, testRex(restarted.children))
		if len(restarted.children.all) != 0 || fake.count("wait ") != before || len(freshGate.sweptList()) != 0 || len(restarted.heartbeat.fails) != 0 || len(restarted.heartbeat.skip) != 0 {
			t.Fatal("restart adopted or accounted for an acknowledged screen")
		}
		if b, err := os.ReadFile(sentinel); err != nil || string(b) != "sentinel\n" {
			t.Fatalf("restart consumed emit sentinel: %q, %v", b, err)
		}
		if notifications.Load() != 1 || exits.Load() != 1 || strings.Count(log.String(), "child heartbeat:alpha exited:") != 1 {
			t.Fatalf("completion replayed: notify %d, exit %d, log %q", notifications.Load(), exits.Load(), log.String())
		}
		if fake.blockLabel("block:A") != "done:heartbeat:alpha" {
			t.Fatal("restart discarded failed screen")
		}
	}
	// The latest server has no old child registry entry to help cleanup.
	fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
	fake.hold("wait")
	restarted.heartbeatTick()
	waitFor(t, "replacement watcher", func() bool { return fake.called("wait --json block:NEW") })
	if fake.blockLabel("block:A") != "" || fake.blockLabel("block:B") != "done:heartbeat:beta" || fake.blockLabel("block:X") != "shell" || fake.blockLabel("block:D") != "done:other" {
		t.Fatal("cleanup did not stay within the project's done namespace")
	}
	calls := fake.calls()
	closed := slices.Index(calls, "block close block:A")
	run := -1
	for i, call := range calls {
		if strings.HasPrefix(call, "run ") {
			run = i
		}
	}
	if closed < 0 || closed >= run || fake.count("block close ") != 1 {
		t.Fatalf("retained screen not closed before replacement: %q", calls)
	}
	testRex(restarted.children).cancel()
	waitRexWorkers(t, testRex(restarted.children))
}

func TestRexAcknowledgedSuccessCloseFailure(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same serve"
		if restart {
			name = "fresh serve"
		}
		t.Run(name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.trackBlocks()
			fake.reply("run", `{"block_ids":["block:A"]}`, 0)
			fake.reply("wait", `{"exit_code":0}`, 0)
			fake.reply("block-close", "", 1)
			root := t.TempDir()
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			s := rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			var reports atomic.Int32
			s.children.notify = func(string, error) { reports.Add(1) }
			s.heartbeatTick()
			waitRexWorkers(t, testRex(s.children))
			if fake.blockLabel("block:A") != "done:heartbeat:alpha" || reports.Load() != 1 {
				t.Fatal("success close failure left an unacknowledged result")
			}
			if restart {
				testRex(s.children).cancel()
				gate = &fakeHeartbeat{due: []string{"alpha"}}
				s = rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
				s.children.notify = func(string, error) { reports.Add(1) }
				if err := testRex(s.children).adopt(); err != nil {
					t.Fatal(err)
				}
				waitRexWorkers(t, testRex(s.children))
				if fake.count("wait ") != 1 || reports.Load() != 1 {
					t.Fatal("success notification replayed on restart")
				}
			}
			before := fake.count("block close block:A")
			s.heartbeatTick()
			if fake.count("run ") != 1 || fake.count("block close block:A") != before+1 {
				t.Fatal("failed cleanup did not hold replacement or was not deduplicated")
			}
			fake.reply("block-close", "", 0)
			fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
			fake.hold("wait")
			s.heartbeatTick()
			if fake.blockLabel("block:A") != "" || fake.count("run ") != 2 {
				t.Fatal("next launch did not clean up retained success")
			}
			testRex(s.children).cancel()
			waitRexWorkers(t, testRex(s.children))
		})
	}
}

func TestRexAcknowledgementFollowsHooksAndHoldsReplacement(t *testing.T) {
	fake := newFakeRex(t)
	fake.trackBlocks()
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{"exit_code":1}`, 1)
	fake.hold("block-rename")
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
	notifyEntered, exitEntered := make(chan struct{}), make(chan struct{})
	notifyRelease, exitRelease := make(chan struct{}), make(chan struct{})
	releaseNotify := sync.OnceFunc(func() { close(notifyRelease) })
	releaseExit := sync.OnceFunc(func() { close(exitRelease) })
	t.Cleanup(func() { releaseNotify(); releaseExit(); fake.release("block-rename") })
	var notifications, exits atomic.Int32
	s.children.notify = func(string, error) { notifications.Add(1); close(notifyEntered); <-notifyRelease }
	onExit := s.children.onExit
	s.children.onExit = func(id string, at time.Time, err error, tail string) {
		exits.Add(1)
		close(exitEntered)
		<-exitRelease
		onExit(id, at, err, tail)
	}
	s.heartbeatTick()
	c, _ := s.children.get("heartbeat:alpha")
	assertHeld := func() {
		t.Helper()
		replacement, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, io.Discard)
		if replacement != nil || err == nil || !strings.Contains(err.Error(), "completing") {
			t.Fatalf("pending retirement allowed replacement: %v, %v", replacement, err)
		}
		if current, _ := s.children.get("heartbeat:alpha"); current != c || fake.count("run ") != 1 || fake.called("block close") {
			t.Fatal("replacement overtook retirement")
		}
	}
	waitFor(t, "notifier", func() bool {
		select {
		case <-notifyEntered:
			return true
		default:
			return false
		}
	})
	if fake.called("block rename") {
		t.Fatal("acknowledged before notification returned")
	}
	path := sweepRunPath(s.opts.Root, "alpha")
	if err := os.WriteFile(path, []byte("sentinel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.heartbeatTick() // completion is still held in the notifier
	s.heartbeatTick() // retirement must hold before clearing the emit file
	if body, err := os.ReadFile(path); err != nil || string(body) != "sentinel\n" {
		t.Fatalf("pending retirement cleared the old emit file: %q, %v", body, err)
	}
	assertHeld()
	releaseNotify()
	waitFor(t, "exit hook", func() bool {
		select {
		case <-exitEntered:
			return true
		default:
			return false
		}
	})
	if fake.called("block rename") {
		t.Fatal("acknowledged before exit hook returned")
	}
	assertHeld()
	releaseExit()
	waitFor(t, "held acknowledgement", func() bool { return fake.called("block rename") })
	assertHeld()
	fake.release("block-rename")
	waitRexWorkers(t, testRex(s.children))
	if notifications.Load() != 1 || exits.Load() != 1 || s.heartbeat.fails["alpha"] != 1 || fake.blockLabel("block:A") != "done:heartbeat:alpha" {
		t.Fatal("retirement lost or duplicated completion")
	}
}

func TestRexAcknowledgementRecovery(t *testing.T) {
	for _, mode := range []string{"retry", "lost reply", "shutdown", "manually closed", "notify error"} {
		t.Run(mode, func(t *testing.T) {
			oldDelay := rexRetryDelay
			rexRetryDelay = 10 * time.Millisecond
			t.Cleanup(func() { rexRetryDelay = oldDelay })
			fake := newFakeRex(t)
			fake.trackBlocks()
			fake.reply("run", `{"block_ids":["block:A"]}`, 0)
			fake.reply("wait", `{"exit_code":1}`, 1)
			if mode != "notify error" {
				fake.reply("block-rename", "", 1)
			}
			if mode == "lost reply" {
				if err := os.WriteFile(filepath.Join(fake.dir, "rename-lost"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			var log syncBuf
			opts := Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate, Logger: &log}
			if mode == "notify error" {
				opts.NotifyURL = "://invalid"
			}
			s := rexServer(t, fake, opts)
			var reports atomic.Int32
			notify := s.children.notify
			s.children.notify = func(id string, err error) {
				reports.Add(1)
				if notify != nil {
					notify(id, err)
				}
			}
			if mode == "manually closed" {
				onExit := s.children.onExit
				s.children.onExit = func(id string, at time.Time, err error, tail string) {
					onExit(id, at, err, tail)
					if err := os.Remove(filepath.Join(fake.dir, "blocks", "block:A")); err != nil {
						panic(err)
					}
				}
			}
			s.heartbeatTick()
			if mode == "retry" || mode == "shutdown" {
				waitFor(t, "acknowledgement retry", func() bool { return fake.count("block rename") >= 2 })
				if current, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, io.Discard); err == nil || current != nil {
					t.Fatal("unacknowledged exit allowed replacement")
				}
				if mode == "retry" {
					fake.reply("block-rename", "", 0)
				} else {
					testRex(s.children).cancel()
				}
			}
			waitRexWorkers(t, testRex(s.children))
			if reports.Load() != 1 || !slices.Equal(gate.cleanList(), []bool{false}) || s.heartbeat.fails["alpha"] != 1 || s.heartbeat.skip["alpha"] != 1 {
				t.Fatalf("acknowledgement replayed completion: reports %d, clean %v, fails %v, skips %v", reports.Load(), gate.cleanList(), s.heartbeat.fails, s.heartbeat.skip)
			}
			want := "done:heartbeat:alpha"
			if mode == "shutdown" {
				want = "heartbeat:alpha"
			}
			if mode == "manually closed" {
				want = ""
			}
			if got := fake.blockLabel("block:A"); got != want {
				t.Fatalf("label %q; want %q", got, want)
			}
			if mode == "lost reply" && fake.count("block rename") != 1 {
				t.Fatal("lost reply was not resolved by readback")
			}
			if mode == "notify error" && !strings.Contains(log.String(), "notify build request:") {
				t.Fatalf("notification did not attempt and fail: %q", log.String())
			}
			if mode == "shutdown" {
				calls := len(fake.calls())
				if _, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, io.Discard); err == nil {
					t.Fatal("cancelled host allowed replacement")
				}
				if len(fake.calls()) != calls {
					t.Fatal("acknowledgement retry continued after join")
				}
			}
			if fake.called("block close") {
				t.Fatal("failed or absent block was closed during retirement")
			}
		})
	}
}

func TestRexRetainedCleanupScope(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "close succeeds"
		if lostReply {
			name = "closed despite error"
		}
		t.Run(name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.trackBlocks(
				rexBlock{ID: "block:A1", Label: "done:heartbeat:alpha"},
				rexBlock{ID: "block:A2", Label: "done:heartbeat:alpha"},
				rexBlock{ID: "block:OTHER", Label: "done:heartbeat:alpha-other"},
				rexBlock{ID: "block:LIVE", Label: "heartbeat:beta"},
				rexBlock{ID: "block:B", Label: "done:heartbeat:beta"},
			)
			fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
			fake.hold("wait")
			if lostReply {
				fake.reply("block-close", "", 1)
				if err := os.WriteFile(filepath.Join(fake.dir, "close-lost"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true"})
			if _, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, io.Discard); err != nil {
				t.Fatal(err)
			}
			if fake.blockLabel("block:A1") != "" || fake.blockLabel("block:A2") != "" || fake.count("block close ") != 2 {
				t.Fatal("cleanup did not close all project results")
			}
			for _, block := range []string{"block:OTHER", "block:LIVE", "block:B"} {
				if fake.blockLabel(block) == "" {
					t.Fatalf("cleanup closed %s", block)
				}
			}
			calls := fake.calls()
			lastClose, run := -1, -1
			for i, c := range calls {
				if strings.HasPrefix(c, "block close ") {
					lastClose = i
				}
				if strings.HasPrefix(c, "run ") {
					run = i
				}
			}
			if lastClose < 0 || run <= lastClose {
				t.Fatalf("launch overtook cleanup: %q", calls)
			}
			testRex(s.children).cancel()
			waitRexWorkers(t, testRex(s.children))
		})
	}
}

func TestRexRetainedCleanupRequiresReadableListing(t *testing.T) {
	for _, tc := range []struct {
		name, blocks, sessions string
		status                 int
	}{
		{"present unreadable session", "", `{"sessions":[{"session_id":"session:S"}]}`, 1},
		{"unreachable session", "", "", 1},
		{"malformed blocks", "{", `{"sessions":[{"session_id":"session:S"}]}`, 0},
		{"missing blocks", `{}`, `{"sessions":[{"session_id":"session:S"}]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", tc.blocks, tc.status)
			sessionStatus := 0
			if tc.sessions == "" {
				sessionStatus = 1
			}
			fake.reply("ls", tc.sessions, sessionStatus)
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true"})
			if c, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, io.Discard); c != nil || err == nil {
				t.Fatal("uncertain listing allowed launch")
			}
			if fake.called("run ") || fake.called("new ") || fake.called("block close ") {
				t.Fatalf("uncertain listing mutated Rex: %q", fake.calls())
			}
		})
	}
}

func TestRexLegacyExitAcknowledgement(t *testing.T) {
	fake := newFakeRex(t)
	fake.trackBlocks(rexBlock{ID: "block:LEGACY", Label: "heartbeat:alpha"})
	fake.reply("wait", `{"exit_code":1}`, 1)
	gate := &fakeHeartbeat{}
	root := t.TempDir()
	s := rexServer(t, fake, Options{Root: root, Heartbeat: gate})
	if err := testRex(s.children).adopt(); err != nil {
		t.Fatal(err)
	}
	waitRexWorkers(t, testRex(s.children))
	if fake.blockLabel("block:LEGACY") != "done:heartbeat:alpha" || s.heartbeat.fails["alpha"] != 1 || len(gate.sweptList()) != 0 {
		t.Fatal("legacy failure was not acknowledged once without advancing cursors")
	}
	testRex(s.children).cancel()
	fresh := rexServer(t, fake, Options{Root: root, Heartbeat: gate})
	if err := testRex(fresh.children).adopt(); err != nil {
		t.Fatal(err)
	}
	waitRexWorkers(t, testRex(fresh.children))
	if fake.count("wait ") != 1 || len(fresh.children.all) != 0 || len(fresh.heartbeat.fails) != 0 {
		t.Fatal("legacy acknowledgement replayed on next startup")
	}
}
