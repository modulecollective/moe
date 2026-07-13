//go:build unix

package claude

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

// fakeClaudeOnPath writes an executable `claude` shell script into a
// fresh dir and prepends it to PATH, so ExecuteOneShot's LookPath
// resolves to it. Mirrors the codex package's fake-CLI idiom.
func fakeClaudeOnPath(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestExecuteOneShotDrainsAllProgress is the drain-before-Wait
// regression: the fake CLI emits the system/init sid event plus a burst
// of tool_use lines and exits 0 immediately, so the child is reaped
// while the pipe still holds buffered events. Under the fix (drain, then
// Wait) every line lands and the sid comes back; under the old order
// (Wait, then <-done) cmd.Wait closes the read end on reap and the tail
// of the burst — sometimes the init event itself — is dropped.
func TestExecuteOneShotDrainsAllProgress(t *testing.T) {
	const sid = "019e28c3-feb5-7291-aafb-12a7071a8fdb"
	const burst = 200
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"` + sid + `"}'` + "\n" +
		"i=0\n" +
		"while [ $i -lt " + strconv.Itoa(burst) + " ]; do\n" +
		`  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}'` + "\n" +
		"  i=$((i+1))\n" +
		"done\n"
	fakeClaudeOnPath(t, script)

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

// TestExecuteOneShotDeadlineKillsProcessGroup is the timeout fix: the
// fake CLI backgrounds a grandchild that inherits stdout and never
// exits, so a leader-only SIGKILL would leave the grandchild holding the
// pipe (drain hangs) and running (writing into the sandbox after the
// turn). SetProcessGroup's group-wide kill must take the grandchild too:
// the call returns promptly with a timeout error and the grandchild is
// gone.
func TestExecuteOneShotDeadlineKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"x"}'` + "\n" +
		"sleep 300 &\n" +
		"echo $! > " + pidFile + "\n" +
		"wait\n"
	fakeClaudeOnPath(t, script)

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
	if err == nil || !strings.Contains(err.Error(), "claude: -p timed out") {
		t.Fatalf("err = %v, want a claude timeout error", err)
	}

	pid := readPID(t, pidFile)
	if !processGoneWithin(pid, 5*time.Second) {
		t.Errorf("grandchild pid %d survived the deadline — group kill missed it", pid)
	}
}

// readPID reads a pid the fake CLI wrote to path, retrying briefly since
// the shell writes it asynchronously after backgrounding the grandchild.
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

// processGoneWithin polls kill(pid, 0) until it reports ESRCH (no such
// process) or the deadline passes.
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
