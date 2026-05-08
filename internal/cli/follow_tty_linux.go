//go:build linux

package cli

import "syscall"

// Linux uses the SVR4-style TCGETS ioctl; BSDs use TIOCGETA — see
// follow_tty_bsd.go.
const ttyTermiosGet = syscall.TCGETS
