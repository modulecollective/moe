//go:build linux

package cli

import "syscall"

// Linux uses the SVR4-style TCGETS/TCSETS ioctls; BSDs use TIOCGETA/
// TIOCSETA — see follow_tty_bsd.go.
const (
	ttyTermiosGet = syscall.TCGETS
	ttyTermiosSet = syscall.TCSETS
)
