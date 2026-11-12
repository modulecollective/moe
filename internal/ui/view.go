package ui

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/modulecollective/moe/internal/cliout"
)

const helpText = `Keys
  Up/Down, j/k   select the previous/next board item
  PgUp/PgDn      page the focused pane; Home/End jump to its ends
  Tab            switch focus between the board and the details
  n              write a note to the selected run or idea
  a              answer the selected run's open question
  e              edit the selected open idea or intent
  t              reveal the selected run's Rex terminal
  :              command line for the selected item
  ?              this help (Esc closes)
  q, Ctrl-C      quit (asks first when drafts are unsaved)

Editor
  Ctrl-S save · Esc back (the draft is kept) · Ctrl-X discard
  Enter is a newline; arrows, Home/End, Backspace/Delete, paste

Commands (:)
  pause | handoff | park | finish | resume    the selected run
  new idea <project>/<slug>                   opens a draft; creates on save
  new intent <project>/<slug>
  edit | close                                the selected idea or intent
  tag sdlc | tag design | untag               the selected idea's licence
  terminal | help | quit

Notes reach the run's next prompt; they never pause, resume or start
work. finish ships the run (!!) from its next boundary. resume releases
a hold and starts nothing. Tagging licenses later pickup; it launches
nothing.`

// frame is one painted screen: exactly m.high rows, plus where the
// cursor goes when it shows.
type frame struct {
	rows           []string
	cursor         bool
	curRow, curCol int
}

func (m *model) view() frame {
	f := frame{rows: make([]string, m.high)}
	w := m.width - 1 // never write the last column: no autowrap
	if m.tooSmall() {
		for i, l := range []string{"too small", fmt.Sprintf("%dx%d, need 40x12", m.width, m.high), "nothing is lost"} {
			if i < len(f.rows) {
				f.rows[i] = plain(l, w)
			}
		}
		return f
	}
	bh, dh := m.boardHeight(), m.detailHeight()
	row := 0
	f.rows[row] = m.titleRow(w)
	row++

	sel, hasSel := m.selected()
	for i := range bh {
		n := m.boardTop + i
		if n < len(m.board.Lines) {
			f.rows[row] = boardLine(m.board.Lines[n], hasSel && sel.Line == n, w)
		}
		row++
	}

	f.rows[row] = m.dividerRow(sel, hasSel, w)
	row++

	lines, cur := m.detailLines(sel, hasSel, w, dh)
	for i := range dh {
		if i < len(lines) {
			f.rows[row+i] = lines[i]
		}
	}
	if cur != nil {
		f.cursor, f.curRow, f.curCol = true, row+cur.row, cur.col
	}
	row += dh

	footer, fcur := m.footerRow(w)
	f.rows[row] = footer
	if fcur >= 0 {
		f.cursor, f.curRow, f.curCol = true, row, fcur
	}
	return f
}

func (m *model) titleRow(w int) string {
	s := " MoE ui"
	if m.board.Header != "" {
		s += " · " + m.board.Header
	}
	if m.busy != nil {
		s += " · working: " + m.busy.label
	}
	if m.message != "" {
		s += " · " + m.message
	}
	if n := m.dirtyDrafts(); n > 0 {
		s += " · " + plural(n, "unsaved draft")
	}
	return plain(s, w)
}

func (m *model) dividerRow(sel Item, ok bool, w int) string {
	label := "(nothing selected)"
	if ok {
		label = sel.Label()
	}
	focus := "board"
	if m.focus == paneDetail {
		focus = "details"
	}
	pos := ""
	if len(m.board.Lines) > m.boardHeight() {
		pos = fmt.Sprintf(" [%d–%d/%d]", m.boardTop+1, min(m.boardTop+m.boardHeight(), len(m.board.Lines)), len(m.board.Lines))
	}
	s := "── " + label + " ──" + pos + " · focus: " + focus + " "
	if pad := w - textWidth(s); pad > 0 {
		s += strings.Repeat("─", pad)
	}
	return plain(s, w)
}

// detailLines is the lower pane, already clipped, scrolled to detailTop
// (or to the editor's cursor), with the cursor's place when editing.
func (m *model) detailLines(sel Item, ok bool, w, h int) ([]string, *cell) {
	var src []string
	switch m.mode {
	case modeHelp:
		src = strings.Split(helpText, "\n")
	case modeEdit, modeSave:
		d := m.active
		if m.mode == modeSave {
			d = m.saveDraft
		}
		head := []string{"editing " + draftWhat(d.key) + " — Ctrl-S save · Esc back (kept) · Ctrl-X discard"}
		for _, l := range d.uncertain {
			head = append(head, "! "+l)
		}
		rows, cr, cc := d.ed.layout(max(w, 1))
		visible := h - len(head)
		top := 0
		if visible > 0 && cr >= visible {
			top = cr - visible + 1
		}
		var out []string
		for _, l := range head {
			out = append(out, plain(l, w))
		}
		for i := top; i < len(rows) && len(out) < h; i++ {
			out = append(out, rows[i]) // layout already made it safe and fit
		}
		if m.mode == modeSave || visible <= 0 {
			return out, nil
		}
		return out, &cell{row: len(head) + cr - top, col: cc}
	default:
		src = m.itemDetail(sel, ok)
	}
	top := min(m.detailTop, max(len(src)-h, 0))
	m.detailTop = top
	var out []string
	for i := top; i < len(src) && len(out) < h; i++ {
		out = append(out, plain(src[i], w))
	}
	return out, nil
}

func (m *model) itemDetail(it Item, ok bool) []string {
	if !ok {
		return []string{"Nothing to show: the board is empty."}
	}
	out := append([]string(nil), it.Detail...)
	for _, d := range m.draftsFor(it) {
		if d.dirty() {
			out = append(out, "unsaved "+draftWhat(d.key)+" — "+reopenKey(d.key.purpose)+" to continue it")
		}
		for _, l := range d.uncertain {
			out = append(out, "! "+l)
		}
	}
	if r := m.lastResult[it.Label()]; len(r) > 0 {
		out = append(out, "", "last action:")
		for _, l := range r {
			out = append(out, "  "+l)
		}
	}
	if it.Kind == ItemIdea || it.Kind == ItemIntent {
		out = append(out, "", "── canvas ──")
		out = append(out, strings.Split(strings.TrimRight(it.Body, "\n"), "\n")...)
	}
	return out
}

func reopenKey(purpose string) string {
	switch purpose {
	case "note":
		return "n"
	case "answer":
		return "a"
	case "body":
		return "e"
	}
	return ": new again"
}

func (m *model) draftsFor(it Item) []*draft {
	var out []*draft
	for _, d := range m.drafts {
		if d.key.project == it.Project && d.key.run == it.Run {
			out = append(out, d)
		}
	}
	return out
}

// footerRow is the key line, or the prompt with its cursor column.
func (m *model) footerRow(w int) (string, int) {
	switch m.mode {
	case modeCommand:
		hint := commandHint(m.cmd.String(), m.cmdTarget)
		rows, _, cc := m.cmd.layout(1 << 20)
		s := ":" + rows[0]
		line := plain(s+"   "+hint, w)
		return line, min(1+cc, w)
	case modeConfirm:
		return plain(m.confirm.prompt+" y/N", w), -1
	case modeSave:
		return plain("Enter/s save only · g save + tag sdlc · d save + tag design only · Esc back", w), -1
	case modeChoice:
		var parts []string
		for i, c := range m.choices {
			parts = append(parts, fmt.Sprintf("%d %s", i+1, c.Label))
		}
		return plain("reveal in which Rex app? "+strings.Join(parts, " · ")+" · Esc cancel", w), -1
	case modeEdit:
		return plain("Ctrl-S save · Esc back · Ctrl-X discard · Ctrl-C quit", w), -1
	case modeHelp:
		return plain("Esc close help", w), -1
	}
	keys := []string{"↑↓ select", "Tab pane"}
	if it, ok := m.selected(); ok {
		if it.Notes {
			keys = append(keys, "n note")
		}
		if it.Question != nil {
			keys = append(keys, "a answer")
		}
		if it.Editable {
			keys = append(keys, "e edit")
		}
		if it.Terminal {
			keys = append(keys, "t terminal")
		}
	}
	keys = append(keys, ": command", "? help", "q quit")
	return plain(" "+strings.Join(keys, "  "), w), -1
}

// plain makes external text safe and clips it to w cells.
func plain(s string, w int) string {
	var b bytes.Buffer
	_ = cliout.ClipLine(&b, []byte(sanitize(s)), w)
	return b.String()
}

// boardLine paints one dashboard line: its colour (SGR) passes through,
// every other control byte shows as caret notation, and it clips to w.
// The selected line's leading space becomes a marker that needs no
// colour to read.
func boardLine(line string, selected bool, w int) string {
	var b bytes.Buffer
	p := []byte(line)
	for len(p) > 0 {
		if n := cliout.CSILen(p); n > 0 && p[n-1] == 'm' {
			b.Write(p[:n])
			p = p[n:]
			continue
		}
		c := p[0]
		if c < 0x20 || c == 0x7f {
			b.WriteString(visible(rune(c)))
			p = p[1:]
			continue
		}
		b.WriteByte(c)
		p = p[1:]
	}
	s := b.Bytes()
	if selected {
		if len(s) > 0 && s[0] == ' ' {
			s = append([]byte{'>'}, s[1:]...)
		} else {
			s = append([]byte("> "), s...)
		}
	}
	var out bytes.Buffer
	_ = cliout.ClipLine(&out, []byte(sanitizeC1(string(s))), w)
	return out.String()
}

// sanitizeC1 replaces C1 controls (U+0080–U+009F), which some terminals
// honour as escapes, keeping everything else byte for byte.
func sanitizeC1(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x80 && r < 0xa0 {
			return '�'
		}
		return r
	}, s)
}

func textWidth(s string) int {
	n := 0
	for p := []byte(s); len(p) > 0; {
		size, w := cliout.TextUnit(p)
		n += w
		p = p[size:]
	}
	return n
}

// paint writes a frame over the previous one in place: absolute row
// positions, erase-to-end-of-line, one synchronized-output block. No
// full-screen erase, so a refresh never flashes.
func (f frame) paint() []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[?2026h\x1b[?25l")
	for i, r := range f.rows {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[0m\x1b[K", i+1, r)
	}
	if f.cursor {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", f.curRow+1, f.curCol+1)
	}
	b.WriteString("\x1b[?2026l")
	return b.Bytes()
}
