package ui

import (
	"slices"
	"strings"
	"testing"
)

func TestCommandCompletionVocabularyAndCycles(t *testing.T) {
	for _, tc := range []struct {
		line  string
		words []string
	}{
		{"", []string{"close", "edit", "finish", "handoff", "help", "new-idea", "new-intent", "park", "pause", "quit", "resume", "tag-design", "tag-sdlc", "terminal", "untag"}},
		{"p", []string{"park", "pause"}},
		{"new-i", []string{"new-idea", "new-intent"}},
		{"tag-", []string{"tag-design", "tag-sdlc"}},
		{"tag-s", []string{"tag-sdlc"}},
		{"ne", []string{"new-idea", "new-intent"}},
	} {
		for _, existing := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				m, q := testModel(t)
				if existing {
					m.setBoard(board(1, run("p", "old")))
				}
				keyboard(m, ":"+tc.line)
				words := slices.Clone(tc.words)
				key := "\t"
				if reverse {
					slices.Reverse(words)
					key = "\x1b[Z"
				}
				prefix := tc.line[:strings.LastIndex(tc.line, " ")+1]
				for i := 0; i < 2*len(words); i++ {
					keyboard(m, key)
					want := prefix + words[i%len(words)]
					if m.cmd.String() != want || m.cmd.cur != len([]rune(want)) {
						t.Fatalf("%q reverse=%v step %d: %q at %d, want %q", tc.line, reverse, i, m.cmd.String(), m.cmd.cur, want)
					}
				}
				if len(q) != 0 || len(m.drafts) != 0 || len(m.console.events) != 0 || m.mode != modeCommand {
					t.Fatal("completion caused side effects")
				}
			}
		}
	}
}

func TestCommandCompletionAtCursor(t *testing.T) {
	for _, tc := range []struct {
		line   string
		cur    int
		want   string
		cursor int
	}{
		{"new-ix  项目/é", 5, "new-idea  项目/é", 8},
		{"\u2003new-ix  p/é", 6, "\u2003new-idea  p/é", 9},
		{"  new-ixxx  p/é", 7, "  new-idea  p/é", 10},
		{"paZZ p/世界", 2, "park p/世界", 4},
		{"new-intent p/é", 3, "new-idea p/é", 8},
		{"new-  p/é", 4, "new-idea  p/é", 8},
		{"tag-x p/é", 4, "tag-design p/é", 10},
		{"New-i", 5, "New-i", 5},
		{"bogus", 5, "bogus", 5},
		{"new-idea p/name", 15, "new-idea p/name", 15},
		{"pause ", 6, "pause ", 6},
		{"tag-sdlc ", 9, "tag-sdlc ", 9},
		{"new-idea ", 9, "new-idea ", 9},
		{"new i", 5, "new i", 5},
		{"tag d", 5, "tag d", 5},
		{"tag sdlc ", 9, "tag sdlc ", 9},
		{"new idea ", 9, "new idea ", 9},
	} {
		m, _ := testModel(t)
		m.openCommand()
		m.cmd = newEditor(tc.line, true)
		m.cmd.cur = tc.cur
		keyboard(m, "\t")
		if m.cmd.String() != tc.want || m.cmd.cur != tc.cursor {
			t.Errorf("%q at %d: %q at %d, want %q at %d", tc.line, tc.cur, m.cmd.String(), m.cmd.cur, tc.want, tc.cursor)
		}
	}
}

func TestCommandCompletionResetAndPaste(t *testing.T) {
	for _, tc := range []struct{ keys, want string }{
		{"a\t", "parka"}, // the edited prefix has no match
		{"\x7f\t", "park"},
		{"\x1b[D\t", "park"},
		{"\x1b[H\t", "close"},
		{"\x1b[F\t", "park"},
		{"\x1b[3~\t", "park"},
		{"\x1b[200~\x1b[201~\t", "park"},
	} {
		m, _ := testModel(t)
		keyboard(m, ":p\t")
		keyboard(m, tc.keys)
		want := tc.want
		if m.cmd.String() != want {
			t.Errorf("%q: %q, want %q", tc.keys, m.cmd.String(), want)
		}
	}
	m, q := testModel(t)
	keyboard(m, ":new-\x1b[200~i\t\x1b[201~")
	if m.cmd.String() != "new-i\t" || m.completion != nil || len(q) != 0 {
		t.Fatal("paste dispatched completion")
	}
	for _, cancel := range []KeyCode{KeyEsc, KeyCtrlC, KeyEnter} {
		m, _ := testModel(t)
		keyboard(m, ":p\t")
		m.key(Key{Code: cancel})
		if m.completion != nil || m.cmd != nil {
			t.Fatal("command lifecycle retained cycle")
		}
		keyboard(m, ":p\t")
		if m.cmd.String() != "park" {
			t.Fatal("fresh prompt retained cycle")
		}
	}
}

func TestCommandCompletionRefreshResizeAndTarget(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "old")))
	keyboard(m, ":p\t")
	target, cycle, focus := m.cmdTarget, m.completion, m.focus
	for _, size := range [][2]int{{20, 40}, {30, 100}} {
		m.setBoard(board(2, run("p", "replacement")))
		m.resize(size[0], size[1])
		m.animation++
		f := m.view()
		if m.cmdTarget != target || target.Run != "old" || m.completion != cycle || m.focus != focus || !f.cursor || f.curCol < 1 || f.curCol >= size[1]-1 || f.curRow < 0 || f.curRow >= size[0] {
			t.Fatalf("completion lost across refresh/resize: %+v", f)
		}
	}
	keyboard(m, "\t")
	if m.cmd.String() != "pause" || len(q) != 0 {
		t.Fatal("refresh reset cycle or dispatched work")
	}
	keyboard(m, "\x1b[Z")
	if m.cmd.String() != "park" {
		t.Fatal("reverse changed the match set")
	}
}

func TestCommandCompletionTextLimit(t *testing.T) {
	m, _ := testModel(t)
	m.openCommand()
	text := "p " + strings.Repeat("é", maxText-2)
	m.cmd = newEditor(text, true)
	m.cmd.cur = 1
	keyboard(m, "\t")
	if m.cmd.String() != text || m.cmd.cur != 1 || !strings.Contains(strings.Join(m.consoleRows(), ""), "command text limit reached") {
		t.Fatal("oversized replacement not refused intact")
	}
	m.cmd = newEditor("p "+strings.Repeat("é", maxText-5), true)
	m.cmd.cur = 1
	m.completion = nil
	keyboard(m, "\t")
	if len(m.cmd.text) != maxText || !strings.HasPrefix(m.cmd.String(), "park ") {
		t.Fatal("replacement at exact limit refused")
	}
}

func TestRemovedSpacedCommands(t *testing.T) {
	for _, command := range []string{"new idea p/new-one", "new intent p/new-one", "tag sdlc", "tag design"} {
		for _, existing := range []bool{false, true} {
			m, q := testModel(t)
			if existing {
				m.setBoard(board(1, idea("p", "old", 1)))
			}
			keyboard(m, ":"+command+"\r")
			if len(q) != 0 || len(m.drafts) != 0 || m.active != nil || m.mode != modeNormal || !strings.Contains(strings.Join(m.consoleRows(), ""), "unknown command "+quote(strings.Fields(command)[0])) {
				t.Fatalf("%s accepted or wrong error", command)
			}
			if commandHint(command, nil) == "creates on save" {
				t.Fatal("removed command has creation hint")
			}
		}
	}
}

func TestCommandCompletionKeepsArguments(t *testing.T) {
	m, q := testModel(t)
	keyboard(m, ":new-i p/é")
	m.cmd.cur = 5
	for _, tc := range []struct{ key, want string }{
		{"\t", "new-idea p/é"},
		{"\t", "new-intent p/é"},
		{"\t", "new-idea p/é"},
		{"\x1b[Z", "new-intent p/é"},
	} {
		keyboard(m, tc.key)
		if m.cmd.String() != tc.want || len(q) != 0 || len(m.drafts) != 0 {
			t.Fatalf("completion = %q, want %q without effects", m.cmd.String(), tc.want)
		}
	}
}
