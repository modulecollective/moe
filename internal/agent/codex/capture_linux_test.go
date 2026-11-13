//go:build linux

package codex

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

func TestOneShotDeadlineClosesEscapedStderr(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "escaped.pid")
	// setsid escapes the process-group kill and keeps only stderr open.
	// Stdout reaches EOF first; Wait must still be bounded by the deadline.
	fakeCodexOnPath(t, fmt.Sprintf("#!/bin/sh\nprintf 'before escape' >&2\nsetsid sh -c 'echo $$ > \"$1\"; exec sleep 300' sh %q >/dev/null &\nwait\n", pidPath))
	capture := &agent.OneShotCapture{}
	t.Cleanup(func() { os.RemoveAll(capture.Dir) })
	// Even a regression that wedges the executor releases the descriptor when
	// the cleanup timer kills the owned fixture, letting the test fail promptly.
	cleanupFired := make(chan struct{})
	cleanup := time.AfterFunc(10*time.Second, func() {
		close(cleanupFired)
		if b, err := os.ReadFile(pidPath); err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(b), "%d", &pid); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	defer cleanup.Stop()
	defer func() {
		pid := readPID(t, pidPath)
		syscall.Kill(pid, syscall.SIGKILL)
	}()
	_, err := (Agent{}).ExecuteOneShot(agent.OneShotRequest{Root: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard, Capture: capture, Timeout: 300 * time.Millisecond})
	select {
	case <-cleanupFired:
		t.Fatal("stderr drainage waited for fixture cleanup")
	default:
	}
	if err := syscall.Kill(readPID(t, pidPath), 0); err != nil {
		t.Fatalf("escaped fixture did not survive to hold stderr: %v", err)
	}
	var timeout *agent.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("lost timeout: %v", err)
	}
	got, readErr := os.ReadFile(capture.StderrPath)
	if readErr != nil || string(got) != "before escape" {
		t.Fatalf("lost pre-deadline stderr: %q %v", got, readErr)
	}
}
