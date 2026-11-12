//go:build linux || darwin

package ui

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type termState struct{ t syscall.Termios }

// openTTY opens the controlling terminal privately. Non-blocking puts it
// on Go's poller, so closing it wakes a pending read: the reader can be
// cancelled and joined, as the cooldown reader is.
func openTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}

func getTermios(f *os.File) (termState, error) {
	var s termState
	if err := termiosIoctl(f, ioctlGetTermios, &s.t); err != nil {
		return s, fmt.Errorf("read terminal mode: %w", err)
	}
	return s, nil
}

func setTermios(f *os.File, s termState) error {
	if err := termiosIoctl(f, ioctlSetTermios, &s.t); err != nil {
		return fmt.Errorf("set terminal mode: %w", err)
	}
	return nil
}

func termiosIoctl(f *os.File, req uint, t *syscall.Termios) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(t)))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// rawTermios is the console's mode: no echo, no line editing, no signal
// keys (Ctrl-C and Ctrl-Z arrive as bytes and are handled explicitly)
// and no software flow control, so Ctrl-S reaches the editor as submit
// instead of freezing output. Output processing stays on.
func rawTermios(s termState) termState {
	t := s.t
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return termState{t}
}
