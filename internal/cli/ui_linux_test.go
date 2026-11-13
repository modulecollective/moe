package cli

import (
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

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// This helper runs the real UI composition in a test-owned terminal and
// bureaucracy. No moe executable or live Rex session is invoked.
func TestUIKeyboardHelper(t *testing.T) {
	if os.Getenv("MOE_UI_KEYBOARD_HELPER") != "1" {
		t.Skip("child helper")
	}
	os.Exit(runUI(strings.Fields(os.Getenv("MOE_UI_KEYBOARD_ARGS")), os.Stdout, os.Stderr))
}

type uiKeyboardChild struct {
	master *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	output strings.Builder
	done   chan error
}

func startUIKeyboard(t *testing.T, root string, args ...string) *uiKeyboardChild {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	ws := struct{ row, col, x, y uint16 }{30, 100, 0, 0}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		t.Fatal(e)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestUIKeyboardHelper$")
	cmd.Env = append(os.Environ(), "MOE_UI_KEYBOARD_HELPER=1", "MOE_HOME="+root, "MOE_UI_KEYBOARD_ARGS="+strings.Join(args, " "), "TERM=xterm-256color", "NO_COLOR=1", "REX_SESSION=", "REX_BLOCK=")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	c := &uiKeyboardChild{master: master, cmd: cmd, done: make(chan error, 1)}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- cmd.Wait() }()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			c.mu.Lock()
			c.output.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { cmd.Process.Kill(); master.Close(); <-readDone })
	c.wait(t, "MINISTRY OF EVERYTHING")
	return c
}

func (c *uiKeyboardChild) text() string { c.mu.Lock(); defer c.mu.Unlock(); return c.output.String() }
func (c *uiKeyboardChild) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(c.master, s); err != nil {
		t.Fatal(err)
	}
}
func (c *uiKeyboardChild) wait(t *testing.T, s string) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.text(), s) {
		if time.Now().After(until) {
			t.Fatalf("UI never showed %q: %q", s, c.text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (c *uiKeyboardChild) quit(t *testing.T) {
	t.Helper()
	c.send(t, "q")
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI did not quit")
	}
}

func TestUIKeyboardCreatesThroughWriters(t *testing.T) {
	for _, tc := range []struct {
		name, command, key, tag string
		design, hidden, empty   bool
	}{
		{name: "save only", command: "new idea", key: "s"},
		{name: "empty board", command: "new idea", key: "s", empty: true},
		{name: "sdlc licence", command: "new idea", key: "g", tag: "sdlc"},
		{name: "design licence", command: "new idea", key: "d", tag: "sdlc", design: true},
		{name: "filtered intent", command: "new intent", hidden: true},
		{name: "completed idea", command: "ne\t i\t", key: "s"},
		{name: "completed intent", command: "ne\t i\t\t"},
		{name: "reverse completed intent", command: "ne\t i\x1b[Z", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := controlFixture(t)
			if tc.empty {
				root = spawnFixture(t)
			}
			args := []string{}
			if tc.hidden {
				args = []string{"--workflow", "sdlc"}
			}
			c := startUIKeyboard(t, root, args...)
			if tc.empty {
				c.wait(t, "all projects")
			} else {
				c.wait(t, "moe/change-auth")
			}
			c.send(t, ":"+tc.command+" moe/from-keys\r")
			c.wait(t, "editing new")
			if _, err := run.Load(root, "moe", "from-keys"); err == nil {
				t.Fatal("Enter created before save")
			}
			c.send(t, "operator 世界/p-x\x1b\x7fwords\x13")
			if tc.key != "" {
				c.wait(t, "g  save + tag sdlc")
				c.send(t, tc.key)
			}
			c.wait(t, "done: create")
			if tc.tag != "" {
				c.wait(t, "done: tag")
			}
			if tc.hidden {
				c.wait(t, "hidden by current filters")
			}
			c.quit(t)
			md, err := run.Load(root, "moe", "from-keys")
			wantWorkflow := "intent"
			if tc.key != "" {
				wantWorkflow = "idea"
			}
			if err != nil || md.PromoteTo != tc.tag || md.DesignOnly != tc.design || md.Workflow != wantWorkflow {
				t.Fatalf("created %+v: %v", md, err)
			}
			doc, _ := dash.CaptureDocID(md.Workflow)
			body, err := os.ReadFile(root + "/" + run.ContentPath("moe", "from-keys", doc))
			if err != nil || string(body) != "# from-keys\noperator words" {
				t.Fatalf("operator text: %q, %v", body, err)
			}
		})
	}
}

func TestUIKeyboardInputsAndControls(t *testing.T) {
	root, _ := controlFixture(t)
	askOn(t, root, "moe", "change-auth", "Which store?")
	c := startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	c.send(t, "nuse sqlite\x13")
	c.wait(t, "note #")
	c.send(t, "apostgres\x13")
	c.wait(t, "answered #")
	c.send(t, ":pause\r")
	c.wait(t, "done: pause")
	c.quit(t)
	f, err := input.Load(root, "moe", "change-auth")
	if err != nil || len(f.Pending()) != 2 {
		t.Fatalf("input effects: %+v, %v", f, err)
	}
	if hold := controlRecord(t, root).Hold; hold == nil {
		t.Fatal("pause did not hold")
	}
	c = startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	c.send(t, ":resume\r")
	c.wait(t, "done: resume")
	c.quit(t)
	if controlRecord(t, root).Hold != nil {
		t.Fatal("resume did not release")
	}
	for _, verb := range []string{"handoff", "finish"} {
		root, _ := controlFixture(t)
		c := startUIKeyboard(t, root)
		c.wait(t, "moe/change-auth")
		c.send(t, ":"+verb+"\r")
		c.wait(t, "done: "+verb)
		c.quit(t)
		rec := controlRecord(t, root)
		if len(rec.Requests) != 1 || string(rec.Requests[0].Kind) != verb {
			t.Fatalf("%s receipt: %+v", verb, rec)
		}
	}
}

// Save through the editor, then read through the source's ordinary
// refresh and the scrolled terminal view. Restart has no draft to mask
// a missing persisted read, and delivery must not hide the text.
func TestUIKeyboardNoteReadback(t *testing.T) {
	root, _ := controlFixture(t)
	c := startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	note := "First saved line.\n\n" + strings.Repeat("saved filler\n", 20) + strings.Repeat("x", 150) + "\nEND_OF_SAVED_NOTE"
	c.send(t, "n"+strings.ReplaceAll(note, "\n", "\r")+"\x13")
	c.wait(t, "done: note")
	// Discard editor frames: only a subsequent details paint can prove
	// readback. The save result itself never echoes the note body.
	c.mu.Lock()
	c.output.Reset()
	c.mu.Unlock()
	c.send(t, "\t\x1b[H")
	c.wait(t, "1 pending note")
	c.send(t, "\x1b[F")
	c.wait(t, "END_OF_SAVED_NOTE")
	c.quit(t)
	f, err := input.Load(root, "moe", "change-auth")
	if err != nil || len(f.Notes) != 1 || f.Notes[0].Text != note || !f.Notes[0].Pending() {
		t.Fatalf("saved note = %+v, %v", f, err)
	}
	if err := input.MarkDelivered(root, "moe", "change-auth", "code", []int{1}, ""); err != nil {
		t.Fatal(err)
	}
	c = startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	c.send(t, "\t\x1b[H\x1b[B\x1b[B")
	c.wait(t, "note #1 · delivered to code")
	c.send(t, "\x1b[F")
	c.wait(t, "END_OF_SAVED_NOTE")
	c.quit(t)
}

func TestUIKeyboardCaptureEditLicenceAndClose(t *testing.T) {
	root, _ := controlFixture(t)
	if _, err := createIdea(root, "moe", "capture", "# Capture\n", "", trailers.Block{}); err != nil {
		t.Fatal(err)
	}
	c := startUIKeyboard(t, root, "--workflow", "idea")
	c.wait(t, "moe/capture")
	c.send(t, "eoperator text\x13s")
	c.wait(t, "done: edit")
	for _, command := range []struct{ line, out string }{{"tag sdlc", "done: tag sdlc"}, {"tag design", "done: tag sdlc (design only)"}, {"untag", "done: untag"}} {
		c.send(t, ":"+command.line+"\r")
		c.wait(t, command.out)
	}
	c.send(t, ":close\rn")
	c.wait(t, "cancelled")
	if md, err := run.Load(root, "moe", "capture"); err != nil || md.Status != run.StatusInProgress {
		t.Fatal("cancelled close mutated capture")
	}
	c.send(t, ":close\ry")
	c.wait(t, "done: close")
	c.quit(t)
	md, err := run.Load(root, "moe", "capture")
	if err != nil || md.Status != run.StatusClosed || md.PromoteTo != "" {
		t.Fatalf("closed capture: %+v %v", md, err)
	}
	body, err := os.ReadFile(root + "/" + run.ContentPath("moe", "capture", dash.IdeaDocID))
	if err != nil || string(body) != "# Capture\noperator text" {
		t.Fatalf("edited body: %q %v", body, err)
	}
}

func TestUIKeyboardFailedCreateKeepsDraft(t *testing.T) {
	for _, target := range []string{"missing/new-idea", "moe/change-auth"} {
		t.Run(target, func(t *testing.T) {
			root, _ := controlFixture(t)
			c := startUIKeyboard(t, root)
			c.wait(t, "moe/change-auth")
			c.send(t, ":new idea "+target+"\rprecious text\x13g")
			c.wait(t, "failed: create")
			c.wait(t, "not tagged: the save did not succeed")
			c.send(t, ":new idea "+target+"\r")
			c.wait(t, "precious text")
			c.send(t, "\x18y") // discard the retained draft
			c.wait(t, "draft discarded")
			c.quit(t)
			if md, err := run.Load(root, "moe", "change-auth"); err != nil || md.Workflow != "sdlc" || md.PromoteTo != "" {
				t.Fatalf("failed creation mutated original: %+v, %v", md, err)
			}
			if _, err := run.Load(root, "missing", "new-idea"); err == nil {
				t.Fatal("unknown project created")
			}
		})
	}
}

func TestUIKeyboardCancelledCreationWritesNothing(t *testing.T) {
	root, _ := controlFixture(t)
	c := startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	c.send(t, ":new idea moe/cancel-me\rprivate text\x18y")
	c.wait(t, "draft discarded")
	c.quit(t)
	if _, err := run.Load(root, "moe", "cancel-me"); err == nil {
		t.Fatal("cancel created a capture")
	}
}

func TestUIKeyboardRemovedCaptureAliases(t *testing.T) {
	for _, alias := range []string{"new-idea", "new-intent"} {
		t.Run(alias, func(t *testing.T) {
			root, _ := controlFixture(t)
			c := startUIKeyboard(t, root)
			c.wait(t, "moe/change-auth")
			c.send(t, ":"+alias+" moe/no-capture\r")
			c.wait(t, "unknown command")
			c.quit(t) // an open draft would ask for confirmation instead
			if _, err := run.Load(root, "moe", "no-capture"); err == nil {
				t.Fatal("removed alias created a capture")
			}
		})
	}
}
