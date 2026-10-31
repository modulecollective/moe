package serve

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRexBlocksValidateWholeListing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		valid bool
	}{
		{"empty", `{"blocks":[]}`, true},
		{"optional labels and extra fields", `{"blocks":[{"block_id":"block:A","extra":1},{"block_id":"block:B","label":""},{"block_id":"block:C","label":"shell"}],"revision":3}`, true},
		{"malformed", `{`, false},
		{"missing array", `{}`, false},
		{"null array", `{"blocks":null}`, false},
		{"object", `{"blocks":{}}`, false},
		{"string", `{"blocks":""}`, false},
		{"null row", `{"blocks":[null]}`, false},
		{"missing ID", `{"blocks":[{"label":"heartbeat:alpha"}]}`, false},
		{"empty ID", `{"blocks":[{"block_id":""}]}`, false},
		{"null ID", `{"blocks":[{"block_id":null}]}`, false},
		{"invalid ID type", `{"blocks":[{"block_id":1}]}`, false},
		{"invalid label type", `{"blocks":[{"block_id":"block:A","label":1}]}`, false},
		{"invalid row after heartbeat", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"},{}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", tc.reply, 0)
			h := newRexHost(fake.bin, t.TempDir(), io.Discard)
			t.Cleanup(h.cancel)
			blocks, err := h.blocks("session:OLD")
			if (err == nil) != tc.valid {
				t.Fatalf("blocks = %v, error = %v; valid = %t", blocks, err, tc.valid)
			}
			if !tc.valid && blocks != nil {
				t.Fatalf("invalid listing returned partial blocks: %v", blocks)
			}
		})
	}
}

func seedStartupRexSession(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rexSessionPath(root), []byte("session:OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRexStartupWaitsForReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ls     string
		status int
	}{
		{"present", `{"sessions":[{"session_id":"session:OLD"}]}`, 0},
		{"unavailable", "", 1},
		{"invalid session listing", `{}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := rexStartupFake(t)
			fake.reply("block-ls", "", 1)
			fake.reply("ls", tc.ls, tc.status)
			fake.reply("wait", `{"exit_code":1}`, 1)
			fake.hold("wait")
			root := t.TempDir()
			seedStartupRexSession(t, root)
			for _, project := range []string{"alpha", "beta"} {
				if err := os.WriteFile(sweepRunPath(root, project), []byte("sentinel\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			moe, localArgv := argvRecorder(t, 0)
			gate := &fakeHeartbeat{due: []string{"alpha", "beta"}}
			var log syncBuf
			s := newTestServer(t, Options{Root: root, MoeBin: moe, Heartbeat: gate, Logger: &log})
			s.activity.recordSweepRun("alpha", "prior-run")
			s.heartbeat.record("alpha", true)
			s.selectHost()
			for range 2 {
				s.heartbeatTick()
			}
			if testAdoptionError(s) == nil || testPermanentError(s) != nil || testRex(s.children).sessionID() != "session:OLD" {
				t.Fatalf("adoption = %v, host = %v, ID = %q", testAdoptionError(s), testPermanentError(s), testRex(s.children).sessionID())
			}
			if body, _ := os.ReadFile(rexSessionPath(root)); string(body) != "session:OLD\n" {
				t.Fatalf("session identity changed: %q", body)
			}
			for _, call := range fake.calls() {
				if strings.HasPrefix(call, "run ") || strings.HasPrefix(call, "new ") || strings.HasPrefix(call, "block close ") || strings.HasPrefix(call, "wait ") {
					t.Fatalf("unreconciled startup mutated Rex: %q", fake.calls())
				}
			}
			if localArgv() != "" || len(gate.sweptList()) != 0 || len(s.children.all) != 0 {
				t.Fatal("unreconciled startup dispatched or recorded a child")
			}
			for _, project := range []string{"alpha", "beta"} {
				body, err := os.ReadFile(sweepRunPath(root, project))
				if err != nil || string(body) != "sentinel\n" {
					t.Fatalf("%s emit file changed: %q, %v", project, body, err)
				}
			}
			s.activity.mu.Lock()
			alpha := *s.activity.projects["alpha"]
			beta := *s.activity.projects["beta"]
			s.activity.mu.Unlock()
			if alpha.runID != "prior-run" || !alpha.held || !beta.held || !strings.Contains(beta.decision, "rex adoption pending") {
				t.Fatalf("hold lost activity state: alpha = %+v, beta = %+v", alpha, beta)
			}
			s.heartbeat.mu.Lock()
			left, fails := s.heartbeat.skip["alpha"], s.heartbeat.fails["alpha"]
			s.heartbeat.mu.Unlock()
			if left != 1 || fails != 1 || gate.passCount() != 2 || !strings.Contains(log.String(), "heartbeat adoption retry") {
				t.Fatalf("outage consumed backoff or lost tick/log: left %d, fails %d, passes %d, log %q", left, fails, gate.passCount(), log.String())
			}
			s.heartbeat.record("alpha", false)

			// The recovered list includes a live alpha, an already-exited
			// gamma, and blocks that must not become heartbeat children.
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"},{"block_id":"block:EXIT","label":"heartbeat:gamma"},{"block_id":"block:D","label":"dash"},{"block_id":"block:X","label":"shell"}]}`, 0)
			fake.reply("wait@block:EXIT", `{"exit_code":1}`, 1)
			fake.reply("run@dash", `{"block_ids":["block:D2"]}`, 0)
			fake.reply("run@heartbeat:beta", `{"block_ids":["block:B"]}`, 0)
			s.heartbeatTick()
			a, ok := s.children.get("heartbeat:alpha")
			if !ok || testBlock(s.children, a) != "block:A" || testAdoptionError(s) != nil {
				t.Fatalf("alpha not reconciled before dispatch: child %v, error %v", a, testAdoptionError(s))
			}
			b, ok := s.children.get("heartbeat:beta")
			if !ok || testBlock(s.children, b) != "block:B" || len(s.children.all) != 3 {
				t.Fatalf("beta not dispatched or foreign block adopted: %d children", len(s.children.all))
			}
			if fake.called("run --json --focus=false --keep-open -s session:OLD --label heartbeat:alpha ") || fake.called("new ") || fake.called("block close block:X") {
				t.Fatalf("recovery duplicated alpha, replaced session, or closed foreign block: %q", fake.calls())
			}
			if body, _ := os.ReadFile(sweepRunPath(root, "alpha")); string(body) != "sentinel\n" {
				t.Fatalf("adoption consumed live alpha's emit file: %q", body)
			}
			waitFor(t, "adopted exited block backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.fails["gamma"] == 1
			})
			waitFor(t, "live watchers", func() bool { return fake.called("wait --json block:A") && fake.called("wait --json block:B") })
			before := len(fake.calls())
			s.heartbeatTick()
			if current, _ := s.children.get("heartbeat:alpha"); current != a || len(fake.calls()) != before {
				t.Fatalf("later tick repeated adoption or dash: %q", fake.calls()[before:])
			}
			calls := fake.calls()
			closeAt := slices.Index(calls, "block close block:D")
			dashAt, betaAt, dashCount := -1, -1, 0
			for i, call := range calls {
				if strings.Contains(call, "--label dash ") {
					dashAt, dashCount = i, dashCount+1
				}
				if strings.Contains(call, "--label heartbeat:beta ") {
					betaAt = i
				}
			}
			if closeAt < 0 || dashAt <= closeAt || betaAt <= dashAt || dashCount != 1 {
				t.Fatalf("want one dash refresh after adoption and before beta: %q", calls)
			}
			fake.release("wait")
			waitRexWorkers(t, testRex(s.children))
			waitFor(t, "completion backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.fails["alpha"] == 1 && s.heartbeat.fails["beta"] == 1
			})
			if got := gate.sweptList(); !slices.Equal(got, []string{"beta"}) {
				t.Fatalf("Swept = %v; adopted jobs must not acknowledge dispatch", got)
			}
			s.activity.mu.Lock()
			counts := map[string]int{}
			for _, event := range s.activity.events {
				if event.Kind == "exit" {
					counts[event.Subject]++
				}
			}
			s.activity.mu.Unlock()
			if counts["heartbeat:alpha"] != 1 || counts["heartbeat:gamma"] != 1 || counts["heartbeat:beta"] != 1 {
				t.Fatalf("completion hooks = %v", counts)
			}
		})
	}
}

func TestRexStartupRejectsPartialAdoption(t *testing.T) {
	for _, listing := range []string{`{}`, `{"blocks":null}`, `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"},{}]}`} {
		t.Run(listing, func(t *testing.T) {
			fake := rexStartupFake(t)
			fake.reply("block-ls", listing, 0)
			fake.reply("ls", `{"sessions":[{"session_id":"session:OLD"}]}`, 0)
			root := t.TempDir()
			seedStartupRexSession(t, root)
			s := newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: &fakeHeartbeat{due: []string{"alpha"}}})
			fake.reply("wait", `{"exit_code":1}`, 1)
			s.selectHost()
			s.heartbeatTick()
			if testAdoptionError(s) == nil || len(s.children.all) != 0 || fake.called("run ") || fake.called("new ") || fake.called("wait ") {
				t.Fatalf("invalid listing partially reconciled: error %v, calls %q", testAdoptionError(s), fake.calls())
			}
		})
	}
}

func TestRexStartupRecoversOnIdleTick(t *testing.T) {
	for _, dashFails := range []bool{false, true} {
		name := "dash opens"
		if dashFails {
			name = "dash fails"
		}
		t.Run(name, func(t *testing.T) {
			fake := rexStartupFake(t)
			fake.reply("block-ls", "", 1)
			fake.reply("ls", `{"sessions":[{"session_id":"session:OLD"}]}`, 0)
			fake.reply("wait", `{"exit_code":1}`, 1)
			root := t.TempDir()
			seedStartupRexSession(t, root)
			gate := &fakeHeartbeat{quiet: []string{"alpha"}}
			s := newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			s.selectHost()
			fake.reply("block-ls", `{"blocks":[]}`, 0)
			fake.reply("run@dash", `{"block_ids":["block:D"]}`, 0)
			if dashFails {
				fake.reply("run@dash", "", 1)
			}
			s.heartbeatTick()
			if testAdoptionError(s) != nil || !fake.called("run --json --focus=false --keep-open -s session:OLD --label dash ") || fake.called("new ") || len(s.children.all) != 0 {
				t.Fatalf("idle recovery failed: error %v, calls %q", testAdoptionError(s), fake.calls())
			}
			before := len(fake.calls())
			s.heartbeatTick()
			if len(fake.calls()) != before {
				t.Fatalf("idle tick repeated adoption/dash: %q", fake.calls()[before:])
			}
			gate.due, gate.quiet = []string{"alpha"}, nil
			fake.reply("run@heartbeat:alpha", `{"block_ids":["block:A"]}`, 0)
			s.heartbeatTick()
			waitRexWorkers(t, testRex(s.children))
			waitFor(t, "eligible sweep after dash attempt", func() bool { return len(gate.sweptList()) == 1 })
			if testRex(s.children).sessionID() != "session:OLD" || fake.called("new ") {
				t.Fatalf("empty block array replaced the session: %q", fake.calls())
			}
		})
	}
}

func TestRexStartupMissingSessionRetainsIdentityOnFailedReplacement(t *testing.T) {
	for _, listing := range []string{`{"sessions":[]}`, `{"sessions":[{"session_id":"session:OLD2","label":"moe"}]}`} {
		t.Run(listing, func(t *testing.T) {
			fake := rexStartupFake(t)
			fake.reply("block-ls", "", 1)
			fake.reply("ls", listing, 0)
			fake.reply("run@session:OLD", "", 1)
			fake.reply("new", "", 1)
			fake.reply("wait", `{"exit_code":1}`, 1)
			root := t.TempDir()
			seedStartupRexSession(t, root)
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			s := newTestServer(t, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate})
			s.selectHost()
			if testAdoptionError(s) != nil || testRex(s.children).sessionID() != "session:OLD" || len(s.children.all) != 0 {
				t.Fatalf("failed startup replacement lost identity: adoption %v, ID %q", testAdoptionError(s), testRex(s.children).sessionID())
			}
			if body, _ := os.ReadFile(rexSessionPath(root)); string(body) != "session:OLD\n" {
				t.Fatalf("failed startup replacement changed persisted ID: %q", body)
			}
			newCalls := func() int {
				n := 0
				for _, call := range fake.calls() {
					if strings.HasPrefix(call, "new ") {
						n++
					}
				}
				return n
			}
			if newCalls() != 1 {
				t.Fatalf("startup must make one replacement attempt: %q", fake.calls())
			}

			// Absence was true at startup; it is not cached permission
			// to create another session while the next probe is unavailable.
			fake.reply("ls", "", 1)
			s.heartbeatTick()
			if newCalls() != 1 || len(s.children.all) != 0 || len(gate.sweptList()) != 0 || testRex(s.children).sessionID() != "session:OLD" {
				t.Fatalf("later uncertainty created a replacement/phantom: %q", fake.calls())
			}
			fake.reply("ls", listing, 0)
			fake.reply("new", `{"session_id":"session:NEW","initial_windows":[{"block_ids":["block:A"]}]}`, 0)
			fake.reply("block-ls@session:NEW", `{"blocks":[]}`, 0)
			fake.reply("run@session:NEW", `{"block_ids":["block:A"]}`, 0)
			fake.reply("run@dash", `{"block_ids":["block:D"]}`, 0)
			s.heartbeatTick()
			waitRexWorkers(t, testRex(s.children))
			waitFor(t, "replacement sweep backoff", func() bool {
				s.heartbeat.mu.Lock()
				defer s.heartbeat.mu.Unlock()
				return s.heartbeat.fails["alpha"] == 1
			})
			if newCalls() != 2 || testRex(s.children).sessionID() != "session:NEW" {
				t.Fatalf("confirmed absence did not recover: %q", fake.calls())
			}
			if body, _ := os.ReadFile(rexSessionPath(root)); string(body) != "session:NEW\n" {
				t.Fatalf("successful replacement not persisted: %q", body)
			}
			if !fake.called("run --json --focus=false --keep-open -s session:NEW --label heartbeat:alpha ") || !fake.called("run --json --focus=false --keep-open -s session:NEW --label dash ") {
				t.Fatalf("sweep/dash missing from replacement: %q", fake.calls())
			}
			s.heartbeat.record("alpha", false)
			fake.reply("run@session:NEW", `{"block_ids":["block:B"]}`, 0)
			s.heartbeatTick()
			waitRexWorkers(t, testRex(s.children))
			waitFor(t, "second sweep", func() bool { return len(gate.sweptList()) == 2 })
			if newCalls() != 2 || !fake.called("run --json --focus=false --keep-open -s session:NEW --label heartbeat:alpha ") {
				t.Fatalf("next sweep did not reuse replacement: %q", fake.calls())
			}
		})
	}
}

func TestRexStartupAbsenceIsNotCachedForDash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ls     string
		status int
	}{
		{"unavailable", "", 1},
		{"invalid", `{}`, 0},
		{"present", `{"sessions":[{"session_id":"session:S"}]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", "", 1)
			fake.reply("ls", `{"sessions":[]}`, 0)
			s := rexServer(t, fake, Options{Root: t.TempDir(), MoeBin: "/bin/true"})
			h := testRex(s.children)
			if err := h.adopt(); err != nil {
				t.Fatalf("confirmed absence should complete adoption: %v", err)
			}
			fake.reply("ls", tc.ls, tc.status)
			h.openDash()
			if h.sessionID() != "session:S" || fake.called("run ") || fake.called("new ") || fake.called("block close ") {
				t.Fatalf("dash reused obsolete absence evidence: %q", fake.calls())
			}
		})
	}
}
