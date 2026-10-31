package serve

import (
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// All producers must have returned before waiting. In particular, a test
// that drives ticks in another goroutine must join that goroutine first.
func waitRexWorkers(t *testing.T, h *rexHost) {
	t.Helper()
	done := rexWorkersDone(h)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Rex workers did not stop")
	}
}

func rexWorkersDone(h *rexHost) <-chan struct{} {
	done := make(chan struct{})
	go func() { h.workers.Wait(); close(done) }()
	return done
}

func TestRexWorkerJoinIncludesFinalClose(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	fake.reply("capture", "final screen", 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
	fake.hold("block-close")
	// Release non-cancellable calls even when an assertion fails, before
	// the server cleanup joins workers and removes their fixtures.
	t.Cleanup(func() { fake.release("block-close") })
	exited := make(chan struct{})
	onExit := s.children.onExit
	s.children.onExit = func(id string, at time.Time, err error, tail string) {
		onExit(id, at, err, tail)
		close(exited)
	}

	s.heartbeatTick()
	waitFor(t, "held final close", func() bool { return fake.called("block close block:A") })
	c, _ := s.children.get("heartbeat:alpha")
	select {
	case <-c.done:
	default:
		t.Fatal("final close started before child completion")
	}
	select {
	case <-exited:
	default:
		t.Fatal("final close started before exit hooks finished")
	}
	h := testRex(s.children)
	h.cancel()
	joined := rexWorkersDone(h)
	select {
	case <-joined:
		t.Fatal("join returned while final close was held")
	case <-time.After(50 * time.Millisecond):
	}
	fake.release("block-close")
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("join did not return after final close was released")
	}
	if c.tail() != "final screen\n" || !slices.Equal(gate.cleanList(), []bool{true}) {
		t.Fatalf("completion lost: tail %q, clean %v", c.tail(), gate.cleanList())
	}
	if calls := fake.calls(); len(calls) != 6 || calls[3] != "capture -b block:A --trim" || calls[4] != "block rename -s session:S block:A done:heartbeat:alpha" || calls[5] != "block close block:A" {
		t.Fatalf("unexpected completed calls: %q", calls)
	}
}

type heldSweptHeartbeat struct {
	fakeHeartbeat
	entered chan struct{}
	release chan struct{}
}

func (h *heldSweptHeartbeat) Swept(project string, clean bool) {
	close(h.entered)
	<-h.release
	h.fakeHeartbeat.Swept(project, clean)
}

func TestRexWorkerJoinIncludesHeartbeatCompletion(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:A"]}`, 0)
	fake.reply("wait", `{"exit_code":1}`, 1)
	gate := &heldSweptHeartbeat{
		fakeHeartbeat: fakeHeartbeat{due: []string{"alpha"}},
		entered:       make(chan struct{}), release: make(chan struct{}),
	}
	s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
	release := sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	s.heartbeatTick()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Swept did not start")
	}
	h := testRex(s.children)
	h.cancel()
	joined := rexWorkersDone(h)
	select {
	case <-joined:
		t.Fatal("join returned while Swept was held")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("join did not return after Swept was released")
	}
	if !slices.Equal(gate.cleanList(), []bool{false}) || s.heartbeat.fails["alpha"] != 1 || s.heartbeat.skip["alpha"] != 1 {
		t.Fatalf("completion lost: clean %v, failures %v, skips %v", gate.cleanList(), s.heartbeat.fails, s.heartbeat.skip)
	}
	if fake.called("block close") {
		t.Fatal("failed block closed")
	}
}

func TestRexWorkerCancellationLeavesLiveJob(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		name := "dispatched"
		if adopted {
			name = "adopted"
		}
		t.Run(name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("run", `{"block_ids":["block:A"]}`, 0)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"}]}`, 0)
			fake.hold("wait")
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
			if adopted {
				if err := testRex(s.children).adopt(); err != nil {
					t.Fatal(err)
				}
			} else {
				s.heartbeatTick()
			}
			// Dispatch clears a previous emit file; write the sentinel after it.
			path := sweepRunPath(s.opts.Root, "alpha")
			if err := os.WriteFile(path, []byte("sentinel\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "live Rex wait", func() bool { return fake.called("wait --json block:A") })
			before := fake.calls()
			h := testRex(s.children)
			h.cancel()
			waitRexWorkers(t, h)
			c, _ := s.children.get("heartbeat:alpha")
			select {
			case <-c.done:
				t.Fatal("host cancellation fabricated a remote exit")
			default:
			}
			if len(gate.sweptList()) != 0 || len(s.heartbeat.fails) != 0 || len(s.heartbeat.skip) != 0 {
				t.Fatalf("live job acknowledged: swept %v, failures %v, skips %v", gate.sweptList(), s.heartbeat.fails, s.heartbeat.skip)
			}
			if body, err := os.ReadFile(path); err != nil || string(body) != "sentinel\n" {
				t.Fatalf("live emit file changed: %q, %v", body, err)
			}
			if !slices.Equal(before, fake.calls()) {
				t.Fatalf("cancellation made remote calls: before %q, after %q", before, fake.calls())
			}
		})
	}
}

func TestRexHeartbeatPublishedExitWinsCancellation(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(map[bool]string{false: "dispatched", true: "adopted"}[adopted], func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("wait", `{"exit_code":1}`, 1)
			gate := &fakeHeartbeat{}
			s := rexServer(t, fake, Options{Root: t.TempDir(), Heartbeat: gate})
			entered, unblock := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(unblock) })
			t.Cleanup(release)
			original := s.children.onExit
			s.children.onExit = func(id string, at time.Time, err error, tail string) {
				close(entered)
				<-unblock
				original(id, at, err, tail)
			}
			c := testRex(s.children).register("heartbeat:alpha", "block:A", adopted, io.Discard)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("exit hook never entered")
			}
			if exited, _ := c.snapshot(); !exited {
				t.Fatal("exit not published")
			}
			testRex(s.children).cancel()
			release()
			waitRexWorkers(t, testRex(s.children))
			want := []bool(nil)
			if !adopted {
				want = []bool{false}
			}
			if !slices.Equal(gate.cleanList(), want) || s.heartbeat.fails["alpha"] != 1 || s.heartbeat.skip["alpha"] != 1 || !c.isSettled() {
				t.Fatal("published exit lost on cancellation")
			}
		})
	}
}
