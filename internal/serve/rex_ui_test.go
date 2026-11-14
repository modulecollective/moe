package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Fake Rex records launches without executing these files; only their paths
// and executable modes participate in the real identity classifier.
func rexUIExecutables(t *testing.T) (moe, shell string) {
	t.Helper()
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moe, shell = filepath.Join(dir, "moe"), filepath.Join(dir, "shell")
	for _, path := range []string{moe, shell} {
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return moe, shell
}

func TestRexUIRestartLifetime(t *testing.T) {
	moeBin, shellBin := rexUIExecutables(t)
	const retained = "host: rex ui retained in block block:EDITOR"
	const unknown = "process state unknown for block:EDITOR"
	const invalidChild = unknown + ": rex process: invalid child path"
	const invalidForeground = unknown + ": rex process: invalid foreground path"
	for _, tc := range []struct {
		name, process string
		status        int
		replace       bool
		log           string
		capturedBash  bool
	}{
		{"captured restored shell", `{"last_exit":null,"child":{"pid":234,"name":"bash","path":"/usr/bin/bash","argv0":"-bash","cwd":"/home/dev/work/bureaucracy"},"foreground":{"pid":234,"name":"bash","path":"/usr/bin/bash","argv0":"-bash","cwd":"/home/dev/work/bureaucracy"}}`, 0, true, "host: rex ui block block:EDITOR runs /usr/bin/bash, not moe; replacing", true},
		{"synthetic restored shell", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":%q}}`, shellBin, shellBin), 0, true, "host: rex ui block block:EDITOR runs " + shellBin + ", not moe; replacing", false},
		{"running moe", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q}}`, moeBin), 0, false, retained, false},
		{"running moe with malformed foreground", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":42}`, moeBin), 0, false, retained, false},
		{"moe under shell", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":%q}}`, shellBin, moeBin), 0, false, retained, false},
		{"extra fields", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q,"future":[]},"future":42}`, moeBin), 0, false, retained, false},
		{"missing foreground", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q}}`, shellBin), 0, false, invalidForeground, false},
		{"null foreground", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":null}`, shellBin), 0, false, invalidForeground, false},
		{"malformed foreground", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":42}`, shellBin), 0, false, invalidForeground, false},
		{"missing foreground path", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{}}`, shellBin), 0, false, invalidForeground, false},
		{"null foreground path", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":null}}`, shellBin), 0, false, invalidForeground, false},
		{"empty foreground path", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":""}}`, shellBin), 0, false, invalidForeground, false},
		{"non-string foreground path", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":42}}`, shellBin), 0, false, invalidForeground, false},
		{"relative foreground path", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":"moe"}}`, shellBin), 0, false, invalidForeground, false},
		{"missing child", `{"last_exit":null}`, 0, false, invalidChild, false},
		{"missing child with moe foreground", fmt.Sprintf(`{"last_exit":null,"foreground":{"path":%q}}`, moeBin), 0, false, invalidChild, false},
		{"null child", `{"last_exit":null,"child":null}`, 0, false, invalidChild, false},
		{"malformed child", `{"last_exit":null,"child":[]}`, 0, false, invalidChild, false},
		{"missing child path", `{"last_exit":null,"child":{"name":"moe","argv0":"moe"}}`, 0, false, invalidChild, false},
		{"null child path", `{"last_exit":null,"child":{"path":null}}`, 0, false, invalidChild, false},
		{"empty child path", `{"last_exit":null,"child":{"path":""}}`, 0, false, invalidChild, false},
		{"non-string child path", `{"last_exit":null,"child":{"path":42}}`, 0, false, invalidChild, false},
		{"relative child path", `{"last_exit":null,"child":{"path":"moe"}}`, 0, false, invalidChild, false},
		{"exited", `{"last_exit":{"exit_code":0}}`, 0, true, "host: rex ui open in block block:NEW", false},
		{"failed exit", `{"last_exit":{"exit_code":1}}`, 0, true, "host: rex ui open in block block:NEW", false},
		{"exited with malformed identity", `{"last_exit":{"exit_code":0,"future":true},"child":42,"foreground":[]}`, 0, true, "host: rex ui open in block block:NEW", false},
		{"malformed", `{`, 0, false, unknown, false},
		{"absent field", `{}`, 0, false, unknown, false},
		{"ambiguous exit", `{"last_exit":{}}`, 0, false, unknown, false},
		{"null exit code", `{"last_exit":{"exit_code":null}}`, 0, false, unknown, false},
		{"non-integer exit code", `{"last_exit":{"exit_code":1.5}}`, 0, false, unknown, false},
		{"invalid exit", `{"last_exit":false}`, 0, false, unknown, false},
		{"unavailable", "", 1, false, unknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.capturedBash {
				path, err := rexExecutable("/usr/bin/bash")
				if err != nil {
					t.Skipf("captured shell executable unavailable: %v", err)
				}
				if _, err := filepath.EvalSymlinks(path); err != nil {
					t.Skipf("captured shell executable cannot resolve symlinks: %v", err)
				}
			}
			fake := newFakeRex(t)
			fake.reply("block-ls", `{"blocks":[{"block_id":"block:EDITOR","label":"ui"},{"block_id":"block:LEGACY","label":"dash"},{"block_id":"block:JOB","label":"heartbeat:p"},{"block_id":"block:MANUAL","label":"my terminal"}]}`, 0)
			fake.reply("block-call", tc.process, tc.status)
			fake.reply("run", `{"block_ids":["block:NEW"]}`, 0)
			var log syncBuf
			h := newRexHost(fake.bin, t.TempDir(), &log)
			defer h.cancel()
			h.session = "session:S"
			h.moeBin = moeBin
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
				wantRun := "run --json --focus=false --keep-open -s session:S --label ui --cwd " + h.root + " -- /usr/bin/env MOE_HOME=" + h.root + " " + moeBin + " ui"
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
			bin, shellBin := rexUIExecutables(t)
			dir := filepath.Dir(bin)
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
					childPath, foregroundPath = shellBin, bin
				}
			case "unresolvable child", "unresolvable foreground":
				broken := filepath.Join(dir, "broken-link")
				if err := os.Symlink(filepath.Join(dir, "missing"), broken); err != nil {
					t.Fatal(err)
				}
				childPath, foregroundPath = broken, shellBin
				if mode == "unresolvable foreground" {
					childPath, foregroundPath = shellBin, broken
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
			switch mode {
			case "unresolvable child":
				wantLog = "process state unknown for block:EDITOR: resolve child executable"
			case "unresolvable foreground":
				wantLog = "process state unknown for block:EDITOR: resolve foreground executable"
			}
			if !strings.Contains(log.String(), wantLog) {
				t.Fatalf("log = %q, want %q", log.String(), wantLog)
			}
		})
	}
}

func TestRexUIDuplicateAndCloseFailure(t *testing.T) {
	moeBin, shellBin := rexUIExecutables(t)
	for _, process := range []string{`{"last_exit":{"exit_code":0}}`, fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":%q}}`, shellBin, shellBin)} {
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
			h.moeBin = moeBin
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
	moeBin, shellBin := rexUIExecutables(t)
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
	fake.reply("block-call@block:STALE", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q},"foreground":{"path":%q}}`, shellBin, shellBin), 0)
	fake.reply("block-call", fmt.Sprintf(`{"last_exit":null,"child":{"path":%q}}`, moeBin), 0)
	var log syncBuf
	h := newRexHost(fake.bin, t.TempDir(), &log)
	defer h.cancel()
	h.session = "session:S"
	h.moeBin = moeBin
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
