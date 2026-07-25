//go:build linux

package cliout

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

func TestTerminalHeightReadsCurrentPTYSizeThroughWrapper(t *testing.T) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	setSize := func(rows uint16) {
		t.Helper()
		size := terminalWinsize{row: rows, col: 120}
		if _, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			f.Fd(),
			syscall.TIOCSWINSZ,
			uintptr(unsafe.Pointer(&size)),
		); errno != 0 {
			t.Fatalf("set pty size: %v", errno)
		}
	}

	for _, rows := range []uint16{37, 19} {
		setSize(rows)
		got, err := TerminalHeight(unwrapWriter{w: f})
		if err != nil {
			t.Fatalf("TerminalHeight after resize to %d: %v", rows, err)
		}
		if got != int(rows) {
			t.Fatalf("TerminalHeight after resize=%d want %d", got, rows)
		}
	}
}
