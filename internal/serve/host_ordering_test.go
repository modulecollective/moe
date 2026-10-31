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
			select {
			case <-old.settled:
			case <-time.After(5 * time.Second):
				t.Fatal("Swept completion never settled")
			}
			s.heartbeatTick()
			next, _ := s.children.get("heartbeat:alpha")
			if next == old {
				t.Fatal("settled child held replacement")
			}
			select {
			case <-next.settled:
			case <-time.After(5 * time.Second):
				t.Fatal("replacement never settled")
			}
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

func TestHeartbeatReplacementWaitsForHooks(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, hook := range []string{"notify", "exit"} {
			name := "local/" + hook
			if remote {
				name = "rex/" + hook
			}
			t.Run(name, func(t *testing.T) {
				gate := &fakeHeartbeat{due: []string{"alpha"}}
				var s *Server
				if remote {
					fake := newFakeRex(t)
					fake.reply("run", `{"block_ids":["block:A"]}`, 0)
					fake.reply("wait", `{"exit_code":0}`, 0)
					s = rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
				} else {
					s = newTestServer(t, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
				}
				entered, unblock := make(chan struct{}), make(chan struct{})
				release := sync.OnceFunc(func() { close(unblock) })
				t.Cleanup(release)
				var once sync.Once
				hold := func() { once.Do(func() { close(entered); <-unblock }) }
				if hook == "notify" {
					s.children.notify = func(string, error) { hold() }
				} else {
					exit := s.children.onExit
					s.children.onExit = func(id string, at time.Time, err error, tail string) { hold(); exit(id, at, err, tail) }
				}
				s.heartbeatTick()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("hook never entered")
				}
				old, _ := s.children.get("heartbeat:alpha")
				if exited, _ := old.snapshot(); !exited {
					t.Fatal("hook preceded exit publication")
				}
				path := sweepRunPath(s.opts.Root, "alpha")
				if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				s.heartbeatTick()
				if current, _ := s.children.get("heartbeat:alpha"); current != old {
					t.Fatal("replacement overtook hook")
				}
				if body, _ := os.ReadFile(path); string(body) != "sentinel" {
					t.Fatal("held hook lost emit file")
				}
				if len(gate.sweptList()) != 0 {
					t.Fatal("bookkeeping overtook hook")
				}
				if c, err := s.children.spawn("heartbeat:alpha", "/bin/true", nil, s.opts.Root, nil); c != nil || err == nil {
					t.Fatal("registry admitted replacement before hook")
				}
				release()
				select {
				case <-old.settled:
				case <-time.After(5 * time.Second):
					t.Fatal("child never settled")
				}
				s.heartbeatTick()
				next, _ := s.children.get("heartbeat:alpha")
				if next == old {
					t.Fatal("settled child held replacement")
				}
				select {
				case <-next.settled:
				case <-time.After(5 * time.Second):
					t.Fatal("replacement never settled")
				}
				if got := gate.sweptList(); len(got) != 2 {
					t.Fatalf("completion count = %v", got)
				}
			})
		}
	}
}

func TestImmediateExitWaitsForStartRegistration(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "rex"}[remote], func(t *testing.T) {
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			var s *Server
			var fake *fakeRex
			if remote {
				fake = newFakeRex(t)
				fake.reply("run", `{"block_ids":["block:A"]}`, 0)
				fake.reply("wait", `{"exit_code":0}`, 0)
				s = rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
			} else {
				s = newTestServer(t, Options{Root: t.TempDir(), MoeBin: "/bin/true", Heartbeat: gate})
			}
			entered, unblock := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(unblock) })
			t.Cleanup(release)
			original := s.children.onSpawn
			s.children.onSpawn = func(id string, at time.Time, detail string) { close(entered); <-unblock; original(id, at, detail) }
			tickDone := make(chan struct{})
			go func() { s.heartbeatTick(); close(tickDone) }()
			<-entered
			c, _ := s.children.get("heartbeat:alpha")
			if exited, _ := c.snapshot(); exited || len(gate.sweptList()) != 0 {
				t.Fatal("observer overtook start callback")
			}
			if remote && fake.called("wait ") {
				t.Fatal("remote observer started before registration returned")
			}
			release()
			<-tickDone
			select {
			case <-c.settled:
			case <-time.After(5 * time.Second):
				t.Fatal("immediate exit did not settle")
			}
			s.activity.mu.Lock()
			row := *s.activity.projects["alpha"]
			s.activity.mu.Unlock()
			if !row.started.IsZero() || row.sweptAt.IsZero() || !row.clean || len(gate.sweptList()) != 1 {
				t.Fatal("immediate exit lost start or completion")
			}
		})
	}
}
