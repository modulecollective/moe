//go:build unix

package cli

import (
	"syscall"
	"unsafe"
)

// getTermios snapshots the current termios on fd. Returns ENOTTY when
// fd isn't a tty.
//
// We use this to detect when hunk's UI has finished its teardown — the
// alt-screen exit + termios restore sequence runs whether the operator
// quit by typing `q`, by raw-mode ^C (a 0x03 byte to hunk's stdin), or
// by signal ^C in cooked mode. Only the last one shows up to moe via
// signal.Notify; the other two are invisible from this side. Polling
// termios for the raw → cooked transition lets us notice the byte-^C
// case so we can SIGINT hunk's process out of its post-UI hang.
func getTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(ttyTermiosGet), uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return nil, errno
	}
	return &t, nil
}
