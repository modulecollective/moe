//go:build unix

package codex

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

func TestOneShotRawCapture(t *testing.T) {
	const raw = "{\"type\":\"thread.started\",\"thread_id\":\"capture-test\"}\n" +
		"{\"type\":\"item.started\",\"item\":{\"type\":\"command_execution\",\"command\":\"echo hi\"}}\n" +
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"\"}}\n" +
		"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":42}}\n"
	for _, tc := range []struct {
		name, tail string
		code       int
		timeout    bool
	}{
		{name: "empty-final"}, {name: "nonzero", tail: "exit 23\n", code: 23},
		{name: "timeout", tail: "sleep 300\n", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCodexOnPath(t, "#!/bin/sh\nprintf '%s' '"+raw+"'\nprintf 'stderr bytes\\000without newline' >&2\n"+tc.tail)
			capture := &agent.OneShotCapture{}
			t.Cleanup(func() { os.RemoveAll(capture.Dir) })
			var out, errb bytes.Buffer
			cap := 5 * time.Second
			if tc.timeout {
				cap = 300 * time.Millisecond
			}
			sid, err := (Agent{}).ExecuteOneShot(agent.OneShotRequest{Root: t.TempDir(), Stdout: &out, Stderr: &errb, Capture: capture, Timeout: cap})
			if sid != "capture-test" {
				t.Fatalf("sid=%q", sid)
			}
			var timeout *agent.TimeoutError
			var exit *exec.ExitError
			if tc.timeout {
				if !errors.As(err, &timeout) {
					t.Fatalf("want timeout: %v", err)
				}
			} else if tc.code != 0 {
				if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
					t.Fatalf("want exit %d: %v", tc.code, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if capture.Error != "" {
				t.Fatal(capture.Error)
			}
			for path, want := range map[string]string{capture.StdoutPath: raw, capture.StderrPath: "stderr bytes\x00without newline"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("capture %s=%q (%v)", path, got, err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("private capture: %v %v", info, err)
				}
			}
			if out.String() != "> bash: echo hi\n" || errb.String() != "stderr bytes\x00without newline" {
				t.Fatalf("forwarding: %q %q", out.String(), errb.String())
			}
		})
	}
}

func TestOneShotCaptureSetupFailurePreservesExit(t *testing.T) {
	// MkdirTemp fails, but the executable still launches and both pipes drain.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	fakeCodexOnPath(t, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"x\"}'\nprintf 'diagnostic' >&2\nexit 19\n")
	var errb bytes.Buffer
	capture := &agent.OneShotCapture{}
	_, err := (Agent{}).ExecuteOneShot(agent.OneShotRequest{Root: t.TempDir(), Stdout: io.Discard, Stderr: &errb, Capture: capture, Timeout: time.Second})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 19 {
		t.Fatalf("lost process outcome: %v", err)
	}
	if capture.Error == "" || !strings.Contains(errb.String(), "capture raw streams") || !strings.Contains(errb.String(), "diagnostic") {
		t.Fatalf("missing capture failure: %+v %s", capture, errb.String())
	}
}

func TestCaptureWriteFailureKeepsDraining(t *testing.T) {
	c := &agent.OneShotCapture{}
	var warnings bytes.Buffer
	w, _, finish := captureStreams(c, &warnings)
	t.Cleanup(func() { os.RemoveAll(c.Dir) })
	// Force writes to fail without requiring a full host filesystem.
	if err := w.(*captureWriter).file.Close(); err != nil {
		t.Fatal(err)
	}
	const size = 32 * 1024 * 1024
	n, err := io.CopyN(w, zeroReader{}, size)
	if err != nil || n != size {
		t.Fatalf("capture stopped drainage: %d %v", n, err)
	}
	finish()
	if c.Error == "" || !strings.Contains(warnings.String(), "capture raw streams") {
		t.Fatalf("lost write error: %+v %s", c, warnings.String())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestOneShotLargeRawCaptureHasBoundedAllocation(t *testing.T) {
	const size = 64 * 1024 * 1024
	const prefix = "{\"type\":\"thread.started\",\"thread_id\":\"large\"}\n"
	fakeCodexOnPath(t, "#!/bin/sh\nprintf '%s' '"+prefix+"'\nhead -c 67108864 /dev/zero\nprintf 'tail'\nhead -c 67108864 /dev/zero >&2\nprintf 'end' >&2\n")
	c := &agent.OneShotCapture{}
	t.Cleanup(func() { os.RemoveAll(c.Dir) })
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sid, err := (Agent{}).ExecuteOneShot(agent.OneShotRequest{Root: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard, Capture: c, Timeout: 30 * time.Second})
	runtime.ReadMemStats(&after)
	if err != nil || sid != "large" || c.Error != "" {
		t.Fatalf("large stream: %q %v %+v", sid, err, c)
	}
	// The parser may allocate up to its 8 MiB token cap; the 128 MiB of
	// raw streams must not cause proportional Go heap allocation.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 40*1024*1024 {
		t.Fatalf("allocated %d for raw streams", allocated)
	}
	for _, tc := range []struct{ path, prefix, tail string }{{c.StdoutPath, prefix, "tail"}, {c.StderrPath, "", "end"}} {
		want := sha256.New()
		io.WriteString(want, tc.prefix)
		io.CopyN(want, zeroReader{}, size)
		io.WriteString(want, tc.tail)
		got := sha256.New()
		f, err := os.Open(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(got, f)
		f.Close()
		if err != nil || n != int64(len(tc.prefix)+size+len(tc.tail)) || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			t.Fatalf("incomplete large stream %s: %d %v", tc.path, n, err)
		}
	}
}
