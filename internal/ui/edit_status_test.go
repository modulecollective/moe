package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/cliout"
)

func TestEditingStatusInDetailsRule(t *testing.T) {
	for _, tc := range []struct {
		name, keys, description, first string
		cursorRow                      int
	}{
		{"note", "n", "note for p/target", "kept text", 0},
		{"answer", "a", "answer to question #42 on p/target", "kept text", 0},
		{"idea", "e", "edit of p/target", "kept text", 0},
		{"intent", "e", "edit of p/target", "kept text", 0},
		{"new idea", ":new-idea p/fresh\r", "new idea p/fresh", "# fresh", 1},
		{"new intent", ":new-intent p/fresh\r", "new intent p/fresh", "# fresh", 1},
	} {
		for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 20}} {
			t.Run(fmt.Sprintf("%s/%dx%d", tc.name, size[0], size[1]), func(t *testing.T) {
				m, _ := testModel(t)
				m.resize(size[1], size[0])
				it := run("p", "target")
				it.Question = &Question{ID: 42, Text: "which?"}
				if tc.name == "idea" || tc.name == "intent" {
					it = idea("p", "target", 1)
					it.Body = ""
					if tc.name == "intent" {
						it.Kind = ItemIntent
					}
				}
				m.setBoard(board(1, it, run("p", "other")))
				keyboard(m, tc.keys+"kept text")
				check := func() {
					t.Helper()
					f, l := m.view(), m.layout()
					want := "━━ Details · editing " + tc.description
					if !strings.HasPrefix(f.rows[l.detailRule], plain(want, m.width-1)) {
						t.Fatalf("editing rule = %q, want %q", f.rows[l.detailRule], want)
					}
					if f.rows[l.detailRow] != tc.first {
						t.Fatalf("first body row = %q, want %q", f.rows[l.detailRow], tc.first)
					}
					if !f.cursor || f.curRow != l.detailRow+tc.cursorRow || f.curCol != len("kept text") {
						t.Fatalf("cursor = %t %d,%d", f.cursor, f.curRow, f.curCol)
					}
					assertEditFrameBounds(t, m, f)
					m.color = true
					colored := m.view()
					m.color = false
					for i, row := range colored.rows {
						if stripSGR(row) != f.rows[i] {
							t.Fatalf("color changed row %d", i)
						}
					}
					if colored.curRow != f.curRow || colored.curCol != f.curCol || !strings.HasPrefix(colored.rows[l.detailRule], cliout.Bright+"━━") {
						t.Fatal("color changed cursor or focused heading style")
					}
					for _, row := range colored.rows[l.detailRow : l.detailRow+l.detail] {
						if strings.Contains(row, "\x1b") || strings.Contains(row, "editing ") {
							t.Fatalf("status/style leaked into draft: %q", row)
						}
					}
				}
				check()
				// A newly created draft deliberately has a different target
				// from selection; a refresh must keep that captured identity.
				m.setBoard(board(2, run("p", "other"), it))
				check()
				press(m, Key{Code: KeyEsc})
				if strings.Contains(m.view().rows[m.layout().detailRule], "editing ") {
					t.Fatal("editing label survived Esc")
				}
				keyboard(m, tc.keys)
				check()
			})
		}
	}
}

func assertEditFrameBounds(t *testing.T, m *model, f frame) {
	t.Helper()
	l := m.layout()
	if len(f.rows) != m.high || !f.cursor || f.curRow < l.detailRow || f.curRow >= l.detailRow+l.detail || f.curCol < 0 || f.curCol >= m.width-1 {
		t.Fatalf("frame/cursor outside editor: %+v", f)
	}
	for _, row := range f.rows {
		if textWidth(row) > m.width-1 {
			t.Fatalf("row exceeds terminal: %q", row)
		}
	}
}

func TestEditingStatusWarningsAndRecovery(t *testing.T) {
	for _, kind := range []string{"closed question", "closed target", "refused", "uncertain"} {
		for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 20}} {
			t.Run(fmt.Sprintf("%s/%dx%d", kind, size[0], size[1]), func(t *testing.T) {
				m, q := testModel(t)
				m.resize(size[1], size[0])
				it := run("p", "target")
				it.Question = &Question{ID: 42, Text: "which?"}
				m.setBoard(board(1, it))
				key, warning := "n", ""
				if kind == "closed question" {
					key = "a"
				}
				keyboard(m, key+"kept text")
				switch kind {
				case "closed question":
					it.Question = nil
					m.setBoard(board(2, it))
					warning = "question #42 is no longer open: saving will be refused; Ctrl-X discards this answer"
				case "closed target":
					it.Notes = false
					m.setBoard(board(2, it))
					warning = "p/target no longer takes notes: saving will be refused; Ctrl-X discards it"
				default:
					res := Result{Message: "save refused; retry or discard"}
					if kind == "uncertain" {
						res = Result{Uncertain: true, Message: "save outcome unknown", Lines: []string{"readback: not recorded"}}
					}
					warning = res.Message
					press(m, Key{Code: KeyCtrlS})
					m.finished(jobResult{job: <-q, results: []Result{res}})
					keyboard(m, key)
				}
				f, l := m.view(), m.layout()
				if !strings.HasPrefix(f.rows[l.detailRow], "! ") || !strings.Contains(strings.Join(m.detailLinesForTest(), ""), warning) {
					t.Fatalf("first warning missing: %q", f.rows[l.detailRow:l.detailRow+l.detail])
				}
				if f.curRow <= l.detailRow || f.rows[f.curRow] != "kept text" {
					t.Fatal("warning displaced or hid draft cursor")
				}
				assertEditFrameBounds(t, m, f)
				press(m, Key{Code: KeyEsc})
				if got := paintedDetail(t, m); !strings.Contains(got, "! "+warning) {
					t.Fatalf("Esc lost first warning: %q", got)
				}
				if kind == "uncertain" && !strings.Contains(paintedDetail(t, m), "readback: not recorded") {
					t.Fatal("Esc lost uncertain-save evidence")
				}
				keyboard(m, key)
				press(m, Key{Code: KeyCtrlX})
				keyboard(m, "y")
				if m.dirtyDrafts() != 0 {
					t.Fatal("discard did not recover from warning")
				}
			})
		}
	}
}

func TestEditingStatusWrapResizeAndSanitize(t *testing.T) {
	m, _ := testModel(t)
	it := run("p世界\x1b[2J", strings.Repeat("長target", 30))
	m.setBoard(board(1, it))
	keyboard(m, "n")
	keyboard(m, strings.Repeat(strings.Repeat("世界 text ", 20)+"\r", 30)+"cursor text")
	for _, size := range [][2]int{{100, 30}, {80, 24}, {40, 20}, {100, 30}} {
		m.resize(size[1], size[0])
		f, l := m.view(), m.layout()
		assertEditFrameBounds(t, m, f)
		if !strings.HasPrefix(f.rows[l.detailRule], "━━ Details · editing note for p世界^[") || strings.Contains(f.rows[l.detailRule], "\x1b") {
			t.Fatalf("unsafe or displaced long label: %q", f.rows[l.detailRule])
		}
		if f.rows[f.curRow] != "cursor text" || f.curCol != len("cursor text") || f.curRow != l.detailRow+l.detail-1 {
			t.Fatal("resize/scroll lost cursor text")
		}
	}
}

func TestEditingStatusSaveChoiceAndHelp(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1)))
	keyboard(m, "emore\x13")
	if m.mode != modeSave || !strings.HasPrefix(m.view().rows[m.layout().detailRule], "━━ Details · edit of p/i ") || m.view().cursor {
		t.Fatal("save choice claims text is editable")
	}
	press(m, Key{Code: KeyEsc})
	if !strings.HasPrefix(m.view().rows[m.layout().detailRule], "━━ Details · editing edit of p/i ") || !m.view().cursor {
		t.Fatal("return from save choice lost editing status")
	}
	press(m, Key{Code: KeyEsc})
	keyboard(m, "?")
	if !strings.HasPrefix(m.view().rows[m.layout().detailRule], "━━ Details · Help ") {
		t.Fatal("help label changed")
	}
}
