package cli

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func cooldownPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatal(errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func TestCooldownTerminalReaderStopsBeforeAgentInput(t *testing.T) {
	master, slave := cooldownPTY(t)
	previous := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = previous })
	t.Setenv("MOE_SERVE_AGENT", "")
	stdinFD := slave.Fd()
	controls, stop := openCooldownControls()
	defer stop()
	if controls == nil {
		t.Fatal("no terminal controls")
	}
	if _, err := master.WriteString("c\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case ctl := <-controls:
		if ctl != controlContinue {
			t.Fatal(ctl)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control reader did not receive c")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop while waiting for input")
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, stdinFD, syscall.F_GETFL, 0)
	if errno != 0 || flags&syscall.O_NONBLOCK != 0 {
		t.Fatalf("stdin flags not restored: %x %v", flags, errno)
	}
	// The next input belongs to the agent, after stop has joined the reader.
	if _, err := master.WriteString("agent input\n"); err != nil {
		t.Fatal(err)
	}
	input := make(chan string, 1)
	go func() { b := make([]byte, 64); n, _ := slave.Read(b); input <- string(b[:n]) }()
	select {
	case got := <-input:
		if got != "agent input\n" {
			t.Fatalf("input %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cooldown reader stole agent input")
	}
}

func TestCooldownServePTYNeverOpensControls(t *testing.T) {
	_, slave := cooldownPTY(t)
	previous := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = previous })
	t.Setenv("MOE_SERVE_AGENT", "1")
	controls, stop := openCooldownControls()
	defer stop()
	if controls != nil {
		t.Fatal("Rex/serve PTY acquired an interactive reader")
	}
}
