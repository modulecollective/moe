//go:build linux

package pty

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStartEchoesThroughMaster verifies the PTY round-trip: start a
// child whose output we can recognize, drain the master, and look
// for the string. Uses /bin/echo to keep the test independent of
// shell or PATH lookups.
func TestStartEchoesThroughMaster(t *testing.T) {
	p, err := Start(exec.Command("/bin/echo", "-n", "hello pty"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	type result struct {
		data []byte
		err  error
	}
	out := make(chan result, 1)
	go func() {
		got, err := io.ReadAll(p.File())
		out <- result{got, err}
	}()

	if err := p.Cmd().Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	select {
	case res := <-out:
		if !strings.Contains(string(res.data), "hello pty") {
			t.Fatalf("missing 'hello pty' in %q (err=%v)", string(res.data), res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PTY read goroutine never finished")
	}
}

func TestStartReportsErrorForMissingBinary(t *testing.T) {
	_, err := Start(exec.Command("/no/such/binary/please"))
	if err == nil {
		t.Fatal("expected error from Start on missing binary")
	}
}

func TestSetSize(t *testing.T) {
	p, err := Start(exec.Command("/bin/sleep", "1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	if err := SetSize(p.File(), 50, 200); err != nil {
		t.Errorf("SetSize: %v", err)
	}
}
