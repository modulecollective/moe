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

// fakeHeartbeat is the cli-side gate, stubbed. due is the projects each
// pass says to sweep and quiet the ones it stands down on; swept records
// the cursor callbacks so a test can assert the ticker closes the loop,
// and cleans the flag each one carried.
type fakeHeartbeat struct {
	mu     sync.Mutex
	due    []string
	quiet  []string
	swept  []string
	cleans []bool
	passes int
}

func (f *fakeHeartbeat) Due(tick time.Duration, log io.Writer) []HeartbeatDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passes++
	var out []HeartbeatDecision
	for _, p := range f.due {
		out = append(out, HeartbeatDecision{Project: p, Sweep: true, Reason: "the journal moved"})
	}
	for _, p := range f.quiet {
		out = append(out, HeartbeatDecision{Project: p, Reason: "a sweep already surveyed the current tip"})
	}
	return out
}

func (f *fakeHeartbeat) Swept(projectID string, clean bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept = append(f.swept, projectID)
	f.cleans = append(f.cleans, clean)
}

func (f *fakeHeartbeat) sweptList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.swept...)
}

func (f *fakeHeartbeat) cleanList() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.cleans...)
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
// actually does: run `moe pulse new --dynamic --heartbeat <project>` as
// a child. --dynamic is the whole consent story — without it the sweep
// would groom and park, and the loop would still need a human keystroke.
// --heartbeat is who: the command is otherwise spelled exactly like the
// one an operator types, and the per-project mode caps the clock rather
// than the operator, so without it the child cannot tell them apart.
func TestHeartbeatSpawnsTheDynamicSweep(t *testing.T) {
	bin, argv := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	got := argv()
	if !strings.Contains(got, "pulse new --dynamic --heartbeat ") ||
		!strings.HasSuffix(strings.TrimSpace(got), " alpha") {
		t.Errorf("child argv = %q, want the clock-marked dynamic sweep", got)
	}
}

// TestHeartbeatReportsWhetherTheSweepWasClean: the exit code is the
// only thing that separates "a survey looked at this board and made its
// calls" from "a survey died partway", and the gate needs the
// difference — a clean sweep stops the parked-work leg re-offering the
// same thread, a dead one must not.
func TestHeartbeatReportsWhetherTheSweepWasClean(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode int
		want     bool
	}{
		{name: "clean", exitCode: 0, want: true},
		{name: "failed", exitCode: 1, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := argvRecorder(t, tc.exitCode)
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			s := newTestServer(t, Options{
				Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
			})

			s.heartbeatTick()
			waitFor(t, "the sweep to be recorded", func() bool { return len(gate.cleanList()) == 1 })

			if got := gate.cleanList()[0]; got != tc.want {
				t.Errorf("Swept clean = %v for a child exiting %d, want %v", got, tc.exitCode, tc.want)
			}
		})
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
		if _, skip := s.heartbeat.record("alpha", true); skip > heartbeatBackoffCap {
			t.Fatalf("skip = %d, want no more than the cap %d", skip, heartbeatBackoffCap)
		}
	}
}

// TestHeartbeatSingleFlightsItsOwnChild: a sweep that kicked a ride can
// outlive many ticks, and the sweep child itself walks the board while
// it lives. A second child for the same project would be a second sweep
// of a board that is already moving.
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
	s := newUnarmedTestServer(t, Options{
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
	if got := argv(); !strings.Contains(got, "pulse new --dynamic ") || !strings.HasSuffix(strings.TrimSpace(got), " alpha") {
		t.Errorf("armed serve spawned %q, want the dynamic sweep", got)
	}
}

// blockingHeartbeat is a gate that parks inside Due until the test lets
// it go — the stand-in for a tick still mid-flight when the operator
// stops the process.
type blockingHeartbeat struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingHeartbeat) Due(tick time.Duration, log io.Writer) []HeartbeatDecision {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

func (b *blockingHeartbeat) Swept(projectID string, clean bool) {}

// TestArmedServeJoinsTheHeartbeat: shutdown waits for a tick in flight
// rather than walking out on it. Two things ride on the join — a tick
// spawns into the same child registry children.shutdown drains, so a
// late spawn would outlive the wind-down, and a goroutine still reading
// heartbeatInterval after ListenAndServe returns races the test that
// shortened it (the flake this fixes).
func TestArmedServeJoinsTheHeartbeat(t *testing.T) {
	bin, _ := argvRecorder(t, 0)
	gate := &blockingHeartbeat{entered: make(chan struct{}, 1), release: make(chan struct{})}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = 20 * time.Minute })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	<-gate.entered
	cancel()

	select {
	case <-done:
		t.Fatal("ListenAndServe returned with a heartbeat tick still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe never returned after the tick finished")
	}
}

// TestHeartbeatTickRecordsTheWholeVerdictSet: the ticker only spawns for
// the sweeps, but the record has to carry the stand-downs too — those are
// the trace that used to go to stderr and die there.
func TestHeartbeatTickRecordsTheWholeVerdictSet(t *testing.T) {
	bin, _ := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}, quiet: []string{"beta"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	// Off the record, not the panel: /serve summarises the trivial away,
	// and the claim here is that the record still holds every verdict for
	// the CLI snapshot and for anything that later wants to read one.
	states := map[string]string{}
	for _, p := range s.activity.snapshot(time.Now()).Projects {
		states[p.Project] = p.Decision
	}
	if len(states) != 2 {
		t.Fatalf("recorded projects = %v, want the swept and the quiet one", states)
	}
	if states["beta"] != "a sweep already surveyed the current tip" {
		t.Errorf("beta reason = %q, want the gate's stand-down words", states["beta"])
	}
	if states["alpha"] != "the journal moved" {
		t.Errorf("alpha reason = %q, want the gate's sweep words", states["alpha"])
	}
}

// TestHeartbeatTickWritesTheStateFile: `moe dash` has no channel to a
// running serve but the file, so a tick that doesn't write it is a tick
// the terminal can't see.
func TestHeartbeatTickWritesTheStateFile(t *testing.T) {
	bin, _ := argvRecorder(t, 0)
	root := t.TempDir()
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: bin, Heartbeat: gate,
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	waitFor(t, "the state file to name the sweep", func() bool {
		snap, ok, err := ReadActivitySnapshot(root)
		if !ok || err != nil {
			return false
		}
		return len(snap.Projects) == 1 && snap.Projects[0].Decision == "the journal moved" &&
			!snap.Projects[0].SweptAt.IsZero() && snap.Projects[0].SweepClean
	})
}

// TestHeartbeatRecordsTheCoolOffAsWhatHappened: the gate said sweep and
// the ticker's backoff said no. The record has to say the second, or the
// dash reads "sweeping" for a project standing still.
func TestHeartbeatRecordsTheCoolOffAsWhatHappened(t *testing.T) {
	bin, _ := argvRecorder(t, 1)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: bin, Heartbeat: gate,
	})

	state := func() serveProjectVM {
		t.Helper()
		vm := s.activity.panel(time.Now())
		if len(vm.Projects) != 1 {
			t.Fatalf("panel projects = %+v, want alpha", vm.Projects)
		}
		return vm.Projects[0]
	}

	// One failure earns one skipped tick, and the tick after it spends the
	// whole cool-off — so the project is back to plain "failed".
	s.heartbeatTick()
	waitFor(t, "the failed sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	s.heartbeatTick()
	if got := state(); got.State != "failed" || !got.Failed {
		t.Errorf("project line = %+v after a spent cool-off, want a failed state", got)
	}

	// A second failure earns two, so the tick after it leaves one
	// outstanding — the state that has to read as "held back", not "about
	// to sweep".
	s.heartbeatTick()
	waitFor(t, "the second failed sweep", func() bool { return len(gate.sweptList()) == 2 })
	s.heartbeatTick()
	got := state()
	if got.State != "cooling" || !got.Failed {
		t.Fatalf("project line = %+v with a cool-off outstanding, want a cooling state", got)
	}
	if !strings.Contains(got.Detail, "1 tick left") {
		t.Errorf("cooling detail = %q, want the ticks left", got.Detail)
	}
}

// TestServeRemovesTheStateFileOnCleanShutdown is what makes a leftover
// file mean something: it names a pid, and a pid that is gone is a serve
// that crashed rather than one that stopped.
func TestServeRemovesTheStateFileOnCleanShutdown(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/true"})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ListenAndServe(ctx) }()

	waitFor(t, "the state file to appear on listen", func() bool {
		_, ok, _ := ReadActivitySnapshot(root)
		return ok
	})
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	if _, ok, _ := ReadActivitySnapshot(root); ok {
		t.Error("the state file survived a clean shutdown")
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

// emitRunRecorder writes a fake `moe` that honours --emit-run: it writes
// the given slug to the path serve passed, then exits with the given
// status. It stands in for a sweep child, whose real job — from serve's
// side — is exactly this: mint a run and name it.
func emitRunRecorder(t *testing.T, slug string, exitCode int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-moe")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--emit-run\" ]; then printf '%s\\n' " + slug + " > \"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestSweepExitEventLinksItsRun: "heartbeat:alpha exited cleanly" was
// every word serve had about a sweep. The run the sweep opened is what
// answers "and what did it do" — one click from the event, now that the
// child names it.
func TestSweepExitEventLinksItsRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "pulse-2026-04-01", "pulse")
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, Heartbeat: gate,
		MoeBin: emitRunRecorder(t, "pulse-2026-04-01", 0),
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	waitFor(t, "the exit event", func() bool {
		for _, ev := range s.activity.panel(time.Now()).Events {
			if ev.Kind == "exit" {
				return true
			}
		}
		return false
	})

	var exit serveEventVM
	for _, ev := range s.activity.panel(time.Now()).Events {
		if ev.Kind == "exit" {
			exit = ev
		}
	}
	if got, want := exit.RunURL, "/run/alpha/pulse-2026-04-01"; got != want {
		t.Errorf("exit event RunURL=%q, want %q", got, want)
	}
	// Serve owns the file: read once, then gone, so its presence never
	// outlives the sweep that wrote it.
	if _, err := os.Stat(sweepRunPath(root, "alpha")); !os.IsNotExist(err) {
		t.Errorf("emit file still there after the exit (err=%v), want it consumed", err)
	}
}

// TestFailedSweepRowLinksItsRun is the payoff: the run a dead sweep left
// open is exactly the thing the operator wants, and hunting the dash for
// a `pulse-*` slug whose date roughly matches was the whole complaint.
func TestFailedSweepRowLinksItsRun(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "pulse-2026-04-01", "pulse")
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, Heartbeat: gate,
		MoeBin: emitRunRecorder(t, "pulse-2026-04-01", 1),
	})

	s.heartbeatTick()
	waitFor(t, "the failed sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	// Failed and Run are written by two different goroutines reacting to
	// the same child exit, so waiting on one and asserting the other
	// races the gap between them. One predicate over one panel snapshot
	// covers both.
	waitFor(t, "the cool-off row to link the run the dead sweep left open", func() bool {
		for _, p := range s.activity.panel(time.Now()).Projects {
			if p.Project == "alpha" && p.Failed && p.Run == "pulse-2026-04-01" {
				return true
			}
		}
		return false
	})
}

// TestSweepLinkNeedsTheRunToExist: the emit file can name a run that
// never landed — a mint that failed after the write, a stale file whose
// run was since removed. A link to a run that isn't there is worse than
// the plain text /serve had before, so the run has to still be on disk
// at exit. (A Ctrl-C'd sweep is *not* this case: disposePulseRun closes
// its run rather than deleting it, so that link stands.)
func TestSweepLinkNeedsTheRunToExist(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, Heartbeat: gate,
		MoeBin: emitRunRecorder(t, "pulse-2026-04-01", 0),
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	// The gate is swept before the exit event exists, so assert on the
	// event's presence — otherwise a ring with no exit event at all reads
	// as "no bad link" and the test passes without testing anything.
	waitFor(t, "the exit event", func() bool {
		for _, ev := range s.activity.panel(time.Now()).Events {
			if ev.Kind == "exit" {
				return true
			}
		}
		return false
	})

	for _, ev := range s.activity.panel(time.Now()).Events {
		if ev.Kind == "exit" && ev.RunURL != "" {
			t.Errorf("exit event RunURL=%q, want none — the run it named is gone", ev.RunURL)
		}
	}
}

// TestStaleEmitFileIsClearedBeforeTheSpawn: a serve restarted mid-sweep
// leaves a file naming the old sweep's run. Absent is the only honest
// "nothing minted yet", so the next spawn clears it first — otherwise the
// new sweep would render as the old one's run.
func TestStaleEmitFileIsClearedBeforeTheSpawn(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "pulse-2026-04-01", "pulse")
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sweepRunPath(root, "alpha"), []byte("pulse-2026-04-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gate := &fakeHeartbeat{due: []string{"alpha"}}
	bin, _ := argvRecorder(t, 0) // a child that mints nothing
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, Heartbeat: gate, MoeBin: bin,
	})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	// The gate is swept before the exit event exists, so without this the
	// ring can still be empty when the loop runs and "no link" is
	// vacuously true.
	waitFor(t, "the exit event", func() bool {
		for _, ev := range s.activity.panel(time.Now()).Events {
			if ev.Kind == "exit" {
				return true
			}
		}
		return false
	})

	for _, ev := range s.activity.panel(time.Now()).Events {
		if ev.Kind == "exit" && ev.RunURL != "" {
			t.Errorf("exit event RunURL=%q, want none — that run belonged to an earlier sweep", ev.RunURL)
		}
	}
}

// TestNewSweepDropsTheLastSweepsRunFromTheRow is the emit file's clear
// applied to the row that renders it: a project the ticker sweeps again
// must stop naming the previous sweep's run, and it has to stop before
// the spawn, because the new child can name its own run the moment it
// starts. The child here never exits, so nothing but the pre-spawn clear
// can take the old run off the row.
func TestNewSweepDropsTheLastSweepsRunFromTheRow(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "alpha", "pulse-2026-04-01", "pulse")
	bin := filepath.Join(t.TempDir(), "slow-moe")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, Heartbeat: gate, MoeBin: bin,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.children.shutdown(ctx, io.Discard)
	})
	s.activity.recordSweepRun("alpha", "pulse-2026-04-01") // an earlier sweep's

	s.heartbeatTick()

	for _, p := range s.activity.panel(time.Now()).Projects {
		if p.Project == "alpha" && p.Run != "" {
			t.Errorf("sweeping row Run=%q, want none — that run belonged to the last sweep", p.Run)
		}
	}
}

// TestLiveSweepRowLinksMidFlight: the slug is on disk seconds after the
// spawn, but serve only hears about it at exit — and a sweep that kicked
// a ride can be "sweeping" for hours. Those are the ones worth peeking
// at, so the row reads the file rather than waiting.
func TestLiveSweepRowLinksMidFlight(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	a := newActivity(root, 4242, "127.0.0.1:4242", true, now)
	a.recordSweepStart("alpha", now.Add(-3*time.Minute))
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sweepRunPath(root, "alpha"), []byte("pulse-2026-04-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	vm := a.panel(now)
	if len(vm.Projects) != 1 || vm.Projects[0].State != "sweeping" {
		t.Fatalf("projects=%+v, want one sweeping row", vm.Projects)
	}
	if got := vm.Projects[0].Run; got != "pulse-2026-04-01" {
		t.Errorf("live row Run=%q, want the run the sweep is filling in", got)
	}
	// Cached: the next page load costs no read.
	if err := os.Remove(sweepRunPath(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	if got := a.panel(now).Projects[0].Run; got != "pulse-2026-04-01" {
		t.Errorf("second load Run=%q, want the cached slug", got)
	}
}

// TestSweepRunFileTakesOnlyASlug: a file boundary, and the value lands in
// an href. Shape-check it rather than trust whatever is on disk.
func TestSweepRunFileTakesOnlyASlug(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"../../etc/passwd", "a b", "", "Pulse-2026", "alpha/pulse-2026-04-01"} {
		if err := os.WriteFile(sweepRunPath(root, "alpha"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readSweepRun(root, "alpha"); got != "" {
			t.Errorf("readSweepRun(%q) = %q, want nothing — not a run slug", body, got)
		}
	}
}

// TestMissingRunDropsTheLazyCachedLink: panel() caches a live sweep's
// slug without checking the run exists — tolerable mid-flight only
// because the exit read is authoritative. When that read finds nothing
// behind the slug, the row must not keep wearing the cached link.
func TestMissingRunDropsTheLazyCachedLink(t *testing.T) {
	root := t.TempDir()
	seedProject(t, root, "alpha")
	s := newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root})
	now := time.Now()

	s.activity.recordSweepStart("alpha", now.Add(-3*time.Minute))
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The emit file names a run that is not on disk.
	if err := os.WriteFile(sweepRunPath(root, "alpha"), []byte("pulse-2026-04-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.activity.panel(now).Projects[0].Run; got != "pulse-2026-04-01" {
		t.Fatalf("live row Run=%q, want the lazily cached slug", got)
	}

	s.children.onExit(heartbeatChildPrefix+"alpha", now, errors.New("exit status 130"), "")
	s.activity.recordSweepEnd("alpha", now, false, 1, 0)

	if got := s.activity.panel(now).Projects[0].Run; got != "" {
		t.Errorf("row Run=%q after the exit read found no run, want the cached link dropped", got)
	}
}
