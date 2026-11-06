//go:build unix

package cli

import (
	"bufio"
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
		lines := bufio.NewScanner(f)
		for lines.Scan() {
			if ctl, ok := parseCooldownControl(lines.Text()); ok {
				select {
				case ch <- ctl:
				default:
				}
			}
		}
	}()
	return ch, func() {
		_ = f.Close()
		<-done
	}
}
