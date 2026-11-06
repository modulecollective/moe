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
// A plain goroutine blocked in os.Stdin.Read can't be cancelled, and one
// left behind would take the interactive agent's first keystrokes. So
// the reader runs on a dup of stdin switched to non-blocking, which Go
// registers with its poller; closing that file wakes the pending read
// and the goroutine exits. The non-blocking flag lives on the shared
// open file description, so stop restores it before the agent inherits
// stdin.
func openCooldownControls() (<-chan cooldownControl, func()) {
	if !cooldownControlsAttached() {
		return nil, func() {}
	}
	stdinFD := os.Stdin.Fd()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, stdinFD, syscall.F_GETFL, 0)
	if errno != 0 {
		return nil, func() {}
	}
	fd, err := syscall.Dup(int(stdinFD))
	if err != nil {
		return nil, func() {}
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, func() {}
	}
	f := os.NewFile(uintptr(fd), "cooldown-stdin")
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
		if flags&syscall.O_NONBLOCK == 0 {
			_ = syscall.SetNonblock(int(stdinFD), false)
		}
	}
}
