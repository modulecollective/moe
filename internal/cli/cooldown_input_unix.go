//go:build unix

package cli

import (
	"bufio"
	"errors"
	"io"
	"os"
	"syscall"
)

// openCooldownControls reads operator controls from the attached
// terminal while a stage waits, and guarantees the reader is gone when
// stop returns.
//
// A separate /dev/tty open owns its non-blocking flag, leaving inherited
// stdin unchanged even if the process is killed. Go registers this file
// with its poller, so closing it wakes a pending read. Stop must also
// join the reader before dispatch so it cannot take the agent's input.
func openCooldownControls() (<-chan cooldownControl, func()) {
	if !cooldownControlsAttached() {
		return nil, func() {}
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, func() {}
	}
	ch := make(chan cooldownControl, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readCooldownControls(f, ch, os.Stderr)
	}()
	return ch, func() {
		_ = f.Close()
		<-done
	}
}

// readCooldownControls feeds parsed control lines from r into ch until r
// ends. Closing the tty is how stop ends it, so ErrClosed is quiet; any
// other read failure means the controls are dead for the rest of the
// wait, and the operator is told rather than left typing into nothing.
func readCooldownControls(r io.Reader, ch chan<- cooldownControl, stderr io.Writer) {
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		if ctl, ok := parseCooldownControl(lines.Text()); ok {
			select {
			case ch <- ctl:
			default:
			}
		}
	}
	if err := lines.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		moePrintf(stderr, "cooldown: terminal controls stopped (%v); Ctrl-C still cancels\n", err)
	}
}
