package ui

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/cliout"
	"github.com/modulecollective/moe/internal/dash"
)

// Use the real chart, factory and indexed tables: synthetic art would miss
// both the width budget and offsets contributed by headings and blanks.
func dashboardBoard() Board {
	now := time.Date(2026, 11, 12, 12, 30, 0, 0, time.Local)
	rows := []dash.Row{{Project: "p", Run: "live", Bucket: dash.BucketActiveRuns, Stage: "code", RunningDoc: "code", When: now}}
	for i := range 2400 {
		rows = append(rows, dash.Row{Project: "p", Run: fmt.Sprintf("done-%04d", i), Bucket: dash.BucketCompletedRuns, When: now})
	}
	counts := make([]int, dash.HistDays)
	for i := range counts {
		counts[i] = i + 1
	}
	frames := dash.StyledFactoryFrames(&bytes.Buffer{}, dash.FactoryStateFromRows(rows), 8, rand.New(rand.NewSource(42)))
	var out bytes.Buffer
	start := dash.RenderOverview(&out, dash.BuildActivityHistogram(counts), frames[0])
	b := Board{Seq: 1, Overview: strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"), Art: Art{Start: start, Height: len(frames[0]), Frames: frames}, Header: "all projects · gathered 12:30:00 · 1 live"}
	out.Reset()
	banner.Dash(&out, now, "serve armed")
	b.Banner = strings.TrimSuffix(out.String(), "\n")
	out.Reset()
	fmt.Fprintln(&out, "  service detail")
	lines := dash.RenderIndexedRows(&out, now, rows, 1, 1, true)
	b.Lines = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	for i, r := range rows {
		it := run(r.Project, r.Run)
		it.Line = lines[i] + 1
		b.Items = append(b.Items, it)
	}
	return b
}

func assertPinned(t *testing.T, m *model) {
	t.Helper()
	f := m.view()
	if f.rows[0] != boardLine(m.board.Banner, false, m.width-1) {
		t.Fatal("shared banner moved")
	}
	for i, line := range m.board.Overview {
		if stripSGR(f.rows[i+1]) != stripSGR(line) {
			t.Fatalf("overview row %d moved or clipped: %q", i, f.rows[i+1])
		}
	}
	if strings.Count(strings.Join(f.rows, "\n"), "MINISTRY OF EVERYTHING") != 1 {
		t.Fatal("banner duplicated")
	}
}

func TestDashboardOverviewSurvivesBoardScroll(t *testing.T) {
	m, _ := testModel(t)
	m.resize(40, 120)
	m.setBoard(dashboardBoard())
	for range 50 {
		press(m, Key{Code: KeyDown})
	}
	if m.boardTop < 6 {
		t.Fatal("regression did not scroll past the lost header")
	}
	assertPinned(t, m)
	it, _ := m.selected()
	if it.Line < m.boardTop || it.Line >= m.boardTop+m.boardHeight() {
		t.Fatal("selection hidden")
	}
	header := m.view().rows[m.layout().boardRow-1]
	want := fmt.Sprintf("lines %d–%d of %d", m.boardTop+1, m.boardTop+m.boardHeight(), len(m.board.Lines))
	if !strings.Contains(header, want) {
		t.Fatalf("range %q absent: %q", want, header)
	}
	for _, key := range []KeyCode{KeyPgDn, KeyEnd, KeyHome} {
		press(m, Key{Code: key})
		assertPinned(t, m)
	}
	if m.boardTop != 0 {
		t.Fatal("Home did not reach scrollable start")
	}
	// An unchanged observation must preserve a deliberately paged viewport.
	press(m, Key{Code: KeyEnd})
	top := m.boardTop
	m.setBoard(m.raw)
	if m.boardTop != top {
		t.Fatal("refresh reset manual paging")
	}
	press(m, Key{Code: KeyDown})
	it, _ = m.selected()
	if it.Line < m.boardTop || it.Line >= m.boardTop+m.boardHeight() {
		t.Fatal("selection movement did not restore visibility")
	}
}

func TestDashboardCollapseAndEditorGeometry(t *testing.T) {
	b := dashboardBoard()
	wide := 0
	for _, r := range b.Overview {
		wide = max(wide, textWidth(r))
	}
	threshold := len(b.Overview) + 5 + 3 + 6
	sizes := [][2]int{{120, 90}, {100, 40}, {80, 24}, {40, 12}, {wide, 40}, {wide + 1, 40}, {120, threshold - 1}, {120, threshold}}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, _ := testModel(t)
			m.resize(size[1], size[0])
			m.setBoard(b)
			keyboard(m, "nkept text")
			l := m.layout()
			want := size[0]-1 >= wide && size[1]-5-m.consoleHeight()-len(b.Overview) >= 6
			if (l.overview > 0) != want || l.board < 4 || l.detail < 2 {
				t.Fatalf("invalid layout: %+v", l)
			}
			f := m.view()
			if len(f.rows) != size[1] || l.consoleRow+l.console+1 != size[1] {
				t.Fatal("frame height")
			}
			if !f.cursor || f.curRow < l.detailRow || f.curRow >= l.detailRow+l.detail || f.curCol >= size[0]-1 {
				t.Fatalf("editor cursor: %+v", f)
			}
			for _, row := range f.rows {
				if textWidth(row) > size[0]-1 {
					t.Fatalf("too wide: %q", row)
				}
			}
			if want {
				assertPinned(t, m)
			} else {
				screen := strings.Join(f.rows, "\n")
				if strings.Contains(screen, "last 60 days") || strings.Contains(screen, b.Overview[0]) {
					t.Fatal("partial overview in compact mode")
				}
			}
			// Renderer SGR consumes no terminal cells and must not change collapse.
			m.board.Overview = slices.Clone(b.Overview)
			for i, row := range m.board.Overview {
				m.board.Overview[i] = cliout.Mid + row + cliout.Reset
			}
			if m.layout() != l {
				t.Fatal("color changed collapse geometry")
			}
			m.mode = modeNormal
			m.focus = paneConsole
			if m.layout() != l {
				t.Fatal("input mode moved pane geometry")
			}
			m.mode = modeEdit
			m.resize(10, 30)
			if !strings.Contains(m.view().rows[0], "too small") {
				t.Fatal("resize notice")
			}
			m.resize(90, 120)
			if m.active.ed.String() != "kept text" || m.sel.run != "live" {
				t.Fatal("resize lost draft or selection")
			}
			assertPinned(t, m)
		})
	}
}

func TestPaneEmphasisTracksInputRecipient(t *testing.T) {
	m, _ := testModel(t)
	m.setBoard(board(1, idea("p", "i", 1)))
	check := func(want int) {
		t.Helper()
		f := m.view()
		focused := -1
		for i, r := range f.rows {
			if strings.HasPrefix(r, "━━") {
				if focused >= 0 {
					t.Fatal("two input recipients")
				}
				focused = i
			}
		}
		if focused != want {
			t.Fatalf("focus row %d, want %d", focused, want)
		}
		if strings.Contains(strings.Join(f.rows, "\n"), "focus:") || strings.Contains(strings.Join(f.rows, "\n"), "· focus") {
			t.Fatal("old focus text")
		}
		m.color = true
		colored := m.view()
		for i, row := range colored.rows {
			if stripSGR(row) != f.rows[i] {
				t.Fatalf("color changed geometry row %d", i)
			}
		}
		if !strings.HasPrefix(colored.rows[want], cliout.Bright) {
			t.Fatal("focused heading not bright")
		}
		m.color = false
	}
	l := m.layout()
	check(l.boardRow - 1)
	keyboard(m, "\t")
	check(l.detailRow - 1)
	keyboard(m, "\t")
	check(l.consoleRow - 1)
	keyboard(m, "\x1b[Z")
	check(l.detailRow - 1)
	keyboard(m, "?")
	check(l.detailRow - 1)
	press(m, Key{Code: KeyEsc})
	keyboard(m, "e")
	check(l.detailRow - 1)
	keyboard(m, "more\x13")
	check(l.detailRow - 1) // idea save choice
	press(m, Key{Code: KeyEsc})
	press(m, Key{Code: KeyEsc})
	keyboard(m, ":")
	check(m.high - 1)
	f := m.view()
	if !f.cursor || f.curRow != m.high-1 || f.curCol != 4 {
		t.Fatal("command cursor displaced")
	}
	keyboard(m, "close\r")
	check(1)
	if len(m.animate(&f)) != 0 {
		t.Fatal("confirmation received animation")
	}
}

func TestPinnedAnimationAgreesAfterScrollAndResize(t *testing.T) {
	m, _ := testModel(t)
	m.resize(40, 120)
	m.setBoard(dashboardBoard())
	for range 80 {
		press(m, Key{Code: KeyDown})
	}
	keyboard(m, "nprivate draft")
	old := m.view()
	before := slices.Clone(old.rows)
	p := m.animate(&old)
	if len(p) == 0 {
		t.Fatal("no smoke patch")
	}
	for i := range before {
		if i < 1+m.board.Art.Start || i >= 1+m.board.Art.Start+m.board.Art.Height {
			if old.rows[i] != before[i] {
				t.Fatalf("animation touched non-art row %d", i)
			}
		}
	}
	if !bytes.Contains(p, []byte(fmt.Sprintf("\x1b[%d;%dH\x1b[?25h", old.curRow+1, old.curCol+1))) {
		t.Fatal("cursor not restored")
	}
	if !slices.Equal(m.view().rows, old.rows) {
		t.Fatal("full paint disagrees")
	}
	m.resize(12, 40)
	old = m.view()
	if len(m.animate(&old)) > 0 {
		t.Fatal("collapsed overview patched")
	}
	m.resize(40, 120)
	old = m.view()
	m.animate(&old)
	if !slices.Equal(m.view().rows, old.rows) {
		t.Fatal("resize left stale animation coordinates")
	}
}

func TestNarrowHeadingPreservesStateAndSelection(t *testing.T) {
	m, _ := testModel(t)
	m.resize(12, 40)
	b := board(1, run("p", "r"))
	b.Header = "project " + strings.Repeat("long filter ", 10)
	m.setBoard(b)
	keyboard(m, "nunsaved")
	m.busy = &job{label: strings.Repeat("long operation ", 10)}
	f := m.view()
	heading := f.rows[m.layout().boardRow-1]
	for _, want := range []string{"Board", "unsaved draft", "working"} {
		if !strings.Contains(heading, want) {
			t.Fatalf("state %q lost: %q", want, heading)
		}
	}
	if !strings.HasPrefix(f.rows[m.layout().boardRow], ">") {
		t.Fatal("editor hid selected target marker")
	}
	if strings.Contains(heading, "gathered") || strings.Contains(heading, "lines ") {
		t.Fatal("metadata displaced state")
	}
	m.ask("Confirm an action?", func(*model) {})
	heading = m.view().rows[1]
	if !strings.Contains(heading, "unsaved draft") || !strings.Contains(heading, "working") {
		t.Fatalf("confirmation hid state: %q", heading)
	}
}
