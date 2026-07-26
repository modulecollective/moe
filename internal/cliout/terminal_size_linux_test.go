//go:build linux

package cliout

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

func TestTerminalSizeReadsCurrentPTYSizeThroughWrapper(t *testing.T) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	setSize := func(rows, columns uint16) {
		t.Helper()
		size := terminalWinsize{row: rows, col: columns}
		if _, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			f.Fd(),
			syscall.TIOCSWINSZ,
			uintptr(unsafe.Pointer(&size)),
		); errno != 0 {
			t.Fatalf("set pty size: %v", errno)
		}
	}

	for _, size := range []struct {
		rows, columns uint16
	}{
		{rows: 37, columns: 120},
		{rows: 19, columns: 73},
	} {
		setSize(size.rows, size.columns)
		rows, columns, err := TerminalSize(unwrapWriter{w: f})
		if err != nil {
			t.Fatalf("TerminalSize after resize to %dx%d: %v", size.columns, size.rows, err)
		}
		if rows != int(size.rows) || columns != int(size.columns) {
			t.Fatalf("TerminalSize after resize=(%d, %d) want (%d, %d)",
				rows, columns, size.rows, size.columns)
		}
	}
}
