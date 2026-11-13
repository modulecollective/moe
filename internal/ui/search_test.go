package ui

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSearchMatchingAndAcceptance(t *testing.T) {
	for _, query := range []string{" MATCH ", "p/match", "[x].*", "世界", "/"} {
		t.Run(query, func(t *testing.T) {
			m, q := testModel(t)
			want := run("p", "match-[x].*-世界")
			want.Detail = []string{"chosen details"}
			items := []Item{run("other", "unrelated")}
			for i := range 30 {
				items = append(items, run("other", fmt.Sprintf("filler-%d", i)))
			}
			items = append(items, want)
			m.setBoard(board(1, items...))
			m.focus = paneConsole
			typed(m, "/"+query)
			if query == "/" { // every project/run label contains a slash
				want = items[0]
			}
			if m.mode != modeSearch || m.sel != keyOf(want) || !m.has {
				t.Fatalf("selection = %+v, mode %d", m.sel, m.mode)
			}
			if query != "/" {
				it, _ := m.selected()
				if it.Line < m.boardTop || it.Line >= m.boardTop+m.boardHeight() || !strings.Contains(strings.Join(m.detailLinesForTest(), ""), "chosen details") {
					t.Fatal("off-screen match or its details not reachable")
				}
			}
			press(m, Key{Code: KeyEnter})
			if m.mode != modeNormal || m.search != nil || m.focus != paneBoard || len(q) != 0 {
				t.Fatal("acceptance did not return to board navigation without action")
			}
			typed(m, "nafter search")
			press(m, Key{Code: KeyCtrlS})
			if a := (<-q).steps[0]; a.Project != want.Project || a.Run != want.Run || a.Text != "after search" {
				t.Fatalf("action target = %+v", a)
			}
		})
	}
}

func TestSearchOccurrencesAndEndpoints(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a"), idea("p", "match", 1), run("p", "z"), idea("p", "match", 3)))
	before := m.board
	typed(m, "/match")
	press(m, Key{Code: KeyUp})
	if m.sel != (itemKey{"p", "match", 1}) {
		t.Fatal("wrapped before first candidate")
	}
	press(m, Key{Code: KeyDown}, Key{Code: KeyDown})
	if m.sel != (itemKey{"p", "match", 3}) || len(m.search.candidates) != 2 {
		t.Fatal("occurrences deduplicated or navigation wrapped")
	}
	m.detailTop = 1
	press(m, Key{Code: KeyLeft})
	if m.detailTop != 1 {
		t.Fatal("query cursor reset detail scroll")
	}
	press(m, Key{Code: KeyUp})
	if m.detailTop != 0 || m.sel.bucket != 1 || !reflect.DeepEqual(m.board, before) || len(q) != 0 {
		t.Fatal("search altered board or failed to reset changed selection")
	}
}

func TestSearchEmptyNoMatchesAndRecovery(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, run("p", "a"), run("p", "b")))
	press(m, Key{Code: KeyDown})
	m.detailTop = 3
	typed(m, "/   ")
	if m.sel.run != "b" || len(m.search.candidates) != 2 || m.detailTop != 3 {
		t.Fatal("empty query moved selection or detail scroll")
	}
	typed(m, "absent")
	press(m, Key{Code: KeyEnter})
	if m.has || m.mode != modeSearch || !strings.Contains(strings.Join(m.detailLinesForTest(), ""), "No matches") {
		t.Fatal("zero match state accepted or retained preview")
	}
	m.setBoard(board(2, run("p", "new-absent"), run("p", "a")))
	if !m.has || m.sel.run != "new-absent" {
		t.Fatal("refresh failed to recover a match")
	}
	press(m, Key{Code: KeyWordBackspace})
	if len(m.search.candidates) != 2 || m.sel.run != "new-absent" {
		t.Fatal("deleting query failed to recover all candidates")
	}
	m.setBoard(board(3))
	press(m, Key{Code: KeyEnter})
	if m.has || m.mode != modeSearch {
		t.Fatal("empty board accepted")
	}
	press(m, Key{Code: KeyEsc})
	if m.has || m.mode != modeNormal {
		t.Fatal("empty board cancellation selected a stale item")
	}
	// A search opened before the first board arrives also recovers normally.
	m, _ = testModel(t)
	typed(m, "/")
	if m.has || len(m.search.candidates) != 0 {
		t.Fatal("empty board has candidate")
	}
	m.setBoard(board(1, run("p", "a")))
	if !m.has || m.sel.run != "a" {
		t.Fatal("empty query did not recover on arrival")
	}
}

func TestSearchRefreshRetainsMatchingIdentity(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, idea("p", "match", 1), idea("p", "match", 3), run("p", "match-other")))
	typed(m, "/match")
	press(m, Key{Code: KeyDown})
	m.detailTop = 2
	m.pendingSel = &itemKey{"p", "unrelated", 0}
	m.setBoard(board(2, run("p", "unrelated"), idea("p", "match", 3), idea("p", "match", 1)))
	if m.sel != (itemKey{"p", "match", 3}) || m.detailTop != 2 || m.pendingSel != nil {
		t.Fatal("pending save or reorder displaced matching occurrence")
	}
	m.setBoard(board(3, run("p", "unrelated"), idea("p", "match", 4)))
	if m.sel != (itemKey{"p", "match", 4}) || m.detailTop != 0 {
		t.Fatal("bucket change lost same run or retained stale detail scroll")
	}
	m.setBoard(board(4, run("p", "match-next"), run("p", "unrelated")))
	if m.sel.run != "match-next" {
		t.Fatal("removed match did not fall back to first match")
	}
	if strings.Contains(consoleText(m), "selected p/unrelated") {
		t.Fatal("ordinary reconciliation announced a selection search overruled")
	}
	m.setBoard(board(3, run("p", "match-old")))
	if m.sel.run != "match-next" || m.search.ed.String() != "match" {
		t.Fatal("stale board changed search")
	}
	m.setBoard(board(5, run("p", "unrelated")))
	if m.has {
		t.Fatal("refresh left a nonmatching selection")
	}
}

func TestSearchCancellationUsesLatestBoard(t *testing.T) {
	for _, focus := range []pane{paneBoard, paneDetail, paneConsole} {
		for _, cancel := range []KeyCode{KeyEsc, KeyCtrlC} {
			for _, change := range []string{"same", "reorder", "bucket", "gone", "empty"} {
				t.Run(fmt.Sprintf("%d/%d/%s", focus, cancel, change), func(t *testing.T) {
					m, _ := testModel(t)
					items := []Item{run("p", "a"), idea("p", "anchor", 3), run("p", "match")}
					m.setBoard(board(1, items...))
					press(m, Key{Code: KeyDown})
					m.focus, m.detailTop, m.boardTop = focus, 2, 0
					typed(m, "/match")
					want := itemKey{"p", "anchor", 3}
					switch change {
					case "reorder":
						items = []Item{items[2], items[0], items[1]}
					case "bucket":
						items[1].Bucket, want.bucket = 4, 4
					case "gone":
						items, want = []Item{items[0], items[2]}, keyOf(items[2])
					case "empty":
						items = nil
					}
					m.setBoard(board(2, items...))
					if got := m.key(Key{Code: cancel}); got != effNone {
						t.Fatalf("cancel effect = %v", got)
					}
					if m.mode != modeNormal || m.search != nil || m.focus != focus || m.board.Seq != 2 || m.detailTop != 2 {
						t.Fatal("cancellation did not restore focus/scroll against latest board")
					}
					if change == "empty" {
						if m.has {
							t.Fatal("empty board has selection")
						}
					} else if !m.has || m.sel != want {
						t.Fatalf("selection = %+v, want %+v", m.sel, want)
					}
					if change == "gone" && !strings.Contains(consoleText(m), "p/anchor left the board; selected p/match") {
						t.Fatal("missing fallback notice")
					}
				})
			}
		}
	}
}

func TestSearchCancellationRestoresScrollAndReachesMovedAnchor(t *testing.T) {
	m, _ := testModel(t)
	var items []Item
	for i := range 30 {
		items = append(items, run("p", fmt.Sprintf("item-%02d", i)))
	}
	m.setBoard(board(1, items...))
	m.move(15)
	m.boardTop, m.detailTop = 10, 4
	typed(m, "/item-29")
	press(m, Key{Code: KeyEsc})
	if m.boardTop != 10 || m.detailTop != 4 || m.sel.run != "item-15" {
		t.Fatal("scroll anchor lost")
	}
	typed(m, "/item-29")
	items[15], items[29] = items[29], items[15]
	m.setBoard(board(2, items...))
	press(m, Key{Code: KeyEsc})
	it, _ := m.selected()
	if it.Run != "item-15" || it.Line < m.boardTop || it.Line >= m.boardTop+m.boardHeight() {
		t.Fatal("moved anchor unreachable")
	}
}

func TestSearchKeepsDraftsCommandsAndPendingSave(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a"), run("p", "match")))
	typed(m, "nprecious /")
	press(m, Key{Code: KeyEsc})
	typed(m, ":pause/")
	target, command := m.cmdTarget, m.cmd.String()
	press(m, Key{Code: KeyEsc})
	typed(m, "/match")
	m.setBoard(board(2, run("p", "match")))
	if m.cmdTarget != target || m.cmdTarget.Run != "a" || command != "pause/" || m.cmd != nil {
		t.Fatal("captured command target changed")
	}
	press(m, Key{Code: KeyHome})
	for range len("match") {
		press(m, Key{Code: KeyDelete})
	}
	typed(m, "p/a")
	it, ok := m.selected()
	if !ok || it.Bucket != keptBucket {
		t.Fatal("kept draft not searchable")
	}
	press(m, Key{Code: KeyEnter})
	typed(m, "n")
	if m.active.ed.String() != "precious /" || m.active.key.run != "a" {
		t.Fatal("draft changed target or contents")
	}
	press(m, Key{Code: KeyCtrlS})
	j := <-q
	typed(m, "/match")
	m.finished(jobResult{job: j, results: []Result{{Message: "refused"}}})
	if m.mode != modeSearch || m.sel.run != "match" || m.drafts[*j.draft].ed.String() != "precious /" {
		t.Fatal("pending refusal disrupted search or draft")
	}
	press(m, Key{Code: KeyCtrlC})
	typed(m, "n")
	press(m, Key{Code: KeyCtrlS})
	j = <-q
	typed(m, "/match")
	m.finished(jobResult{job: j, results: []Result{{OK: true}}})
	if m.mode != modeSearch || m.sel.run != "match" || m.drafts[*j.draft] != nil {
		t.Fatal("pending success disrupted search")
	}
}

func TestSearchPendingCreation(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "match")))
	typed(m, ":new idea p/created")
	press(m, Key{Code: KeyEnter}, Key{Code: KeyCtrlS}, Key{Code: KeyEnter})
	j := <-q
	typed(m, "/match")
	m.finished(jobResult{job: j, results: []Result{{OK: true}}})
	if m.mode != modeSearch || m.sel.run != "match" || m.pendingSel == nil {
		t.Fatal("creation result disrupted search or lost pending selection")
	}
	m.setBoard(board(2, idea("p", "created", 3), run("p", "match")))
	if m.sel.run != "match" || m.pendingSel != nil || m.search.ed.String() != "match" {
		t.Fatal("newly saved capture displaced matching selection")
	}
}

func TestSearchAllKindsAndLabelsOnly(t *testing.T) {
	m, _ := testModel(t)
	items := []Item{run("p", "match-run"), idea("p", "match-idea", 1),
		{Project: "p", Run: "match-intent", Kind: ItemIntent},
		{Project: "p", Run: "match-chore", Kind: ItemChore},
		{Project: "p", Run: "match-completed", Bucket: 4},
		{Project: "p", Run: "unrelated", Body: "secret-prose", Detail: []string{"secret-prose"}},
	}
	b := board(1, items...)
	b.Lines = append(b.Lines, "secret-prose heading")
	b.Header = "secret-prose status"
	b.ProjectFilter, b.WorkflowFilter = "p", "sdlc"
	m.setBoard(b)
	typed(m, "/match")
	if !slices.Equal(m.search.candidates, []int{0, 1, 2, 3, 4}) || !reflect.DeepEqual(m.board, b) {
		t.Fatal("search omitted an item kind or changed filtered board")
	}
	press(m, Key{Code: KeyWordBackspace})
	typed(m, "secret-prose")
	if m.has {
		t.Fatal("search included prose, headings or status")
	}
	for range len("secret-prose") {
		press(m, Key{Code: KeyBackspace})
	}
	if !m.has || m.sel != keyOf(items[0]) || len(m.search.candidates) != len(items) {
		t.Fatal("deleting a no-match query did not restore first candidate")
	}
}

func TestSearchEditingAndModeBoundaries(t *testing.T) {
	m, q := testModel(t)
	m.setBoard(board(1, run("p", "a")))
	press(m, Key{Code: KeyPaste, Text: "/qn:jk"})
	if m.mode != modeNormal {
		t.Fatal("normal paste opened search")
	}
	typed(m, "/jkqn:/")
	for _, code := range []KeyCode{KeyTab, KeyBackTab, KeyPgUp, KeyPgDn, KeyCtrlS, KeyCtrlX} {
		press(m, Key{Code: code})
	}
	if m.search.ed.String() != "jkqn:/" || len(q) != 0 || m.mode != modeSearch {
		t.Fatal("shortcut escaped query")
	}
	press(m, Key{Code: KeyHome}, Key{Code: KeyDelete}, Key{Code: KeyRight}, Key{Code: KeyBackspace}, Key{Code: KeyEnd}, Key{Code: KeyWordBackspace})
	if m.search.ed.String() != "" {
		t.Fatal("single-line editing failed")
	}
	press(m, Key{Code: KeyPaste, Text: "hello\n世界/p-x", Truncated: true}, Key{Code: KeyWordLeft}, Key{Code: KeyWordRight})
	if m.search.ed.String() != "hello 世界/p-x" || m.search.ed.cur != len([]rune("hello 世界/p-x")) || !strings.Contains(consoleText(m), "search paste truncated") {
		t.Fatal("paste/word editing failed")
	}
	press(m, Key{Code: KeyPaste, Text: strings.Repeat("x", maxText)})
	if m.search.ed.String() != "hello 世界/p-x" || !strings.Contains(consoleText(m), "search text limit reached") {
		t.Fatal("text limit not visibly refused")
	}
	if m.key(Key{Code: KeyCtrlZ}) != effSuspend || m.key(Key{Code: KeyCtrlL}) != effNone || m.mode != modeSearch {
		t.Fatal("global keys lost")
	}
	press(m, Key{Code: KeyCtrlC})
	typed(m, "?")
	typed(m, "/")
	if m.mode != modeHelp {
		t.Fatal("slash escaped help")
	}
	press(m, Key{Code: KeyEsc})
	m.ask("sure?", func(*model) {})
	typed(m, "/")
	if m.mode == modeSearch || len(q) != 0 {
		t.Fatal("slash opened search in confirmation")
	}
}

func TestSearchRenderingAndAnimation(t *testing.T) {
	for _, width := range []int{40, 100} {
		for _, color := range []bool{false, true} {
			m, _ := testModel(t)
			m.resize(20, width)
			m.color = color
			a := run("p", "match")
			b := board(1, a, idea("p", "match", 3), run("p", "other"))
			b.Lines[1] = "  \x1b[1mp/match\x1b[0m \x1b[2J\u0085"
			m.setBoard(b)
			geometry := m.layout()
			if row, _ := m.footerRow(width - 1); !strings.Contains(row, "/ search") {
				t.Fatal("normal search hint clipped")
			}
			typed(m, "/match")
			f := m.view()
			l := m.layout()
			if geometry != l || !strings.HasPrefix(f.rows[l.boardRow], "> p/match") {
				t.Fatalf("markers/geometry: %q", f.rows)
			}
			// The minimum layout shows one board row. Scroll it to prove
			// an unselected matching occurrence keeps the safe row renderer.
			m.boardTop = 1
			if row := m.view().rows[l.boardRow]; !strings.HasPrefix(row, "* \x1b[1mp/match\x1b[0m ^[[2J�") {
				t.Fatalf("match marker: %q", row)
			}
			if m.paneFocused(paneBoard) || m.paneFocused(paneDetail) || m.paneFocused(paneConsole) {
				t.Fatal("pane claims search focus")
			}
			if !f.cursor || f.curRow != m.high-1 || !strings.Contains(f.rows[f.curRow], "1/2 Enter/Esc") {
				t.Fatal("search footer lost cursor/count/hints")
			}
			press(m, Key{Code: KeyPaste, Text: strings.Repeat("世界", 100) + "\x1b[2J"})
			f = m.view()
			if !strings.Contains(f.rows[f.curRow], "No matches") || f.curCol >= width-1 || f.curCol < 2 || strings.Contains(f.rows[f.curRow], "\x1b[2J") {
				t.Fatal("long query cursor or no-match clipping unsafe")
			}
			for _, row := range f.rows {
				if textWidth(row) > width-1 {
					t.Fatalf("row too wide: %q", row)
				}
			}
			m.resize(10, 30)
			if !strings.Contains(m.view().rows[0], "too small") {
				t.Fatal("small terminal notice missing")
			}
			m.resize(20, width)
			if !m.view().cursor || m.search.ed.String() == "" {
				t.Fatal("resize lost search")
			}
			press(m, Key{Code: KeyCtrlC})
			if strings.Contains(strings.Join(m.view().rows, ""), "* ") {
				t.Fatal("cancel retained marks")
			}
		}
	}
	m, _ := testModel(t)
	m.setBoard(animatedBoard())
	typed(m, "/a")
	f := m.view()
	before, candidates, sel, query := slices.Clone(f.rows), slices.Clone(m.search.candidates), m.sel, m.search.ed.String()
	// Changing a label without a board event distinguishes animation from rematching.
	m.board.Items[0].Run = "changed"
	patch := string(m.animate(&f))
	if !strings.Contains(patch, fmt.Sprintf("\x1b[%d;%dH\x1b[?25h", f.curRow+1, f.curCol+1)) || !slices.Equal(candidates, m.search.candidates) || m.sel != sel || m.search.ed.String() != query {
		t.Fatal("animation changed search or failed to restore cursor")
	}
	for i := range before {
		if i != 2 && before[i] != f.rows[i] {
			t.Fatalf("animation repainted search row %d", i)
		}
	}
}
