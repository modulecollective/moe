//go:build unix

package codex

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// fakeCodexOnPath writes an executable `codex` shell script into a fresh
// dir and prepends it to PATH so ExecuteOneShot's LookPath resolves to
// it.
func fakeCodexOnPath(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestExecuteOneShotDrainsAllProgress is the drain-before-Wait
// regression for codex: the fake CLI emits thread.started plus a burst
// of command_execution items and exits 0 immediately, so the child is
// reaped while the pipe still holds buffered events. Under the fix every
// line lands and the sid comes back; under the old order the tail of the
// burst — sometimes thread.started itself — is dropped on reap.
func TestExecuteOneShotDrainsAllProgress(t *testing.T) {
	const sid = "019e28c3-feb5-7291-aafb-12a7071a8fdb"
	const burst = 200
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"type":"thread.started","thread_id":"` + sid + `"}'` + "\n" +
		"i=0\n" +
		"while [ $i -lt " + strconv.Itoa(burst) + " ]; do\n" +
		`  printf '%s\n' '{"type":"item.started","item":{"id":"c","type":"command_execution","command":"echo hi"}}'` + "\n" +
		"  i=$((i+1))\n" +
		"done\n"
	fakeCodexOnPath(t, script)

	// Stdout and Stderr must be distinct writers: the progress reader
	// writes to Stdout while exec's stderr copier writes to Stderr, so
	// aliasing them races the same buffer.
	var out, errBuf bytes.Buffer
	gotSid, err := Agent{}.ExecuteOneShot(agent.OneShotRequest{
		Root:       t.TempDir(),
		UserPrompt: "go",
		Timeout:    30 * time.Second,
		Stdout:     &out,
		Stderr:     &errBuf,
	})
	if err != nil {
		t.Fatalf("ExecuteOneShot: %v", err)
	}
	if gotSid != sid {
		t.Errorf("sid = %q, want %q", gotSid, sid)
	}
	if n := strings.Count(out.String(), "> bash: echo hi"); n != burst {
		t.Errorf("progress lines = %d, want %d (buffered events dropped on reap)", n, burst)
	}
}

// TestExecuteOneShotDeadlineKillsProcessGroup is the timeout fix for
// codex: the fake CLI backgrounds a grandchild that inherits stdout and
// never exits, so a leader-only SIGKILL would leave it holding the pipe
// (drain hangs) and running (writing into the sandbox after the turn).
// SetProcessGroup's group-wide kill must take the grandchild too.
func TestExecuteOneShotDeadlineKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"type":"thread.started","thread_id":"x"}'` + "\n" +
		"sleep 300 &\n" +
		"echo $! > " + pidFile + "\n" +
		"wait\n"
	fakeCodexOnPath(t, script)

	start := time.Now()
	_, err := Agent{}.ExecuteOneShot(agent.OneShotRequest{
		Root:       t.TempDir(),
		UserPrompt: "go",
		Timeout:    500 * time.Millisecond,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("ExecuteOneShot ran %s — the group kill didn't unblock the drain", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "codex: exec timed out") {
		t.Fatalf("err = %v, want a codex timeout error", err)
	}

	pid := readPID(t, pidFile)
	if !processGoneWithin(pid, 5*time.Second) {
		t.Errorf("grandchild pid %d survived the deadline — group kill missed it", pid)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid file never appeared: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processGoneWithin(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
