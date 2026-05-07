//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import "syscall"

// BSD-family termios ioctls. Linux uses TCGETS/TCSETS instead;
// see follow_tty_linux.go.
const (
	ttyTermiosGet = syscall.TIOCGETA
	ttyTermiosSet = syscall.TIOCSETA
)
