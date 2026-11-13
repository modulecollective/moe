package ui

import (
	"fmt"
	"strings"
	"testing"
)

// Drive the production decoder before the model, including command Enter.
func keyboard(m *model, text string) effect {
	var d decoder
	var eff effect
	for _, k := range d.feed([]byte(text)) {
		eff = m.key(k)
	}
	return eff
}

func TestConsoleHistoryAndFocus(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	keyboard(m, "nmy private words")
	draft := m.active
	m.say("p/a: failure\x1b[2J " + strings.Repeat("long reason ", 30))
	if m.active != draft || m.mode != modeEdit {
		t.Fatal("message stole the editor")
	}
	if strings.Contains(strings.Join(m.consoleRows(), ""), "my private words") {
		t.Fatal("draft body logged")
	}
	keyboard(m, "\x1b\x1b") // leave editor, then normal Escape
	keyboard(m, "\t\t")
	if m.focus != paneConsole {
		t.Fatal("Tab did not reach console")
	}
	keyboard(m, "\x1b[H")
	top := m.console.top
	old := strings.Join(m.consoleLines(), "\n")
	m.say("p/a: a later outcome")
	if m.console.top != top || m.console.unread != 1 || strings.Join(m.consoleLines(), "\n") != old {
		t.Fatal("arrival moved a scrolled view")
	}
	if got := strings.Join(m.consoleRows(), ""); strings.ContainsRune(got, '\x1b') || !strings.Contains(got, "^[[2J") {
		t.Fatal("unsafe or missing message")
	}
	keyboard(m, "\x1b[F")
	if !m.console.follow || m.console.unread != 0 {
		t.Fatal("End did not follow")
	}
	keyboard(m, "\x1b[Z")
	if m.focus != paneDetail {
		t.Fatal("Shift-Tab did not reverse focus")
	}
	for i := 0; i < consoleLimit+10; i++ {
		m.say(fmt.Sprintf("event %d", i))
	}
	if len(m.console.events) != consoleLimit || m.console.events[0].text != "event 10" {
		t.Fatal("history did not evict oldest")
	}
	m.say(strings.Repeat("é", messageLimit+1))
	if !strings.HasSuffix(m.console.events[consoleLimit-1].text, "[message truncated]") {
		t.Fatal("truncation not labeled")
	}
}

func TestConsoleGeometryAndObservationDedup(t *testing.T) {
	for _, size := range [][2]int{{12, 40}, {24, 80}, {40, 120}} {
		m, _ := testModel(t)
		m.resize(size[0], size[1])
		b := board(1, run("p", "a"))
		b.Issues = []string{"p/a: owner unreachable"}
		m.setBoard(b)
		m.setBoard(b)
		if len(m.console.events) != 1 {
			t.Fatal("unchanged observation repeated")
		}
		keyboard(m, "ntext")
		before := m.boardHeight()
		m.say("a message")
		f := m.view()
		if len(f.rows) != size[0] || m.boardHeight() != before || m.boardHeight() < 3 || m.detailHeight() < 2 {
			t.Fatalf("bad geometry %v", size)
		}
		for _, r := range f.rows {
			if textWidth(r) > size[1]-1 {
				t.Fatalf("too wide: %q", r)
			}
		}
		if strings.Contains(f.rows[0], "message") {
			t.Fatal("outcome in title")
		}
		m.resize(10, 30)
		m.view()
		m.resize(size[0], size[1])
		if m.active.ed.String() != "text" || m.mode != modeEdit {
			t.Fatal("resize lost editor")
		}
	}
}

func TestCreationKeyboardAndSavePrompt(t *testing.T) {
	for _, alias := range []string{"new idea", "new-idea", "new intent", "new-intent"} {
		for _, existing := range []bool{false, true} {
			m, q := testModel(t)
			m.resize(12, 40)
			if existing {
				m.setBoard(board(1, run("p", "old")))
			}
			keyboard(m, ":"+alias+" p/new-one\r")
			if len(q) != 0 || m.mode != modeEdit || m.active.key.run != "new-one" {
				t.Fatalf("%s did not open draft", alias)
			}
			if f := strings.Join(m.view().rows, "\n"); !strings.Contains(f, "p/new-one") {
				t.Fatal("target not visible")
			}
			keyboard(m, "body\x13")
			if strings.Contains(alias, "idea") {
				f := strings.Join(m.view().rows, "\n")
				for _, text := range []string{"Enter/s  save only", "g  save + tag sdlc", "d  save + tag design only"} {
					if !strings.Contains(f, text) {
						t.Fatalf("missing %q: %s", text, f)
					}
				}
				keyboard(m, "s")
			}
			j := <-q
			if len(j.steps) != 1 || j.steps[0].Kind != ActCreate || j.steps[0].Text != "# new-one\nbody" {
				t.Fatalf("wrong create: %+v", j)
			}
			m.finished(jobResult{job: j, results: []Result{{Message: "project not found; draft retained"}}})
			if len(m.drafts) != 1 || !strings.Contains(strings.Join(m.consoleRows(), ""), "project not found") {
				t.Fatal("failed create lost draft or reason")
			}
		}
	}
	m, q := testModel(t)
	m.setBoard(Board{ProjectFilter: "other"})
	keyboard(m, ":new-intent p/hidden\rbody\x13")
	m.finished(jobResult{job: <-q, results: []Result{{OK: true, Message: "created p/hidden"}}})
	if m.pendingSel != nil || !strings.Contains(strings.Join(m.consoleRows(), ""), "hidden by current filters") {
		t.Fatal("hidden create not explained")
	}
}

func TestCommandKeyboardValidation(t *testing.T) {
	for _, line := range []string{"pause extra", "handoff extra", "park extra", "finish extra", "resume extra", "edit extra", "close extra", "tag sdlc extra", "untag extra", "terminal extra", "help extra", "quit extra", "q extra", "new-idea", "new intent", "new idea p/Bad", "new-intent p/new extra", "bogus"} {
		m, q := testModel(t)
		m.setBoard(board(1, idea("p", "i", 1)))
		if keyboard(m, ":"+line+"\r") != effNone || len(q) != 0 || m.mode != modeNormal || len(m.console.events) == 0 {
			t.Fatalf("%q silently accepted", line)
		}
	}
	for _, line := range []string{"pause", "handoff", "park", "finish", "resume", "edit", "close", "tag sdlc", "tag design", "untag", "terminal"} {
		m, q := testModel(t)
		keyboard(m, ":"+line+"\r")
		if len(q) != 0 || len(m.console.events) == 0 {
			t.Fatalf("%q without selection silently accepted", line)
		}
	}
	m, _ := testModel(t)
	keyboard(m, ":"+strings.Repeat("é", 90))
	for _, code := range []KeyCode{KeyWordLeft, KeyHome, KeyEnd, KeyLeft} {
		m.key(Key{Code: code})
		f := m.view()
		if !f.cursor || f.curCol >= m.width-1 || f.curCol < 1 {
			t.Fatalf("cursor unreachable: %+v", f)
		}
	}
}
