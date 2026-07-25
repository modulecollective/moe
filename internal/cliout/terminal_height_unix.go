//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cliout

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type terminalWinsize struct {
	row, col, xpixel, ypixel uint16
}

func terminalHeight(f *os.File) (int, error) {
	var size terminalWinsize
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&size)),
	); errno != 0 {
		return 0, fmt.Errorf("terminal sizing: %w", errno)
	}
	if size.row == 0 {
		return 0, fmt.Errorf("terminal sizing: zero rows")
	}
	return int(size.row), nil
}
