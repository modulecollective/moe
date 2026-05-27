package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

// TestMain double-roles the test binary as the interrupt-test child:
// when MOE_AGENT_INTERRUPT_CHILD is set, the process installs a SIGINT
// handler, prints "ready", waits for one signal, then exits with the
// requested code. This gives the helper tests a child whose readiness
// is observable and whose trap latency is bounded by the Go runtime
// (not by an external `sleep` whose blocking semantics vary across
// `/bin/sh` implementations).
func TestMain(m *testing.M) {
	if mode := os.Getenv("MOE_AGENT_INTERRUPT_CHILD"); mode != "" {
		runInterruptChild(mode)
		return
	}
	os.Exit(m.Run())
}

func runInterruptChild(mode string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
	fmt.Println("ready")
	<-ch
	switch mode {
	case "exit0":
		os.Exit(0)
	case "exit7":
		os.Exit(7)
	default:
		os.Exit(99)
	}
}

// TestStartCommandNormalExitReturnsNil covers the baseline path: a
// child that exits zero with no interrupt returns nil. Pins that the
// helper doesn't fabricate an interrupt error when none happened.
func TestStartCommandNormalExitReturnsNil(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	stopped := false
	c, err := startCommand(exec.Command("true"), sigCh, func() {
		stopped = true
		close(sigCh)
	})
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if !stopped {
		t.Fatal("stop func was not called")
	}
}

// TestStartCommandStartFailureReturnsErr pins that a failed Start
// surfaces the underlying error and tears down the injected watcher
// (stop is invoked) so the test process leaks no signal registration.
func TestStartCommandStartFailureReturnsErr(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	stopped := false
	_, err := startCommand(
		exec.Command("/nonexistent/moe/agent/interrupt/test/binary"),
		sigCh,
		func() {
			stopped = true
			close(sigCh)
		},
	)
	if err == nil {
		t.Fatal("expected start error, got nil")
	}
	if !stopped {
		t.Fatal("stop func should run on start failure")
	}
}

// TestStartCommandInterruptCleanExitReturnsSentinel is the contract
// the design specifies: the child traps INT and exits 0, but moe saw
// the interrupt, so Wait must return ErrInterrupted rather than the
// child's clean-looking exit.
func TestStartCommandInterruptCleanExitReturnsSentinel(t *testing.T) {
	cmd, stdout := interruptChild(t, "exit0")
	sigCh := make(chan os.Signal, 1)
	c, err := startCommand(cmd, sigCh, func() { close(sigCh) })
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	awaitReady(t, stdout)
	sigCh <- os.Interrupt
	err = c.Wait()
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Wait = %v, want ErrInterrupted", err)
	}
}

// TestStartCommandInterruptNonZeroExitPreservesProcessError pins the
// design's "non-zero child exits keep their original error shape"
// rule: even when an interrupt landed, the *exec.ExitError survives so
// callers can read the real exit code.
func TestStartCommandInterruptNonZeroExitPreservesProcessError(t *testing.T) {
	cmd, stdout := interruptChild(t, "exit7")
	sigCh := make(chan os.Signal, 1)
	c, err := startCommand(cmd, sigCh, func() { close(sigCh) })
	if err != nil {
		t.Fatalf("startCommand: %v", err)
	}
	awaitReady(t, stdout)
	sigCh <- os.Interrupt
	err = c.Wait()
	if errors.Is(err, ErrInterrupted) {
		t.Fatalf("Wait = %v, want process error (ErrInterrupted should not mask exit 7)", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait = %v, want *exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

// interruptChild re-execs the test binary in child mode (see TestMain).
// The mode string selects the exit code the child uses after receiving
// one SIGINT.
func interruptChild(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=ChildIgnored")
	cmd.Env = append(os.Environ(), "MOE_AGENT_INTERRUPT_CHILD="+mode)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	return cmd, bufio.NewReader(pipe)
}

// awaitReady consumes one "ready\n" line from r and then drains the
// rest of the pipe in a goroutine so the child doesn't block on a full
// stdout buffer.
func awaitReady(t *testing.T, r *bufio.Reader) {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("expected ready, got %q", line)
	}
	go io.Copy(io.Discard, r)
}
