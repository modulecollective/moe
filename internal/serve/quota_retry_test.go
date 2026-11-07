package serve

import (
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

func TestQuotaDeferralCompletionAcrossHosts(t *testing.T) {
	for _, host := range []string{"local", "rex", "adopted rex"} {
		t.Run(host, func(t *testing.T) {
			root := t.TempDir()
			gittest.InitAt(t, root)
			payload, err := json.Marshal(run.QuotaRetry{Project: "alpha", Token: "alpha/pulse", NotBefore: time.Now().Add(time.Hour).UTC(), Targets: []run.QuotaTarget{{Project: "alpha", Run: "work", Stage: "code", Backend: "codex", Reason: "spent"}}})
			if err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "commit", "--allow-empty", "-m", "quota retry\n\n"+trailers.Block{Run: "pulse", Project: "alpha", Workflow: "pulse", Consent: "dynamic", QuotaRetry: string(payload)}.String())
			gate := &fakeHeartbeat{due: []string{"alpha"}}
			var log syncBuf
			var s *Server
			var fake *fakeRex
			if host == "local" {
				bin, _ := argvRecorder(t, run.ExitQuotaDeferred)
				s = newTestServer(t, Options{Root: root, MoeBin: bin, Heartbeat: gate, Logger: &log})
			} else {
				fake = newFakeRex(t)
				fake.reply("run", `{"block_ids":["block:A"]}`, 0)
				fake.reply("wait", `{"exit_code":76}`, 1)
				fake.reply("capture", "quota exhausted", 0)
				fake.reply("block-ls", `{"blocks":[{"block_id":"block:A","label":"heartbeat:alpha"}]}`, 0)
				s = rexServer(t, fake, Options{Root: root, MoeBin: "/bin/true", Heartbeat: gate, Logger: &log})
			}
			var notifications atomic.Int32
			s.children.notify = func(string, error) { notifications.Add(1) }
			// A deferral must neither erase nor increase failure history.
			s.heartbeat.record("alpha", true)
			s.heartbeat.record("alpha", true)
			if host == "adopted rex" {
				if err := testRex(s.children).adopt(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.children.spawn("heartbeat:alpha", s.opts.MoeBin, nil, root, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			c, ok := s.children.get("heartbeat:alpha")
			if !ok {
				t.Fatal("no child")
			}
			waitFor(t, "deferred completion", c.isSettled)
			if fake != nil {
				waitRexWorkers(t, testRex(s.children))
			}
			if !quotaDeferred(c.exitErr) || notifications.Load() != 0 {
				t.Fatalf("exit=%v notifications=%d", c.exitErr, notifications.Load())
			}
			if s.heartbeat.fails["alpha"] != 2 || s.heartbeat.skip["alpha"] != 2 {
				t.Fatalf("failure history changed: %v %v", s.heartbeat.fails, s.heartbeat.skip)
			}
			if host == "adopted rex" {
				if len(gate.cleanList()) != 0 {
					t.Fatal("adoption acknowledged an unobserved board")
				}
			} else if clean := gate.cleanList(); len(clean) != 1 || clean[0] {
				t.Fatalf("deferred survey acknowledged: %v", clean)
			}
			p := s.activity.snapshot(time.Now()).Projects[0]
			if !p.SweepDeferred || p.SweepClean || p.Failed() || !strings.Contains(p.Decision, "deferred for quota until") {
				t.Fatalf("activity=%+v", p)
			}
			panel := s.activity.panel(time.Now())
			for _, ev := range panel.Events {
				if ev.Kind == "exit" && (ev.Failed || !strings.Contains(ev.Text, "deferred for quota")) {
					t.Fatalf("exit event=%+v", ev)
				}
			}
			if !strings.Contains(log.String(), "deferred for quota until") || strings.Contains(log.String(), "sweep failed") || strings.Contains(log.String(), "swept clean") {
				t.Fatal(log.String())
			}
			if fake != nil && !fake.called("block close block:A") {
				t.Fatalf("quota deferral kept a failed screen: %q", fake.calls())
			}
		})
	}
}
