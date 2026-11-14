package cli

import (
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

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
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
		{name: "save only", command: "new-idea", key: "s"},
		{name: "empty board", command: "new-idea", key: "s", empty: true},
		{name: "sdlc licence", command: "new-idea", key: "g", tag: "sdlc"},
		{name: "design licence", command: "new-idea", key: "d", tag: "sdlc", design: true},
		{name: "filtered intent", command: "new-intent", hidden: true},
		{name: "completed idea", command: "new-i\t", key: "s"},
		{name: "completed intent", command: "new-i\t\t"},
		{name: "reverse completed intent", command: "new-i\x1b[Z", empty: true},
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
	for _, command := range []struct{ line, out string }{{"tag-sdlc", "done: tag sdlc"}, {"tag-design", "done: tag sdlc (design only)"}, {"untag", "done: untag"}} {
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

func TestUIKeyboardIdeaTagKeys(t *testing.T) {
	root, _ := controlFixture(t)
	for _, slug := range []string{"first", "second"} {
		if _, err := createIdea(root, "moe", slug, "# "+slug+"\n", "", trailers.Block{}); err != nil {
			t.Fatal(err)
		}
	}
	s := newUISource(root, DashFilter{WorkflowFilter: "idea"}, os.Stdout)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := takeBoard(t, s)
	if len(b.Items) != 2 {
		t.Fatalf("idea board = %+v", b.Items)
	}
	first, second := b.Items[0].Run, b.Items[1].Run
	c := startUIKeyboard(t, root, "--workflow", "idea")
	c.wait(t, "idea moe/"+first+" · in_progress")

	// Wait for a complete working frame followed by a completed frame
	// from this send. Old done messages repainted in console history
	// cannot satisfy a repeat's wait.
	sendTag := func(key, result string) {
		t.Helper()
		offset := len(c.text())
		c.send(t, key)
		until := time.Now().Add(5 * time.Second)
		for {
			frames := strings.Split(c.text()[offset:], "\x1b[?2026l")
			working := false
			for _, frame := range frames[:len(frames)-1] {
				if strings.Contains(frame, " · working: ") {
					working = true
				} else if working && strings.Contains(frame, result) {
					return
				}
			}
			if time.Now().After(until) {
				t.Fatalf("key %q never completed %q: %q", key, result, c.text()[offset:])
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	check := func(slug, workflow string, design bool) {
		t.Helper()
		md, err := run.Load(root, "moe", slug)
		if err != nil || md.PromoteTo != workflow || md.DesignOnly != design || md.Workflow != "idea" || md.Status != run.StatusInProgress {
			t.Fatalf("idea %s = %+v, %v", slug, md, err)
		}
		body, err := os.ReadFile(root + "/" + run.ContentPath("moe", slug, dash.IdeaDocID))
		if err != nil || string(body) != "# "+slug+"\n" {
			t.Fatalf("idea body %s = %q, %v", slug, body, err)
		}
	}
	for _, tc := range []struct {
		key, workflow, label string
		design, unchanged    bool
	}{
		{"g", "sdlc", "tag sdlc", false, false},
		{"g", "sdlc", "tag sdlc", false, true},
		{"d", "sdlc", "tag sdlc (design only)", true, false},
		{"d", "sdlc", "tag sdlc (design only)", true, true},
		{"g", "sdlc", "tag sdlc", false, false},
		{"u", "", "untag", false, false},
		{"u", "", "untag", false, true},
		{":tag-sdlc\r", "sdlc", "tag sdlc", false, false},
		{":tag-\t\t\r", "sdlc", "tag sdlc", false, true},
		{":tag-design\r", "sdlc", "tag sdlc (design only)", true, false},
		{":tag-\x1b[Z\x1b[Z\r", "sdlc", "tag sdlc (design only)", true, true},
		{":untag\r", "", "untag", false, false},
	} {
		before := gittest.HeadSHA(t, root)
		sendTag(tc.key, "done: "+tc.label+" moe/"+first)
		check(first, tc.workflow, tc.design)
		check(second, "", false)
		if after := gittest.HeadSHA(t, root); (before == after) != tc.unchanged {
			t.Fatalf("key %q unchanged=%v: HEAD %s → %s", tc.key, tc.unchanged, before, after)
		}
	}
	// The selection stayed on the first idea through all mutations and
	// refreshes. Move to the other row and set its independent licence.
	c.send(t, "j")
	c.wait(t, "idea moe/"+second+" · in_progress")
	sendTag("d", "done: tag sdlc (design only) moe/"+second)
	check(first, "", false)
	check(second, "sdlc", true)
	c.quit(t)
	entries, err := os.ReadDir(root + "/projects/moe/runs")
	if err != nil || len(entries) != 3 { // the existing SDLC run plus two ideas
		t.Fatalf("tag keys minted a run: %v, %v", entries, err)
	}
	refs, err := git.Output(root, "for-each-ref", "--format=%(refname)", "refs/heads/session/")
	if err != nil || strings.TrimSpace(refs) != "" {
		t.Fatalf("tag keys launched a session: %q, %v", refs, err)
	}
}

func TestUIKeyboardFailedCreateKeepsDraft(t *testing.T) {
	for _, target := range []string{"missing/new-idea", "moe/change-auth"} {
		t.Run(target, func(t *testing.T) {
			root, _ := controlFixture(t)
			c := startUIKeyboard(t, root)
			c.wait(t, "moe/change-auth")
			c.send(t, ":new-idea "+target+"\rprecious text\x13g")
			c.wait(t, "failed: create")
			c.wait(t, "not tagged: the save did not succeed")
			c.mu.Lock()
			c.output.Reset()
			c.mu.Unlock()
			c.send(t, ":new-idea "+target+"\r")
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

func TestUIKeyboardUppercaseRefusalAndLowercaseRetry(t *testing.T) {
	root, _ := controlFixture(t)
	c := startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	before := gittest.HeadSHA(t, root)
	c.send(t, ":new-idea moe/Bad\r")
	c.wait(t, "want <project>/<slug> with a lowercase-kebab slug")
	for _, slug := range []string{"Bad", "bad"} {
		if _, err := os.Stat(root + "/projects/moe/runs/" + slug); !os.IsNotExist(err) {
			t.Fatalf("uppercase refusal left %s: %v", slug, err)
		}
	}
	if after := gittest.HeadSHA(t, root); after != before {
		t.Fatal("uppercase refusal wrote to the journal")
	}
	c.send(t, ":new-idea moe/bad\r")
	c.wait(t, "editing new idea moe/bad")
	if _, err := os.Stat(root + "/projects/moe/runs/bad"); !os.IsNotExist(err) {
		t.Fatalf("Enter created before save: %v", err)
	}
	c.send(t, "operator body\x13")
	c.wait(t, "g  save + tag sdlc")
	c.send(t, "s")
	c.wait(t, "done: create idea moe/bad")
	c.quit(t)
	md, err := run.Load(root, "moe", "bad")
	if err != nil || md.Workflow != "idea" || md.Status != run.StatusInProgress || md.PromoteTo != "" || md.DesignOnly {
		t.Fatalf("lowercase capture = %+v, %v", md, err)
	}
	body, err := os.ReadFile(root + "/" + run.ContentPath("moe", "bad", dash.IdeaDocID))
	if err != nil || string(body) != "# bad\noperator body" {
		t.Fatalf("lowercase body = %q, %v", body, err)
	}
	if _, err := os.Stat(root + "/projects/moe/runs/Bad"); !os.IsNotExist(err) {
		t.Fatalf("retry created uppercase capture: %v", err)
	}
}

func TestUIKeyboardCancelledCreationWritesNothing(t *testing.T) {
	root, _ := controlFixture(t)
	c := startUIKeyboard(t, root)
	c.wait(t, "moe/change-auth")
	c.send(t, ":new-idea moe/cancel-me\rprivate text\x18y")
	c.wait(t, "draft discarded")
	c.quit(t)
	if _, err := run.Load(root, "moe", "cancel-me"); err == nil {
		t.Fatal("cancel created a capture")
	}
}

func TestUIKeyboardRemovedSpacedCommands(t *testing.T) {
	for _, command := range []string{"new idea moe/no-capture", "new intent moe/no-capture", "tag sdlc", "tag design"} {
		t.Run(command, func(t *testing.T) {
			root, _ := controlFixture(t)
			c := startUIKeyboard(t, root)
			c.wait(t, "moe/change-auth")
			before := gittest.HeadSHA(t, root)
			c.send(t, ":"+command+"\r")
			c.wait(t, "unknown command")
			c.quit(t) // an open draft would ask for confirmation instead
			if after := gittest.HeadSHA(t, root); after != before {
				t.Fatal("removed command wrote to the journal")
			}
			if _, err := run.Load(root, "moe", "no-capture"); err == nil {
				t.Fatal("removed command created a capture")
			}
		})
	}
}
