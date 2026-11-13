package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRexUIRestartLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, process string
		status        int
		replace       bool
	}{
		{"running", `{"last_exit":null}`, 0, false},
		{"exited", `{"last_exit":{"exit_code":0}}`, 0, true},
		{"failed exit", `{"last_exit":{"exit_code":1}}`, 0, true},
		{"malformed", `{`, 0, false},
		{"absent field", `{}`, 0, false},
		{"ambiguous exit", `{"last_exit":{}}`, 0, false},
		{"unavailable", "", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:EDITOR","label":"ui"},{"block_id":"block:LEGACY","label":"dash"},{"block_id":"block:JOB","label":"heartbeat:p"},{"block_id":"block:MANUAL","label":"my terminal"}]}`, 0)
			fake.reply("block-call", tc.process, tc.status)
			fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
			h := newRexHost(fake.bin, t.TempDir(), nil)
			defer h.cancel()
			h.session = "session:S"
			h.moeBin = "/usr/bin/true"
			h.openUI()
			if !fake.called("block close block:LEGACY") {
				t.Fatal("legacy dash not retired")
			}
			if fake.called("block close block:EDITOR") != tc.replace || fake.called("run ") != tc.replace {
				t.Fatalf("wrong replacement: %q", fake.calls())
			}
			if fake.called("block close block:JOB") || fake.called("block close block:MANUAL") {
				t.Fatal("unrelated block closed")
			}
		})
	}
}

func TestRexUIDuplicateAndCloseFailure(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		fake := newFakeRex(t)
		blocks := `{"blocks":[{"block_id":"block:A","label":"ui"}`
		if duplicate {
			blocks += `,{"block_id":"block:B","label":"ui"}`
		}
		fake.reply("block-ls", blocks+`]}`, 0)
		fake.reply("block-call", `{"last_exit":{"exit_code":0}}`, 0)
		fake.reply("block-close", "cannot close", 1)
		h := newRexHost(fake.bin, t.TempDir(), nil)
		h.session = "session:S"
		h.moeBin = "/usr/bin/true"
		h.openUI()
		h.cancel()
		if fake.called("run ") {
			t.Fatal("duplicate console launched")
		}
		if duplicate && fake.called("block close") {
			t.Fatal("duplicate editors closed")
		}
	}
}

func TestRexUILostLaunchAcknowledgement(t *testing.T) {
	fake := newFakeRex(t)
	if err := os.Mkdir(filepath.Join(fake.dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake.dir, "run-lost"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fake.reply("run", `{"block_ids":["block:EDITOR"]}`, 1)
	fake.reply("ls", `{"sessions":[{"session_id":"session:S","label":"moe"}]}`, 0)
	fake.reply("block-call", `{"last_exit":null}`, 0)
	h := newRexHost(fake.bin, t.TempDir(), nil)
	defer h.cancel()
	h.session = "session:S"
	h.moeBin = "/usr/bin/true"
	h.openUI()
	h.openUI()
	count := 0
	for _, c := range fake.calls() {
		if strings.HasPrefix(c, "run ") {
			count++
		}
	}
	if count != 1 || fake.called("block close block:EDITOR") {
		t.Fatalf("lost acknowledgement duplicated editor: %q", fake.calls())
	}
}
