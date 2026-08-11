package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHeartbeat is the cli-side gate, stubbed. due is what each pass
// returns; swept records the tip-cursor callbacks so a test can assert
// the ticker closes the loop.
type fakeHeartbeat struct {
	mu     sync.Mutex
	due    []string
	swept  []string
	passes int
}

func (f *fakeHeartbeat) Due(tick time.Duration, log io.Writer) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passes++
	return append([]string(nil), f.due...)
}

func (f *fakeHeartbeat) Swept(projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept = append(f.swept, projectID)
}

func (f *fakeHeartbeat) sweptList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.swept...)
}

func (f *fakeHeartbeat) passCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passes
}

// argvRecorder writes a script that appends its arguments to a file and
// exits with the given status, and returns the script path plus a reader
// for what it captured. It stands in for the `moe` binary: the point of
// spawning a child at all is that the binary on disk is what runs, so
// the test asserts on the argv that reaches it.
func argvRecorder(t *testing.T, exitCode int) (bin string, argv func() string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "argv")
	bin = filepath.Join(dir, "fake-moe")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + out + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() string {
		body, err := os.ReadFile(out)
		if err != nil {
			return ""
		}
		return string(body)
	}
}

// settle polls until cond holds, up to a generous budget. The
// heartbeat's children exit asynchronously, so every assertion about an
// outcome has to wait for the watcher goroutine rather than assume it
// has run. Reports whether cond ever held, so callers on a goroutine
// (where t.Fatalf is illegal) can use it too.
func settle(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !settle(cond) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// syncBuf is a logger a test can read while the ticker's watcher
// goroutines are still writing to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHeartbeatSpawnsTheDynamicSweep pins the one thing the ticker
// actually does: run `moe pulse new --dynamic <project>` as a child. The
// flag is the whole consent story — without it the sweep would groom and
// park, and the loop would still need a human keystroke.
func TestHeartbeatSpawnsTheDynamicSweep(t *testing.T) {
	bin, argv := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	if got := argv(); !strings.Contains(got, "pulse new --dynamic alpha") {
		t.Errorf("child argv = %q, want the dynamic sweep", got)
	}
}

// TestHeartbeatBacksOffAfterFailures: a night of exhausted plan limits, a
// dead network and a broken `gh` all read the same here, and all get the
// same answer — the project's tick backs off so a dead vendor can't mint
// a pile of failed sweeps by morning.
func TestHeartbeatBacksOffAfterFailures(t *testing.T) {
	bin, _ := argvRecorder(t, 1)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	var log syncBuf
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate, Logger: &log,
	})

	s.heartbeatTick()
	waitFor(t, "the failed sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	// One failure earns one skipped tick.
	s.heartbeatTick()
	if got := len(gate.sweptList()); got != 1 {
		t.Fatalf("swept %d times, want the second tick skipped by the cool-off", got)
	}
	// The cool-off is spent, so the tick after it tries again.
	s.heartbeatTick()
	waitFor(t, "the retry", func() bool { return len(gate.sweptList()) == 2 })
	if !strings.Contains(log.String(), "cooling off") {
		t.Errorf("log = %q, want the cool-off named", log.String())
	}
}

// TestHeartbeatResetsBackoffOnACleanSweep: the cool-off is a reaction to
// a run of failures, not a penalty that accumulates. One clean sweep and
// the cadence is back.
func TestHeartbeatResetsBackoffOnACleanSweep(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/true"})
	s.heartbeat.record("alpha", true)
	s.heartbeat.record("alpha", true)
	if cooling, _, _ := s.heartbeat.cooling("alpha"); !cooling {
		t.Fatal("two failures should have earned a cool-off")
	}
	s.heartbeat.record("alpha", false)
	if cooling, _, _ := s.heartbeat.cooling("alpha"); cooling {
		t.Error("a clean sweep must clear the cool-off outright")
	}
}

// TestHeartbeatBackoffIsCapped: exponential, but bounded. Six skipped
// ticks is two hours at the default cadence — long enough to stop a
// pile-up, short enough that the loop comes back on its own.
func TestHeartbeatBackoffIsCapped(t *testing.T) {
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: "/bin/true"})
	for range 20 {
		if skip := s.heartbeat.record("alpha", true); skip > heartbeatBackoffCap {
			t.Fatalf("skip = %d, want no more than the cap %d", skip, heartbeatBackoffCap)
		}
	}
}

// TestHeartbeatSingleFlightsItsOwnChild: a sweep that kicked a ride can
// outlive many ticks, and the ride's own tail pulses own growth while it
// does. A second child for the same project would be a second sweep of a
// board that is already moving.
func TestHeartbeatSingleFlightsItsOwnChild(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "slow-moe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.children.shutdown(ctx, io.Discard)
	})

	s.heartbeatTick()
	s.heartbeatTick()

	c, ok := s.children.get(heartbeatChildPrefix + "alpha")
	if !ok {
		t.Fatal("no heartbeat child registered")
	}
	if exited, _ := c.snapshot(); exited {
		t.Fatal("the first child should still be running")
	}
	if len(gate.sweptList()) != 0 {
		t.Errorf("swept = %v, want nothing finished yet", gate.sweptList())
	}
}

// TestHeartbeatChildIdCannotCollideWithARun: the registry is keyed by
// run id, and a heartbeat child rides it to inherit shutdown and notify.
// The colon is what keeps the two namespaces apart — run ids are
// "<project>/<slug>" and both halves are slugs.
func TestHeartbeatChildIdCannotCollideWithARun(t *testing.T) {
	id := heartbeatChildPrefix + "alpha"
	if !strings.Contains(id, ":") || strings.Contains(id, "/") {
		t.Errorf("heartbeat child id %q must be unspellable as a run id", id)
	}
	if got := heartbeatProject(id); got != "alpha" {
		t.Errorf("heartbeatProject(%q) = %q, want alpha", id, got)
	}
	if got := heartbeatProject("alpha/fix-it"); got != "" {
		t.Errorf("heartbeatProject on a run id = %q, want empty", got)
	}
}

// TestUnarmedServeNeverTicks: an unarmed serve is today's MoE byte for
// byte. That is what makes "the fallback is the status quo" true, and it
// is the retraction the operator has — stop the process.
func TestUnarmedServeNeverTicks(t *testing.T) {
	bin, argv := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = 20 * time.Minute })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.ListenAndServe(ctx); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if got := argv(); got != "" {
		t.Errorf("an unarmed serve spawned %q", got)
	}
	if n := gate.passCount(); n != 0 {
		t.Errorf("an unarmed serve consulted the gate %d times", n)
	}
}

// TestArmedServeTicks is the other half of the arming switch: running
// `moe serve --dynamic` *is* the standing consent, so the clock starts
// with the listener.
func TestArmedServeTicks(t *testing.T) {
	bin, argv := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = 20 * time.Minute })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		settle(func() bool { return len(gate.sweptList()) > 0 })
		cancel()
	}()
	if err := s.ListenAndServe(ctx); err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if got := argv(); !strings.Contains(got, "pulse new --dynamic alpha") {
		t.Errorf("armed serve spawned %q, want the dynamic sweep", got)
	}
}

// TestNotifyDistinguishesASweepFromARun: the two want different
// reactions on a phone. A run that died is a thing to look at now; a
// sweep that died is the tell that the vendor is down and the loop has
// started cooling off.
func TestNotifyDistinguishesASweepFromARun(t *testing.T) {
	gotBody := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
	}))
	defer srv.Close()
	notify := makeNotifier(srv.URL, io.Discard)

	for _, tc := range []struct {
		id    string
		wants []string
	}{
		{id: "alpha/foo", wants: []string{`"kind":"run"`}},
		{id: heartbeatChildPrefix + "alpha", wants: []string{`"kind":"heartbeat"`, `"project":"alpha"`}},
	} {
		notify(tc.id, errors.New("boom"))
		select {
		case body := <-gotBody:
			for _, want := range tc.wants {
				if !strings.Contains(string(body), want) {
					t.Errorf("payload for %s missing %s: %s", tc.id, want, string(body))
				}
			}
			if !strings.Contains(string(body), `"ok":false`) {
				t.Errorf("payload for %s should report the failure: %s", tc.id, string(body))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("notifier never POSTed for %s", tc.id)
		}
	}
}
