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

func TestShiftEnterEditsDrafts(t *testing.T) {
	for _, tc := range []struct {
		purpose, open, seed string
		kind                ActionKind
	}{
		{"note", "n", "", ActNote},
		{"answer", "a", "", ActAnswer},
		{"new-idea", ":new idea p/a\r", "# a\n", ActCreate},
		{"new-intent", ":new intent p/a\r", "# a\n", ActCreate},
		{"body", "e", "# a\n", ActEdit},
	} {
		for _, seq := range []string{"\r", "\n", "\x1b[13;2u", "\x1b[27;2;13~"} {
			t.Run(fmt.Sprintf("%s/%q", tc.purpose, seq), func(t *testing.T) {
				m, q := testModel(t)
				it := run("p", "a")
				it.Question = &Question{ID: 4, Text: "which?"}
				if tc.purpose == "body" {
					it = idea("p", "a", 1)
				}
				m.setBoard(board(1, it))
				keyboard(m, tc.open)
				d := m.active
				key := d.key
				keyboard(m, "é😀z\x1b[D"+seq)
				want := tc.seed + "é😀\nz"
				if m.mode != modeEdit || d == nil || d.ed.String() != want || d.ed.cur != len([]rune(tc.seed))+3 {
					t.Fatalf("middle insertion: mode %v, draft %+v", m.mode, d)
				}
				keyboard(m, "\x1b[F"+seq)
				want += "\n"
				if m.active != d || m.mode != modeEdit || d.ed.String() != want || d.ed.cur != len([]rune(want)) || d.key != key {
					t.Fatalf("end insertion: mode %v, draft %+v", m.mode, d)
				}
				if len(q) != 0 || m.busy != nil || m.saveDraft != nil {
					t.Fatal("newline started a save")
				}
				keyboard(m, "\x13")
				if tc.purpose == "new-idea" || tc.purpose == "body" {
					if m.mode != modeSave || len(q) != 0 {
						t.Fatal("Ctrl-S did not offer the idea save choices")
					}
					keyboard(m, "\r")
				}
				if len(q) != 1 {
					t.Fatal("Ctrl-S did not queue exactly one save")
				}
				j := <-q
				if len(j.steps) != 1 {
					t.Fatalf("save steps = %+v", j.steps)
				}
				a := j.steps[0]
				if a.Kind != tc.kind || a.Text != want || a.Project != "p" || a.Run != "a" || tc.purpose == "answer" && a.QuestionID != 4 {
					t.Fatalf("save = %+v", j.steps)
				}
				// A refused save must leave this exact multiline draft reachable.
				m.finished(jobResult{job: j, results: []Result{{Message: "refused"}}})
				keyboard(m, tc.open)
				if m.mode != modeEdit || m.active != d || d.ed.String() != want {
					t.Fatal("refused save lost the multiline draft")
				}
			})
		}
	}
}

func TestShiftEnterTextLimit(t *testing.T) {
	for _, seq := range []string{"\r", "\x1b[13;2u", "\x1b[27;2;13~"} {
		m, q := testModel(t)
		m.setBoard(board(1, run("p", "a")))
		keyboard(m, "n"+strings.Repeat("é", maxText))
		keyboard(m, seq)
		if m.mode != modeEdit || len(q) != 0 || m.busy != nil || len(m.active.ed.text) != maxText || m.active.ed.cur != maxText || !strings.Contains(consoleText(m), "text limit reached (256 KiB); nothing was inserted") {
			t.Fatalf("%q at limit: mode %v, message %q", seq, m.mode, consoleText(m))
		}
		keyboard(m, "\x7f"+seq)
		if m.active.ed.cur != maxText || m.active.ed.text[maxText-1] != '\n' {
			t.Fatalf("%q did not insert after making room", seq)
		}
	}
}

func TestShiftEnterOutsideEditor(t *testing.T) {
	for _, seq := range []string{"\x1b[13;2u", "\x1b[27;2;13~"} {
		for _, tc := range []struct {
			name, open string
			mode       mode
		}{
			{"command", ":help", modeCommand},
			{"search", "/p/a", modeSearch},
			{"save choice", ":new idea p/a\r\x13", modeSave},
		} {
			t.Run(fmt.Sprintf("%s/%q", tc.name, seq), func(t *testing.T) {
				m, q := testModel(t)
				m.setBoard(board(1, run("p", "a")))
				keyboard(m, tc.open+seq)
				if m.mode != tc.mode || len(q) != 0 || m.busy != nil {
					t.Fatal("Shift+Enter accepted outside the editor")
				}
				if tc.mode == modeCommand && m.cmd.String() != "help" || tc.mode == modeSearch && m.search.ed.String() != "p/a" {
					t.Fatal("Shift+Enter changed single-line text")
				}
				keyboard(m, "\r")
				wantMode := modeNormal
				if tc.mode == modeCommand {
					wantMode = modeHelp
				}
				if m.mode != wantMode || (len(q) == 1) != (tc.mode == modeSave) {
					t.Fatalf("ordinary Enter: mode %v, jobs %d", m.mode, len(q))
				}
			})
		}
	}
}

func TestConsoleHistoryAndFocus(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	keyboard(m, "nmy private words")
	draft := m.active
	m.say("p/a: failure\x1b[2J " + strings.Repeat("long reason ", 60))
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
	for _, size := range [][2]int{{20, 40}, {24, 80}, {40, 120}} {
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
		if len(f.rows) != size[0] || m.boardHeight() != before || m.boardHeight() < 1 || m.detailHeight() < 2 {
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
	for _, alias := range []string{"new idea", "new intent"} {
		for _, existing := range []bool{false, true} {
			m, q := testModel(t)
			m.resize(26, 40)
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
			if len(j.steps) != 1 || j.steps[0].Kind != ActCreate || j.steps[0].Workflow != strings.TrimPrefix(alias, "new ") || j.steps[0].Text != "# new-one\nbody" {
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
	keyboard(m, ":new intent p/hidden\rbody\x13")
	m.finished(jobResult{job: <-q, results: []Result{{OK: true, Message: "created p/hidden"}}})
	if m.pendingSel != nil || !strings.Contains(strings.Join(m.consoleRows(), ""), "hidden by current filters") {
		t.Fatal("hidden create not explained")
	}
}

func TestCommandKeyboardValidation(t *testing.T) {
	for _, line := range []string{"pause extra", "handoff extra", "park extra", "finish extra", "resume extra", "edit extra", "close extra", "tag sdlc extra", "untag extra", "terminal extra", "help extra", "quit extra", "q extra", "new idea", "new intent", "new idea p/Bad", "new intent p/new extra", "bogus"} {
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

func TestSaveOutcomeArrivesBeforeTag(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1)))
	keyboard(m, "eextra\x13g")
	j := <-q
	jobs := make(chan job, 1)
	results := make(chan jobResult, 1)
	release := make(chan struct{})
	workerDone := make(chan struct{})
	go func() {
		worker(func(a Action) Result {
			if a.Kind == ActTag {
				<-release
				return Result{Message: "tag failed"}
			}
			return Result{OK: true, Message: "saved p/i"}
		}, jobs, results)
		close(workerDone)
	}()
	jobs <- j
	progress := <-results
	m.finished(progress)
	if !progress.progress || m.busy == nil || !strings.Contains(strings.Join(m.consoleRows(), ""), "saved p/i") {
		t.Fatal("save outcome held behind tag")
	}
	close(release)
	m.finished(<-results)
	close(jobs)
	<-workerDone
	text := strings.Join(m.consoleRows(), "")
	if strings.Count(text, "saved p/i") != 2 || !strings.Contains(text, "tag failed") || m.busy != nil {
		t.Fatalf("partial result: %s", text)
	}
}

func TestHelpCtrlCProtectsDraft(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	keyboard(m, "nkeep me\x1b\x1b")
	keyboard(m, "?")
	if m.mode != modeHelp {
		t.Fatal("help not opened")
	}
	if keyboard(m, "\x03") != effNone || m.mode != modeConfirm {
		t.Fatal("help Ctrl-C lost draft protection")
	}
}

func TestRevealResultDoesNotReplaceEditor(t *testing.T) {
	m, q := testModel(t)
	it := run("p", "a")
	it.Terminal = true
	m.setBoard(board(1, it))
	keyboard(m, "tnmy draft")
	j := <-q
	active := m.active
	m.finished(jobResult{job: j, results: []Result{{Choices: []Choice{{ID: "app", Label: "Rex"}}, Message: "choose a Rex app"}}})
	if m.active != active || m.mode != modeEdit {
		t.Fatal("reveal outcome replaced the editor")
	}
	keyboard(m, "\x1b\x1b")
	keyboard(m, "t")
	if m.mode != modeChoice || len(q) != 0 {
		t.Fatal("deferred choice not reachable locally")
	}
	keyboard(m, "\x1b\x1b")
	if len(m.choices) != 0 {
		t.Fatal("cancel did not clear choices")
	}
}
