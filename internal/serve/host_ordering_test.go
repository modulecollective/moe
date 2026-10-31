package serve

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestHeartbeatReplacementWaitsForSwept(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "rex"
		}
		t.Run(name, func(t *testing.T) {
			gate := &orderingHeartbeat{fakeHeartbeat: fakeHeartbeat{due: []string{"alpha"}}, entered: make(chan struct{}), release: make(chan struct{})}
			root := t.TempDir()
			var s *Server
			if remote {
				fake := newFakeRex(t)
				fake.reply("run", `{"block_ids":["block:A"]}`, 0)
				fake.reply("wait", `{"exit_code":0}`, 0)
				s = rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			} else {
				s = newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			}
			release := sync.OnceFunc(func() { close(gate.release) })
			t.Cleanup(release)
			s.heartbeatTick()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("Swept never entered")
			}
			old, _ := s.children.get("heartbeat:alpha")
			if err := os.WriteFile(sweepRunPath(root, "alpha"), []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			s.heartbeatTick()
			if current, _ := s.children.get("heartbeat:alpha"); current != old {
				t.Fatal("replaced child before Swept returned")
			}
			if body, _ := os.ReadFile(sweepRunPath(root, "alpha")); string(body) != "sentinel" {
				t.Fatal("replacement cleared emit file before Swept returned")
			}
			release()
		})
	}
}

func TestRexRecordedIdentitySurvivesDiscoveryOutage(t *testing.T) {
	fake := rexStartupFake(t)
	fake.reply("whoami", "", 1)
	root := t.TempDir()
	seedStartupRexSession(t, root)
	moe, argv := argvRecorder(t, 0)
	s := newTestServer(t, Options{Root: root, MoeBin: moe, Heartbeat: &fakeHeartbeat{due: []string{"alpha"}}})
	s.selectHost()
	s.heartbeatTick()
	if _, ok := s.children.get("heartbeat:alpha"); ok || argv() != "" {
		t.Fatal("discovery outage dispatched beside recorded Rex work")
	}
	if fake.called("new ") || fake.called("run ") {
		t.Fatal("discovery outage launched Rex work")
	}
}

type orderingHeartbeat struct {
	fakeHeartbeat
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *orderingHeartbeat) Swept(project string, clean bool) {
	h.once.Do(func() { close(h.entered); <-h.release })
	h.fakeHeartbeat.Swept(project, clean)
}
