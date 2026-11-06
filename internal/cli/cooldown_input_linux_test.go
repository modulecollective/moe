package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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
	// Explicit O_NONBLOCK prevents Go from owning the flag and clearing it
	// when os/exec calls File.Fd. Each test sets its desired initial mode.
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// Session and controlling-terminal changes belong in a child, never the test
// runner. The retained slave shares stdin's open file description, like the
// parent's shell would, but reports travel over a separate pipe.
type cooldownChild struct {
	master  *os.File
	stdinFD uintptr
	flags   uintptr
	reports *os.File
	reader  *bufio.Reader
	cmd     *exec.Cmd
	exited  chan struct{}
	waitErr error
}

func cooldownFlags(t *testing.T, fd uintptr) uintptr {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags
}

func startCooldownChild(t *testing.T, mode string, nonblock bool) *cooldownChild {
	t.Helper()
	master, slave := cooldownPTY(t)
	// Cache before setting flags: another File.Fd call can restore blocking
	// mode and hide the very mutation this test is meant to detect.
	stdinFD := slave.Fd()
	if err := syscall.SetNonblock(int(stdinFD), nonblock); err != nil {
		t.Fatal(err)
	}
	reports, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reports.Close() })
	defer writer.Close()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestCooldownTerminalHelper$")
	cmd.Env = append(os.Environ(), "MOE_COOLDOWN_TEST="+mode)
	cmd.Stdin = slave
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// fd 3 carries reports; fd 4 supplies the controlling tty even when
	// stdin is a pipe, so that suppression must come from the guard.
	cmd.ExtraFiles = []*os.File{writer, slave}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: mode != "no-terminal", Ctty: 4}
	if mode == "pipe" {
		input, inputWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { input.Close(); inputWriter.Close() })
		cmd.Stdin = input
	}
	child := &cooldownChild{
		master: master, stdinFD: stdinFD, flags: cooldownFlags(t, stdinFD),
		reports: reports, reader: bufio.NewReader(reports), cmd: cmd,
		exited: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		child.waitErr = cmd.Wait()
		close(child.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-child.exited:
		case <-time.After(5 * time.Second):
			t.Error("cooldown child did not exit after kill")
		}
	})
	return child
}

func (c *cooldownChild) expect(t *testing.T, want string) {
	t.Helper()
	if err := c.reports.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := c.reader.ReadString('\n')
	if err != nil || got != want+"\n" {
		t.Fatalf("cooldown child report = %q, %v; want %q", got, err, want)
	}
}

func (c *cooldownChild) assertFlags(t *testing.T, when string) {
	t.Helper()
	if got := cooldownFlags(t, c.stdinFD); got != c.flags {
		t.Errorf("stdin flags %s = %#x; want %#x", when, got, c.flags)
	}
}

func (c *cooldownChild) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-c.exited:
		return c.waitErr
	case <-time.After(5 * time.Second):
		t.Fatal("cooldown child did not exit")
		return nil
	}
}

func TestCooldownStdinFlagsSurviveKill(t *testing.T) {
	for _, nonblock := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonblock=%t", nonblock), func(t *testing.T) {
			child := startCooldownChild(t, "ownership", nonblock)
			child.expect(t, "opened")
			child.assertFlags(t, "while controls are active")
			if err := child.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.wait(t); err == nil {
				t.Fatal("child exited successfully instead of being killed")
			}
			status := child.cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child exit = %v; want SIGKILL", status)
			}
			child.assertFlags(t, "after SIGKILL")
		})
	}
}

func TestCooldownTerminalReaderStopsBeforeAgentInput(t *testing.T) {
	child := startCooldownChild(t, "handoff", false)
	child.expect(t, "opened")
	child.assertFlags(t, "while controls are active")
	if _, err := child.master.WriteString("c\n"); err != nil {
		t.Fatal(err)
	}
	child.expect(t, "continued")
	child.expect(t, "stopped")
	child.assertFlags(t, "after normal stop")
	// Send only after close-and-join is acknowledged, as dispatch would.
	if _, err := child.master.WriteString("agent input\n"); err != nil {
		t.Fatal(err)
	}
	child.expect(t, "input \"agent input\\n\"")
	if err := child.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestCooldownServePTYNeverOpensControls(t *testing.T) {
	child := startCooldownChild(t, "serve", false)
	child.expect(t, "disabled")
	if err := child.wait(t); err != nil {
		t.Fatal(err)
	}
	child.assertFlags(t, "after serve suppression")
}

func TestCooldownPipedStdinNeverOpensControls(t *testing.T) {
	child := startCooldownChild(t, "pipe", false)
	child.expect(t, "disabled")
	if err := child.wait(t); err != nil {
		t.Fatal(err)
	}
	child.assertFlags(t, "after pipe suppression")
}

func TestCooldownWithoutControllingTerminal(t *testing.T) {
	child := startCooldownChild(t, "no-terminal", false)
	child.expect(t, "disabled")
	if err := child.wait(t); err != nil {
		t.Fatal(err)
	}
	child.assertFlags(t, "after failed terminal open")
}

func TestCooldownTerminalHelper(t *testing.T) {
	mode := os.Getenv("MOE_COOLDOWN_TEST")
	if mode == "" {
		return
	}
	t.Setenv("MOE_SERVE_AGENT", "")
	if mode == "serve" {
		t.Setenv("MOE_SERVE_AGENT", "1")
	}
	reports := os.NewFile(3, "cooldown-reports")
	defer reports.Close()
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if mode == "no-terminal" {
		if err == nil {
			tty.Close()
			t.Fatal("child unexpectedly has a controlling terminal")
		}
		if !cooldownControlsAttached() {
			t.Fatal("terminal stdin must pass the eligibility guard")
		}
	} else {
		if err != nil {
			t.Fatalf("child lacks a controlling terminal: %v", err)
		}
		tty.Close()
	}
	controls, stop := openCooldownControls()
	defer stop()
	if mode == "serve" || mode == "pipe" || mode == "no-terminal" {
		if controls != nil {
			t.Fatal("ineligible child acquired controls")
		}
		stop()
		fmt.Fprintln(reports, "disabled")
		return
	}
	if controls == nil {
		t.Fatal("no terminal controls")
	}
	fmt.Fprintln(reports, "opened")
	if mode == "ownership" {
		time.Sleep(30 * time.Second)
		t.Fatal("parent did not kill child")
	}
	select {
	case ctl := <-controls:
		if ctl != controlContinue {
			t.Fatalf("control = %v; want continue", ctl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control reader did not receive c")
	}
	fmt.Fprintln(reports, "continued")
	stop()
	stop()
	fmt.Fprintln(reports, "stopped")
	b := make([]byte, 64)
	n, err := os.Stdin.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(reports, "input %q\n", b[:n])
}
