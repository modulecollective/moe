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

func terminalSize(f *os.File) (int, int, error) {
	var size terminalWinsize
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&size)),
	); errno != 0 {
		return 0, 0, fmt.Errorf("terminal sizing: %w", errno)
	}
	if size.row == 0 || size.col == 0 {
		return 0, 0, fmt.Errorf("terminal sizing: invalid size %dx%d", size.col, size.row)
	}
	return int(size.row), int(size.col), nil
}
