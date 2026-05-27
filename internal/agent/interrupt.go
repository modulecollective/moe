package agent

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
)

// ErrInterrupted is the sentinel Wait returns when an os.Interrupt
// landed while the child was running and the child still exited zero.
// Non-zero child exits propagate verbatim — they already carry the
// failure shape callers expect (*exec.ExitError with the real code).
var ErrInterrupted = errors.New("agent: interrupted")

// Command is the wrapper StartCommand returns. The only intended
// caller-visible method is Wait, which mirrors *exec.Cmd.Wait but
// substitutes ErrInterrupted when the child exited zero after an
// observed Ctrl-C at the moe boundary.
type Command struct {
	cmd         *exec.Cmd
	stop        func()
	watchDone   chan struct{}
	interrupted atomic.Bool
}

// StartCommand starts cmd under a scoped os.Interrupt watcher. The
// watcher exists for exactly the lifetime of the subprocess — from
// Start through Wait — so the helper does not perturb moe's
// process-level signal disposition outside of an active backend turn.
//
// On start failure the watcher is torn down and the start error is
// returned verbatim.
func StartCommand(cmd *exec.Cmd) (*Command, error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	stop := func() {
		signal.Stop(sigCh)
		close(sigCh)
	}
	return startCommand(cmd, sigCh, stop)
}

// startCommand is the inner form used by StartCommand and by tests:
// the signal source and its stop function are injected so tests can
// drive the watcher without sending a real SIGINT to the test binary.
func startCommand(cmd *exec.Cmd, sigCh <-chan os.Signal, stop func()) (*Command, error) {
	if err := cmd.Start(); err != nil {
		stop()
		return nil, err
	}
	c := &Command{
		cmd:       cmd,
		stop:      stop,
		watchDone: make(chan struct{}),
	}
	go c.watch(sigCh)
	return c, nil
}

// watch forwards each os.Interrupt arrival to the child and records
// that an interrupt was observed. It exits when sigCh is closed (which
// Wait does via stop after the child exits).
func (c *Command) watch(sigCh <-chan os.Signal) {
	defer close(c.watchDone)
	for range sigCh {
		c.interrupted.Store(true)
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Signal(os.Interrupt)
		}
	}
}

// Wait waits for the child to exit and returns:
//   - the child's process error, when non-nil (preserving
//     *exec.ExitError so callers can still inspect the exit code or
//     kill signal);
//   - ErrInterrupted, when the child exited zero but an os.Interrupt
//     landed during the wait window;
//   - nil otherwise.
func (c *Command) Wait() error {
	waitErr := c.cmd.Wait()
	c.stop()
	<-c.watchDone
	if waitErr != nil {
		return waitErr
	}
	if c.interrupted.Load() {
		return ErrInterrupted
	}
	return nil
}
