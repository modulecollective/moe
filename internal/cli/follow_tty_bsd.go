//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import "syscall"

// BSD-family termios ioctl. Linux uses TCGETS instead; see
// follow_tty_linux.go.
const ttyTermiosGet = syscall.TIOCGETA
