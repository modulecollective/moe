//go:build unix

package cli

import (
	"syscall"
	"unsafe"
)

// Tty handover for the pager. Setpgid:true on the pager's SysProcAttr
// only puts the pager in its own process group; without also moving
// the controlling tty's foreground PG to it, the kernel keeps routing
// tty input to moe and stops the pager with SIGTTIN as soon as it
// reads. Together with `signal.Ignore(syscall.SIGTTOU)` (so the
// reciprocal SIGTTOU on tcsetpgrp doesn't stop *moe*), this is the
// canonical "shell builder" sequence.
//
// Termios save/restore is the SIGKILL escape hatch: less restores
// termios on clean exit and on SIGTERM, but a SIGKILL'd less leaves
// the tty in raw mode. We snapshot before spawning so the SIGKILL
// path can put the tty back into the cooked state the operator
// started in.

// tcgetpgrp returns the foreground process group of the terminal
// referenced by fd. Returns ENOTTY when fd isn't a tty.
func tcgetpgrp(fd int) (int, error) {
	var pgid int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgid)))
	if errno != 0 {
		return 0, errno
	}
	return int(pgid), nil
}

// tcsetpgrp sets the foreground process group of the terminal
// referenced by fd to pgid.
func tcsetpgrp(fd, pgid int) error {
	p := int32(pgid)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&p)))
	if errno != 0 {
		return errno
	}
	return nil
}

// getTermios snapshots the current termios on fd. Returns ENOTTY
// when fd isn't a tty.
func getTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(ttyTermiosGet), uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return nil, errno
	}
	return &t, nil
}

// setTermios restores fd's termios from t.
func setTermios(fd int, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(ttyTermiosSet), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
