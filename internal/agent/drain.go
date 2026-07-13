package agent

import (
	"context"
	"io"
	"time"
)

// drainGrace bounds how long DrainThenWait waits for the stdout drain to
// finish after the context deadline fires before it force-closes the
// pipe read end. The group SIGKILL (SetProcessGroup) normally EOFs the
// pipe near-instantly, so this only matters for the pathological case of
// a process that escaped its group (a setsid'ing daemon) while holding
// the agent's stdout: without the force-close the headless cascade would
// wedge forever, defeating the whole point of the turn timeout. Not
// configurable — no knobs.
const drainGrace = 5 * time.Second

// DrainThenWait waits for the stdout-drain goroutine to finish before
// reaping the child — the order os/exec's StdoutPipe doc requires ("it
// is incorrect to call Wait before all reads from the pipe have
// completed"). The old order (Wait, then <-done) let cmd.Wait close the
// pipe read end on reap, dropping events still buffered in the pipe
// nondeterministically — including the system/init (claude) /
// thread.started (codex) event that carries the session id.
//
// done is closed by the caller when the drain goroutine returns. pipe is
// the child's StdoutPipe; on the deadline path it's force-closed to
// unblock a scanner stuck on a pipe the group kill couldn't EOF (the
// scanner surfaces ErrClosed, which the progress readers already swallow
// silently). c is the started command to reap.
//
// The deadline path only arms when ctx is cancellable: with Timeout==0
// the executor passes context.Background(), whose Done() is nil and
// never selects, so a no-deadline turn blocks on the drain exactly as
// the doc wants and can't reach the grace force-close.
func DrainThenWait(ctx context.Context, done <-chan struct{}, pipe io.Closer, c *Command) error {
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(drainGrace):
			pipe.Close() // force the scanner off a pipe the group kill couldn't EOF
			<-done
		}
	}
	return c.Wait()
}
