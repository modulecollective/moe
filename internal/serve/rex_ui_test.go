package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRexUIRestartLifetime(t *testing.T) {
	const retained = "host: rex ui retained in block block:EDITOR"
	const unknown = "process state unknown for block:EDITOR"
	for _, tc := range []struct {
		name, process string
		status        int
		replace       bool
		log           string
	}{
		{"restored shell", `{"last_exit":null,"child":{"pid":234,"name":"bash","path":"/usr/bin/bash","argv0":"-bash","cwd":"/home/dev/work/bureaucracy"},"foreground":{"pid":234,"name":"bash","path":"/usr/bin/bash","argv0":"-bash","cwd":"/home/dev/work/bureaucracy"}}`, 0, true, "host: rex ui block block:EDITOR runs /usr/bin/bash, not moe; replacing"},
		{"running moe", `{"last_exit":null,"child":{"path":"/usr/bin/true"}}`, 0, false, retained},
		{"running moe with malformed foreground", `{"last_exit":null,"child":{"path":"/usr/bin/true"},"foreground":42}`, 0, false, retained},
		{"moe under shell", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":"/usr/bin/true"}}`, 0, false, retained},
		{"extra fields", `{"last_exit":null,"child":{"path":"/usr/bin/true","future":[]},"future":42}`, 0, false, retained},
		{"missing foreground", `{"last_exit":null,"child":{"path":"/usr/bin/bash"}}`, 0, false, unknown},
		{"null foreground", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":null}`, 0, false, unknown},
		{"malformed foreground", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":42}`, 0, false, unknown},
		{"missing foreground path", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{}}`, 0, false, unknown},
		{"null foreground path", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":null}}`, 0, false, unknown},
		{"empty foreground path", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":""}}`, 0, false, unknown},
		{"non-string foreground path", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":42}}`, 0, false, unknown},
		{"relative foreground path", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":"moe"}}`, 0, false, unknown},
		{"missing child", `{"last_exit":null}`, 0, false, unknown},
		{"missing child with moe foreground", `{"last_exit":null,"foreground":{"path":"/usr/bin/true"}}`, 0, false, unknown},
		{"null child", `{"last_exit":null,"child":null}`, 0, false, unknown},
		{"malformed child", `{"last_exit":null,"child":[]}`, 0, false, unknown},
		{"missing child path", `{"last_exit":null,"child":{"name":"moe","argv0":"moe"}}`, 0, false, unknown},
		{"null child path", `{"last_exit":null,"child":{"path":null}}`, 0, false, unknown},
		{"empty child path", `{"last_exit":null,"child":{"path":""}}`, 0, false, unknown},
		{"non-string child path", `{"last_exit":null,"child":{"path":42}}`, 0, false, unknown},
		{"relative child path", `{"last_exit":null,"child":{"path":"moe"}}`, 0, false, unknown},
		{"exited", `{"last_exit":{"exit_code":0}}`, 0, true, "host: rex ui open in block block:NEW"},
		{"failed exit", `{"last_exit":{"exit_code":1}}`, 0, true, "host: rex ui open in block block:NEW"},
		{"exited with malformed identity", `{"last_exit":{"exit_code":0,"future":true},"child":42,"foreground":[]}`, 0, true, "host: rex ui open in block block:NEW"},
		{"malformed", `{`, 0, false, unknown},
		{"absent field", `{}`, 0, false, unknown},
		{"ambiguous exit", `{"last_exit":{}}`, 0, false, unknown},
		{"null exit code", `{"last_exit":{"exit_code":null}}`, 0, false, unknown},
		{"non-integer exit code", `{"last_exit":{"exit_code":1.5}}`, 0, false, unknown},
		{"invalid exit", `{"last_exit":false}`, 0, false, unknown},
		{"unavailable", "", 1, false, unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:EDITOR","label":"ui"},{"block_id":"block:LEGACY","label":"dash"},{"block_id":"block:JOB","label":"heartbeat:p"},{"block_id":"block:MANUAL","label":"my terminal"}]}`, 0)
			fake.reply("block-call", tc.process, tc.status)
			fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
			var log syncBuf
			h := newRexHost(fake.bin, t.TempDir(), &log)
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
			calls := fake.calls()
			if n := slices.Index(calls, "block call -b block:EDITOR process"); n < 0 || slices.Contains(calls[n+1:], calls[n]) {
				t.Fatalf("want exactly one process read: %q", calls)
			}
			if tc.replace {
				wantRun := "run --json --focus=false --keep-open -s session:S --label ui --cwd " + h.root + " -- /usr/bin/env MOE_HOME=" + h.root + " /usr/bin/true ui"
				closeAt, runAt := slices.Index(calls, "block close block:EDITOR"), slices.Index(calls, wantRun)
				if closeAt < 0 || runAt <= closeAt || slices.Contains(calls[closeAt+1:], calls[closeAt]) || slices.Contains(calls[runAt+1:], wantRun) {
					t.Fatalf("want one close then one exact launch: %q", calls)
				}
			}
			if !strings.Contains(log.String(), tc.log) {
				t.Fatalf("log = %q, want %q", log.String(), tc.log)
			}
		})
	}
}

func TestRexUIExecutablePaths(t *testing.T) {
	for _, mode := range []string{"PATH", "symlink child", "symlink foreground", "atomic replacement", "unresolvable child", "unresolvable foreground"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "moe")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			launched, childPath, foregroundPath := bin, bin, ""
			switch mode {
			case "PATH":
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				launched = "moe"
			case "symlink child", "symlink foreground":
				launched = filepath.Join(dir, "moe-link")
				if err := os.Symlink(bin, launched); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink foreground" {
					childPath, foregroundPath = "/usr/bin/bash", bin
				}
			case "unresolvable child", "unresolvable foreground":
				broken := filepath.Join(dir, "broken-link")
				if err := os.Symlink(filepath.Join(dir, "missing"), broken); err != nil {
					t.Fatal(err)
				}
				childPath, foregroundPath = broken, "/usr/bin/bash"
				if mode == "unresolvable foreground" {
					childPath, foregroundPath = "/usr/bin/bash", broken
				}
			}
			fake := newFakeRex(t)
			fake.reply("run", `{"block_ids":["block:EDITOR"]}`, 0)
			var log syncBuf
			h := newRexHost(fake.bin, t.TempDir(), &log)
			defer h.cancel()
			h.session, h.moeBin = "session:S", launched
			h.openUI()
			if mode == "atomic replacement" {
				if err := os.WriteFile(bin+".new", []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(bin+".new", bin); err != nil {
					t.Fatal(err)
				}
			}
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:EDITOR","label":"ui"}]}`, 0)
			fake.reply("block-call", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":%q}}`, childPath, foregroundPath), 0)
			h.openUI()
			wantBin := launched
			if mode == "PATH" {
				wantBin = bin
			}
			wantRun := "run --json --focus=false --keep-open -s session:S --label ui --cwd " + h.root + " -- /usr/bin/env MOE_HOME=" + h.root + " " + wantBin + " ui"
			var launches []string
			for _, call := range fake.calls() {
				if strings.HasPrefix(call, "run ") {
					launches = append(launches, call)
				}
			}
			if fake.called("block close") || !slices.Equal(launches, []string{wantRun}) {
				t.Fatalf("editor replaced: %q", fake.calls())
			}
			wantLog := "host: rex ui retained in block block:EDITOR"
			if strings.HasPrefix(mode, "unresolvable") {
				wantLog = "process state unknown for block:EDITOR"
			}
			if !strings.Contains(log.String(), wantLog) {
				t.Fatalf("log = %q, want %q", log.String(), wantLog)
			}
		})
	}
}

func TestRexUIDuplicateAndCloseFailure(t *testing.T) {
	for _, process := range []string{`{"last_exit":{"exit_code":0}}`, `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":"/usr/bin/bash"}}`} {
		for _, duplicate := range []bool{false, true} {
			fake := newFakeRex(t)
			blocks := `{"blocks":[{"block_id":"block:A","label":"ui"}`
			if duplicate {
				blocks += `,{"block_id":"block:B","label":"ui"}`
			}
			fake.reply("block-ls", blocks+`]}`, 0)
			fake.reply("block-call", process, 0)
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
			if !duplicate && !fake.called("block close block:A") {
				t.Fatal("did not attempt close")
			}
		}
	}
}

func TestRexUILostLaunchAcknowledgement(t *testing.T) {
	fake := newFakeRex(t)
	if err := os.Mkdir(filepath.Join(fake.dir, "blocks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake.dir, "blocks", "block:STALE"), []byte("ui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake.dir, "run-lost"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fake.reply("run", `{"block_ids":["block:EDITOR"]}`, 1)
	fake.reply("ls", `{"sessions":[{"session_id":"session:S","label":"moe"}]}`, 0)
	fake.reply("block-call@block:STALE", `{"last_exit":null,"child":{"path":"/usr/bin/bash"},"foreground":{"path":"/usr/bin/bash"}}`, 0)
	fake.reply("block-call", `{"last_exit":null,"child":{"path":"/usr/bin/true"}}`, 0)
	var log syncBuf
	h := newRexHost(fake.bin, t.TempDir(), &log)
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
	if !fake.called("block close block:STALE") || !strings.Contains(log.String(), "label reconciled to block:EDITOR") || !strings.Contains(log.String(), "host: rex ui retained in block block:EDITOR") {
		t.Fatalf("replacement did not reconcile and retain: calls %q, log %q", fake.calls(), log.String())
	}
}

func TestRexLastExitIgnoresOptionalIdentity(t *testing.T) {
	for _, tc := range []struct {
		process string
		code    int
		exited  bool
	}{
		{`{"last_exit":null}`, 0, false},
		{`{"last_exit":null,"child":42,"foreground":[]}`, 0, false},
		{`{"last_exit":{"exit_code":0},"child":{"path":42}}`, 0, true},
		{`{"last_exit":{"exit_code":17},"child":[],"foreground":false}`, 17, true},
	} {
		t.Run(tc.process, func(t *testing.T) {
			fake := newFakeRex(t)
			fake.reply("block-call", tc.process, 0)
			h := newRexHost(fake.bin, t.TempDir(), nil)
			defer h.cancel()
			code, exited, err := h.lastExit("block:JOB")
			if err != nil || code != tc.code || exited != tc.exited {
				t.Fatalf("lastExit = %d, %t, %v; want %d, %t, nil", code, exited, err, tc.code, tc.exited)
			}
		})
	}
}
