package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
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
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	ws := struct{ row, col, x, y uint16 }{30, 100, 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		t.Fatal(errno)
	}
	return master, slave
}

// TestUIPTYHelper is the child: the console on a real pty, reporting each
// action it is asked to make on fd 3.
func TestUIPTYHelper(t *testing.T) {
	if os.Getenv("MOE_UI_PTY") != "1" {
		t.Skip("child helper")
	}
	reports := os.NewFile(3, "reports")
	boards := make(chan Board, 1)
	boards <- board(1, run("p", "a"))
	cfg := Config{
		Boards: boards,
		Do: func(a Action) Result {
			fmt.Fprintf(reports, "%s %q\n", a.describe(), a.Text)
			return Result{OK: true, Message: "done"}
		},
	}
	if os.Getenv("MOE_UI_PTY_PANIC") == "1" {
		// Report runs on the loop goroutine, as a model panic would.
		cfg.Report = func(io.Writer, Board) error { panic("probe") }
	}
	err := Run(context.Background(), cfg, os.Stdout, os.Stderr)
	fmt.Fprintf(reports, "exit %v\n", err)
	os.Exit(0)
}

type ptyChild struct {
	before  termState // the terminal before the console started
	master  *os.File
	slave   *os.File
	cmd     *exec.Cmd
	reports *bufio.Reader
	mu      sync.Mutex
	out     strings.Builder
	exited  chan struct{}
}

func startPTYChild(t *testing.T, env ...string) *ptyChild {
	t.Helper()
	master, slave := openPTY(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestUIPTYHelper$")
	cmd.Env = append(os.Environ(), "MOE_UI_PTY=1", "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	c := &ptyChild{master: master, slave: slave, cmd: cmd, reports: bufio.NewReader(r), exited: make(chan struct{})}
	if c.before, err = getTermios(slave); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	go func() { cmd.Wait(); close(c.exited) }()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			c.mu.Lock()
			c.out.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-c.exited
	})
	return c
}

func (c *ptyChild) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func (c *ptyChild) waitOut(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.output(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q in %q", s, c.output())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *ptyChild) report(t *testing.T) string {
	t.Helper()
	line, err := c.reports.ReadString('\n')
	if err != nil {
		t.Fatalf("report: %v (output %q)", err, c.output())
	}
	return strings.TrimSuffix(line, "\n")
}

func (c *ptyChild) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("console did not exit")
	}
}

// The console on a real terminal: raw mode with flow control off, so
// Ctrl-S submits; typed and pasted text arrive intact; quitting restores
// the terminal exactly.
func TestRunOnPTY(t *testing.T) {
	c := startPTYChild(t)
	before := c.before
	c.waitOut(t, "\x1b[?1049h")
	c.waitOut(t, "p/a")
	raw, err := getTermios(c.slave)
	if err != nil {
		t.Fatal(err)
	}
	if raw.t.Iflag&syscall.IXON != 0 || raw.t.Lflag&(syscall.ICANON|syscall.ECHO|syscall.ISIG) != 0 {
		t.Fatalf("not raw: iflag %#x lflag %#x", raw.t.Iflag, raw.t.Lflag)
	}
	c.master.Write([]byte("nhé "))
	c.master.Write([]byte("\x1b[200~pasted\nq:finish\x1b[201~"))
	c.master.Write([]byte("\x13"))
	if got := c.report(t); got != `note to p/a "hé pasted\nq:finish"` {
		t.Fatalf("report = %s", got)
	}
	c.waitOut(t, "done")
	c.master.Write([]byte("q"))
	if got := c.report(t); got != "exit <nil>" {
		t.Fatalf("exit = %s", got)
	}
	c.wait(t)
	after, err := getTermios(c.slave)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("termios not restored:\nbefore %+v\nafter  %+v", before.t, after.t)
	}
	if out := c.output(); !strings.Contains(out, leaveModes) {
		t.Fatalf("screen modes not restored: %q", out[max(len(out)-80, 0):])
	}
}

// A terminating signal restores the terminal too.
func TestRunOnPTYRestoresOnSignal(t *testing.T) {
	c := startPTYChild(t)
	before := c.before
	c.waitOut(t, "p/a")
	c.cmd.Process.Signal(syscall.SIGTERM)
	if got := c.report(t); got != "exit <nil>" {
		t.Fatalf("exit = %s", got)
	}
	c.wait(t)
	after, _ := getTermios(c.slave)
	if after != before || !strings.Contains(c.output(), leaveModes) {
		t.Fatal("signal exit did not restore the terminal")
	}
}

// Without a terminal on stdout the console refuses before touching
// anything.
func TestRunRefusesPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := Run(context.Background(), Config{}, w, os.Stderr); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("pipe = %v", err)
	}
	t.Setenv("TERM", "dumb")
	_, slave := openPTY(t)
	if err := Run(context.Background(), Config{}, slave, os.Stderr); err == nil || !strings.Contains(err.Error(), "TERM") {
		t.Fatalf("dumb = %v", err)
	}
}

// Ctrl-Z gives the terminal back before stopping; continuing re-enters
// and repaints.
func TestRunOnPTYSuspendContinue(t *testing.T) {
	c := startPTYChild(t)
	before := c.before
	c.waitOut(t, "p/a")
	c.master.Write([]byte("\x1a"))
	stat := fmt.Sprintf("/proc/%d/stat", c.cmd.Process.Pid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(stat)
		if f := strings.Fields(string(b)); len(f) > 2 && f[2] == "T" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("console did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mid, _ := getTermios(c.slave); mid != before {
		t.Fatalf("stopped with the terminal still raw:\n%+v\n%+v\n%q", before.t, mid.t, c.output())
	}
	enters := strings.Count(c.output(), "\x1b[?1049h")
	c.cmd.Process.Signal(syscall.SIGCONT)
	deadline = time.Now().Add(5 * time.Second)
	for strings.Count(c.output(), "\x1b[?1049h") == enters {
		if time.Now().After(deadline) {
			t.Fatal("did not re-enter after continue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if raw, _ := getTermios(c.slave); raw.t.Lflag&syscall.ICANON != 0 {
		t.Fatal("continued without raw mode")
	}
	c.master.Write([]byte("q"))
	if got := c.report(t); got != "exit <nil>" {
		t.Fatalf("exit = %s", got)
	}
}

// The idea save dialogue on a real terminal: keys it does not name
// leave it open, and the save still goes through afterwards.
func TestRunOnPTYSaveDialogue(t *testing.T) {
	c := startPTYChild(t)
	c.waitOut(t, "p/a")
	c.master.Write([]byte(":new idea p/i\r"))
	c.waitOut(t, "editing new idea p/i")
	c.master.Write([]byte("\x13"))
	c.waitOut(t, "save + tag sdlc")
	c.master.Write([]byte("\x1b[A\t\x13x"))
	c.master.Write([]byte("s"))
	if got := c.report(t); got != `create idea p/i "# i\n"` {
		t.Fatalf("report = %s", got)
	}
	c.master.Write([]byte("q"))
	if got := c.report(t); got != "exit <nil>" {
		t.Fatalf("exit = %s", got)
	}
}

// A panic on the loop goroutine still hands the terminal back.
func TestRunOnPTYRestoresOnPanic(t *testing.T) {
	c := startPTYChild(t, "MOE_UI_PTY_PANIC=1")
	c.wait(t)
	after, err := getTermios(c.slave)
	if err != nil {
		t.Fatal(err)
	}
	if after != c.before {
		t.Fatalf("termios not restored after a panic:\nbefore %+v\nafter  %+v", c.before.t, after.t)
	}
	if out := c.output(); !strings.Contains(out, "probe") || !strings.Contains(out, leaveModes) {
		t.Fatalf("did not panic and restore: %q", out[max(len(out)-200, 0):])
	}
}
