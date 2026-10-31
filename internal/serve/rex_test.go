package serve

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeRex is a stand-in `rex` on disk, scripted per subcommand. A call
// appends its argv to calls; then, keyed by the subcommand ("run",
// "wait", "block-close", …), it waits while <key>.hold exists, prints
// <key>.out if present and exits with <key>.status (default 0). That is
// enough to shape every reply the Rex host reads, and every argv it
// sends is on the record.
type fakeRex struct {
	t   *testing.T
	dir string
	bin string
}

func newFakeRex(t *testing.T) *fakeRex {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "rex")
	script := `#!/bin/sh
D=` + dir + `
printf '%s\n' "$*" >> "$D/calls"
shift 3
k=$1
[ "$1" = block ] && k="block-$2"
while [ -e "$D/$k.hold" ]; do sleep 0.02; done
[ -e "$D/$k.out" ] && cat "$D/$k.out"
s=0
[ -e "$D/$k.status" ] && s=$(cat "$D/$k.status")
exit $s
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeRex{t: t, dir: dir, bin: bin}
}

// reply sets what the subcommand key prints and its exit status.
func (f *fakeRex) reply(key, out string, status int) {
	f.t.Helper()
	path := filepath.Join(f.dir, key+".out")
	if out == "" {
		_ = os.Remove(path)
	} else if err := os.WriteFile(path, []byte(out+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, key+".status"), []byte(strconv.Itoa(status)), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// hold makes the subcommand key block until release.
func (f *fakeRex) hold(key string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, key+".hold"), nil, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeRex) release(key string) {
	_ = os.Remove(filepath.Join(f.dir, key+".hold"))
}

// calls returns every argv the fake has seen, one line each, with the
// global flags every call carries checked and stripped.
func (f *fakeRex) calls() []string {
	f.t.Helper()
	body, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, "--autostart=false --timeout 10s ")
		if !ok {
			f.t.Errorf("rex call %q lacks --autostart=false --timeout", line)
		}
		out = append(out, rest)
	}
	return out
}

// called reports whether some call starts with prefix.
func (f *fakeRex) called(prefix string) bool {
	for _, c := range f.calls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// rexServer is a test server whose children are hosted by fake, in a
// session that already exists.
func rexServer(t *testing.T, fake *fakeRex, opts Options) *Server {
	t.Helper()
	s := newTestServer(t, opts)
	h := newRexHost(fake.bin, opts.Root, opts.Logger)
	h.session = "session:S"
	t.Cleanup(h.cancel)
	s.children.rex = h
	return s
}

// TestRexLaunchArgvIsExact pins the one command that puts a sweep in Rex:
// the block is labelled with the child id (which is how a restarted serve
// finds it), kept open so its status and screen survive the exit, not
// focused, started in the root, and the sweep's own argv reaches moe
// whole behind an env prefix carrying the two variables serve adds.
func TestRexLaunchArgvIsExact(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"window_id":"window:W","block_ids":["block:B1"],"revision":2}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	moe, _ := argvRecorder(t, 0)
	root := t.TempDir()
	gate := &fakeHeartbeat{due: []string{"alpha"}, prChecks: `{"k":"v w"}`}
	s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: moe, Heartbeat: gate})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	want := "run --json --focus=false --keep-open -s session:S --label heartbeat:alpha --cwd " + root +
		" -- /usr/bin/env MOE_HOME=" + root + " MOE_SERVE_AGENT=1 " + moe +
		" pulse new --dynamic --heartbeat --emit-run " + sweepRunPath(root, "alpha") +
		` --pr-checks {"k":"v w"} alpha`
	if calls := fake.calls(); len(calls) == 0 || calls[0] != want {
		t.Errorf("first rex call = %q\nwant %q", calls, want)
	}
}

// TestRexExitReachesTheGate: the wait payload's exit code is the sweep's
// result, so a clean block reads as a clean sweep and a failed one as a
// failure — and only the clean one is consumed by closing its block.
func TestRexExitReachesTheGate(t *testing.T) {
	for _, tc := range []struct {
		code      int
		wantClean bool
	}{{0, true}, {1, false}} {
		fake := newFakeRex(t)
		fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
		fake.reply("wait", `{"exit_code":`+strconv.Itoa(tc.code)+`}`, tc.code)
		fake.reply("capture", "the last screen", 0)
		moe, _ := argvRecorder(t, 0)
		gate := &fakeHeartbeat{due: []string{"alpha"}}
		s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

		s.heartbeatTick()
		waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
		if got := gate.cleanList()[0]; got != tc.wantClean {
			t.Errorf("exit %d: Swept clean = %v, want %v", tc.code, got, tc.wantClean)
		}
		c, _ := s.children.get(heartbeatChildPrefix + "alpha")
		if got := c.tail(); !strings.Contains(got, "the last screen") {
			t.Errorf("exit %d: tail = %q, want the captured screen", tc.code, got)
		}
		if tc.wantClean {
			waitFor(t, "the clean block to close", func() bool { return fake.called("block close block:B1") })
		} else if fake.called("block close") {
			t.Errorf("exit %d: failed block was closed; it should be kept for the operator", tc.code)
		}
	}
}

// TestRexWaitWithoutPayloadReadsTheProcess: a wait that ends with no
// payload is the transport failing, not the program exiting. The block's
// process record is the fallback, and its last_exit is the answer.
func TestRexWaitWithoutPayloadReadsTheProcess(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
	fake.reply("wait", "", 1)
	fake.reply("block-call", `{"last_exit":{"exit_code":0}}`, 0)
	moe, _ := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	if !gate.cleanList()[0] {
		t.Error("Swept clean = false, want the process record's exit 0")
	}
	if !fake.called("block call -b block:B1 process") {
		t.Errorf("rex calls = %q, want a process read", fake.calls())
	}
}

// TestRexCaptureFailureKeepsTheExit: the tail is a courtesy; the exit is
// the result.
func TestRexCaptureFailureKeepsTheExit(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
	fake.reply("wait", `{"exit_code":1}`, 1)
	fake.reply("capture", "", 1)
	moe, _ := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
	if gate.cleanList()[0] {
		t.Error("Swept clean = true, want the known exit 1")
	}
}

// TestRexUnresolvedLaunch: a create that errors may still have created
// the block. A labelled block in the session is this launch; no match
// means nothing this tick — and above all nothing local.
func TestRexUnresolvedLaunch(t *testing.T) {
	t.Run("label found", func(t *testing.T) {
		fake := newFakeRex(t)
		fake.reply("run", "", 1)
		fake.reply("block-ls", `{"blocks":[{"block_id":"block:B9","label":"heartbeat:alpha"}]}`, 0)
		fake.reply("wait", `{"exit_code":0}`, 0)
		moe, argv := argvRecorder(t, 0)
		gate := &fakeHeartbeat{due: []string{"alpha"}}
		s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

		s.heartbeatTick()
		waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })
		if !fake.called("wait --json block:B9") {
			t.Errorf("rex calls = %q, want the found block watched", fake.calls())
		}
		if argv() != "" {
			t.Errorf("moe ran locally: %q", argv())
		}
	})
	t.Run("no match", func(t *testing.T) {
		fake := newFakeRex(t)
		fake.reply("run", "", 1)
		fake.reply("block-ls", `{"blocks":[{"block_id":"block:X","label":"shell"}]}`, 0)
		moe, argv := argvRecorder(t, 0)
		gate := &fakeHeartbeat{due: []string{"alpha"}}
		s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

		s.heartbeatTick()
		if _, ok := s.children.get(heartbeatChildPrefix + "alpha"); ok {
			t.Error("a child was registered for a launch nobody can find")
		}
		if fake.called("wait") {
			t.Errorf("rex calls = %q, want nothing watched", fake.calls())
		}
		time.Sleep(50 * time.Millisecond)
		if argv() != "" {
			t.Errorf("moe ran locally: %q", argv())
		}
	})
}

// TestRexRetention: a failed block stays for the operator to read, and
// the next launch of that project is what closes it — before launching,
// so the label is never on two blocks at once.
func TestRexRetention(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("run", `{"block_ids":["block:B1"]}`, 0)
	fake.reply("wait", `{"exit_code":1}`, 1)
	moe, _ := argvRecorder(t, 0)
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: t.TempDir(), MoeBin: moe, Heartbeat: gate})

	s.heartbeatTick()
	waitFor(t, "the first sweep", func() bool { return len(gate.sweptList()) == 1 })
	if fake.called("block close") {
		t.Fatal("failed block closed on exit")
	}

	fake.reply("run", `{"block_ids":["block:B2"]}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	s.heartbeat.record("alpha", false) // spend the cool-off the failure earned
	s.heartbeatTick()
	waitFor(t, "the second sweep", func() bool { return len(gate.sweptList()) == 2 })

	var order []string
	for _, c := range fake.calls() {
		if strings.HasPrefix(c, "block close") || strings.HasPrefix(c, "run ") {
			order = append(order, strings.Fields(c)[0]+" "+strings.Fields(c)[len(strings.Fields(c))-1])
		}
	}
	waitFor(t, "the clean block to close", func() bool { return fake.called("block close block:B2") })
	if len(order) < 3 || order[0] != "run alpha" || order[1] != "block block:B1" || order[2] != "run alpha" {
		t.Errorf("launch/close order = %q, want run, close B1, run", order)
	}
}

// TestRexFirstLaunchCreatesTheSession: with no session yet, the first
// job creates it, its block takes the label by rename, and the session
// is remembered for the next start.
func TestRexFirstLaunchCreatesTheSession(t *testing.T) {
	fake := newFakeRex(t)
	fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"window_id":"window:W","block_ids":["block:B1"]}]}`, 0)
	fake.reply("wait", `{"exit_code":0}`, 0)
	moe, _ := argvRecorder(t, 0)
	root := t.TempDir()
	gate := &fakeHeartbeat{due: []string{"alpha"}}
	s := rexServer(t, fake, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: moe, Heartbeat: gate})
	s.children.rex.session = ""

	s.heartbeatTick()
	waitFor(t, "the sweep to be recorded", func() bool { return len(gate.sweptList()) == 1 })

	if !fake.called("new moe --json --keep-open --cwd " + root + " -- /usr/bin/env ") {
		t.Errorf("rex calls = %q, want the session created around the job", fake.calls())
	}
	if !fake.called("block rename -s session:NEW block:B1 heartbeat:alpha") {
		t.Errorf("rex calls = %q, want the first block labelled", fake.calls())
	}
	body, err := os.ReadFile(rexSessionPath(root))
	if err != nil || strings.TrimSpace(string(body)) != "session:NEW" {
		t.Errorf(".moe/rex-session = %q, %v; want session:NEW", body, err)
	}
}
