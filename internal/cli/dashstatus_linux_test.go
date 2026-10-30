//go:build linux

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

type statusWatchWriter struct {
	bytes.Buffer
	terminal io.Writer
	cancel   context.CancelFunc
}

func (w *statusWatchWriter) Unwrap() io.Writer                 { return w.terminal }
func (w *statusWatchWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *statusWatchWriter) Write(p []byte) (int, error) {
	if string(p) == dashFramePost {
		w.cancel()
	}
	return w.Buffer.Write(p)
}

func TestDashWatchStatusOutsideViewportAndCleanup(t *testing.T) {
	root := newTestBureaucracy(t)
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size := [4]uint16{2, 8, 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size))); errno != 0 {
		t.Fatal(errno)
	}
	for k, v := range map[string]string{"REX_BLOCK": "b", "REX_SESSION": "s", "TERM": "xterm-256color", "TMUX": "", "STY": "", "MOE_SERVE_AGENT": ""} {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &statusWatchWriter{terminal: f, cancel: cancel}
	var stderr bytes.Buffer
	if code := watchDash(ctx, root, DashFilter{}, false, w, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	got := w.String()
	_, after, ok := strings.Cut(got, dashFramePost)
	clear := "\x1b]7501;state=clear:id=moe/dash\x1b\\"
	if !ok || !strings.Contains(after, "\x1b]7501;state=idle:id=moe/dash/activity:app=moe:title=") ||
		!strings.HasPrefix(got, clear) || !strings.HasSuffix(after, clear) || strings.Count(got, clear) != 2 {
		t.Fatalf("status clipped or cleanup missing: %q", got)
	}
	_, frame, _ := strings.Cut(got, dashFramePre)
	if strings.Contains(strings.Split(frame, dashFramePost)[0], "7501") {
		t.Fatal("OSC entered viewport")
	}
	// The same real watch path in a nested terminal emits no reports.
	t.Setenv("TMUX", "nested")
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	w = &statusWatchWriter{terminal: f, cancel: cancel}
	if code := watchDash(ctx, root, DashFilter{}, false, w, &stderr); code != 0 || strings.Contains(w.String(), "7501") {
		t.Fatalf("nested watch: exit=%d stdout=%q", code, w.String())
	}
}
