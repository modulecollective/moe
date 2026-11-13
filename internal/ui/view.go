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
  Tab/Shift-Tab  cycle board, details and console
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
  Alt-Backspace / Ctrl-W delete the previous whitespace-delimited word
  Alt-Left/Right or Alt-b/f move by a word

Commands (:)
  pause | handoff | park | finish | resume    the selected run
  new idea <project>/<slug>                   opens a draft; creates on save
  new intent <project>/<slug>
  new-idea / new-intent <project>/<slug>       aliases
  edit | close                                the selected idea or intent
  tag sdlc | tag design | untag               the selected idea's licence
  terminal | help | quit

Pane headings use a heavy rule for the input recipient (also without color).
Board ranges count scrollable lines, including headings and blank rows.
The complete chart and factory stay pinned when space permits; resize restores them.
Console: six message rows; arrows/page/Home scroll history; End follows newest.
Minimum terminal size: 40 columns by 20 rows.
Confirmations: y accepts, Esc cancels; arrows/page/Home/End scroll the question.

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
		for i, l := range []string{"too small", fmt.Sprintf("%dx%d, need 40x20", m.width, m.high), "nothing is lost"} {
			if i < len(f.rows) {
				f.rows[i] = plain(l, w)
			}
		}
		return f
	}
	if m.mode == modeConfirm {
		// A pending decision owns its screen: history position, arrivals and
		// the selected item's details cannot hide the action or its target.
		rows := m.confirmationRows()
		h := m.high - 3 // title, confirmation heading, footer
		top := min(m.confirm.top, max(len(rows)-h, 0))
		m.confirm.top = top
		f.rows[0] = m.titleRow(w)
		label := "Confirm" + m.statusLabel()
		label += fmt.Sprintf(" [%d–%d/%d]", top+1, min(top+h, len(rows)), len(rows))
		f.rows[1] = m.heading(label, true, w)
		copy(f.rows[2:m.high-1], rows[top:min(top+h, len(rows))])
		f.rows[m.high-1], _ = m.footerRow(w)
		return f
	}
	l := m.layout()
	bh, dh := l.board, l.detail
	f.rows[0] = m.titleRow(w)
	for i := range l.overview {
		line := m.board.Overview[i]
		art := m.board.Art
		if i >= art.Start && i < art.Start+art.Height && len(art.Frames) > 0 {
			line = art.Frames[m.animation%len(art.Frames)][i-art.Start]
		}
		f.rows[l.overviewRow+i] = boardLine(line, false, w)
	}
	f.rows[l.boardRule] = m.boardHeading(w)
	row := l.boardRow

	sel, hasSel := m.selected()
	for i := range bh {
		n := m.boardTop + i
		if n < len(m.board.Lines) {
			f.rows[row] = boardLine(m.board.Lines[n], hasSel && sel.Line == n, w)
		}
		row++
	}

	f.rows[l.detailRule] = m.dividerRow(sel, hasSel, w)
	row = l.detailRow

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

	label := "Console"
	if m.console.unread > 0 {
		label += fmt.Sprintf(" · %d new (End follows)", m.console.unread)
	}
	f.rows[l.consoleRule] = m.heading(label, m.paneFocused(paneConsole), w)
	row = l.consoleRow
	for i, line := range m.consoleLines() {
		f.rows[row+i] = line
	}
	row += m.consoleHeight()
	footer, fcur := m.footerRow(w)
	f.rows[row] = footer
	if fcur >= 0 {
		f.cursor, f.curRow, f.curCol = true, row, fcur
	}
	return f
}

func (m *model) titleRow(w int) string {
	return boardLine(m.board.Banner, false, w)
}

// paneFocused follows the actual input recipient, including modal input.
func (m *model) paneFocused(p pane) bool {
	switch m.mode {
	case modeEdit, modeSave, modeHelp:
		return p == paneDetail
	case modeCommand, modeChoice, modeConfirm:
		return false
	}
	return m.focus == p
}

func (m *model) heading(label string, focused bool, w int) string {
	rule, color := "─", cliout.Dim
	if focused {
		rule, color = "━", cliout.Bright
	}
	s := plain(strings.Repeat(rule, 2)+" "+label+" ", w)
	if pad := w - textWidth(s); pad > 0 {
		s += strings.Repeat(rule, pad)
	}
	if m.color {
		return color + s + cliout.Reset
	}
	return s
}

func (m *model) statusLabel() string {
	s := ""
	// Preserve both state cues even when a job's label or filters are long.
	if n := m.dirtyDrafts(); n > 0 {
		s += " · " + plural(n, "unsaved draft")
	}
	if m.busy != nil {
		s += " · working: " + m.busy.label
	}
	return s
}

func (m *model) boardHeading(w int) string {
	s := "Board" + m.statusLabel()
	if m.board.Header != "" {
		s += " · " + m.board.Header
	}
	if len(m.board.Lines) > m.boardHeight() {
		pos := fmt.Sprintf(" · lines %d–%d of %d", m.boardTop+1, min(m.boardTop+m.boardHeight(), len(m.board.Lines)), len(m.board.Lines))
		if textWidth(SafeText(s+pos))+4 <= w {
			s += pos
		}
	}
	return m.heading(s, m.paneFocused(paneBoard), w)
}

func (m *model) dividerRow(sel Item, ok bool, w int) string {
	label := "(nothing selected)"
	if ok {
		label = sel.Label()
	}
	if m.mode == modeEdit && m.active != nil {
		label = draftWhat(m.active.key)
	}
	if m.mode == modeSave {
		label = draftWhat(m.saveDraft.key)
	}
	if m.mode == modeHelp {
		label = "Help"
	}
	return m.heading("Details · "+label, m.paneFocused(paneDetail), w)
}

// detailLines is the lower pane, already clipped, scrolled to detailTop
// (or to the editor's cursor), with the cursor's place when editing.
func (m *model) detailLines(sel Item, ok bool, w, h int) ([]string, *cell) {
	var src []string
	switch m.mode {
	case modeHelp:
		src = strings.Split(helpText, "\n")
	case modeSave:
		if h < 3 {
			return []string{"g  save + tag sdlc", "d  save + tag design only"}, nil
		}
		return []string{"Enter/s  save only", "g  save + tag sdlc", "d  save + tag design only"}, nil
	case modeEdit:
		d := m.active
		var head []string
		for _, l := range m.draftNotes(d) {
			head = append(head, wrapRows(l, w)...)
		}
		// At most half the pane: the text being edited keeps the rest,
		// and the details (Esc) carry every line.
		head = head[:min(len(head), max(h/2, 1))]
		rows, cr, cc := d.ed.layout(max(w, 1))
		visible := h - len(head)
		top := 0
		if visible > 0 && cr >= visible {
			top = cr - visible + 1
		}
		var out []string
		out = append(out, head...)
		for i := top; i < len(rows) && len(out) < h; i++ {
			out = append(out, rows[i]) // layout already made it safe and fit
		}
		if visible <= 0 {
			return out, nil
		}
		return out, &cell{row: len(head) + cr - top, col: cc}
	default:
		src = m.itemDetail(sel, ok)
	}
	// Wrapped, not clipped: every character of a long line is reachable
	// by scrolling.
	var rows []string
	for _, l := range src {
		rows = append(rows, wrapRows(l, w)...)
	}
	top := min(m.detailTop, max(len(rows)-h, 0))
	m.detailTop = top
	return rows[top:min(top+h, len(rows))], nil
}

// wrapRows makes s safe to paint and breaks it into rows of at most w
// cells.
func wrapRows(s string, w int) []string {
	p := []byte(sanitize(s))
	var rows []string
	start, cells := 0, 0
	for i := 0; i < len(p); {
		size, cw := cliout.TextUnit(p[i:])
		if cells+cw > w && i > start {
			rows = append(rows, string(p[start:i]))
			start, cells = i, 0
		}
		cells += cw
		i += size
	}
	return append(rows, string(p[start:]))
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
		out = append(out, m.draftNotes(d)[1:]...)
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
		line, cc := m.cmd.horizontal(max(w-4, 1))
		if textWidth(line)+3+textWidth(hint) < w-4 {
			line += "   " + hint
		}
		return m.heading(":"+line, true, w), 4 + cc
	case modeConfirm:
		if len(m.confirmationRows()) > m.high-3 {
			return plain("y confirm · Esc cancel · ↑↓/Pg scroll", w), -1
		}
		return plain("y confirm · n/Esc cancel", w), -1
	case modeSave:
		if m.detailHeight() >= 3 {
			return plain("Choose above · Esc back", w), -1
		}
		return plain("Enter/s  save only · Esc back", w), -1
	case modeChoice:
		var parts []string
		for i, c := range m.choices {
			parts = append(parts, fmt.Sprintf("%d %s", i+1, c.Label))
		}
		return m.heading("reveal in which Rex app? "+strings.Join(parts, " · ")+" · Esc cancel", true, w), -1
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
		if it.Question != nil || m.staleAnswer(it) != nil {
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

// SafeText escapes external labels before a renderer adds trusted SGR.
// It also keeps a label on one physical line, preserving indexed row positions.
func SafeText(s string) string { return sanitize(s) }

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
		if size := cliout.CSILen(p); size > 0 {
			p = p[size:]
			continue
		}
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

// draftNotes is what the editor says above a draft: what it is and the
// keys, then why its last save failed or may not have landed, and
// whether its question has closed.
func (m *model) draftNotes(d *draft) []string {
	out := []string{"editing " + draftWhat(d.key)}
	if m.answerClosed(d.key) {
		out = append(out, fmt.Sprintf("! question #%d is no longer open: saving will be refused; Ctrl-X discards this answer", d.key.question))
	}
	if m.targetClosed(d.key) {
		what := "notes"
		if d.key.purpose == "body" {
			what = "edits"
		}
		out = append(out, "! "+d.key.project+"/"+d.key.run+" no longer takes "+what+": saving will be refused; Ctrl-X discards it")
	}
	for _, l := range d.refused {
		out = append(out, "! "+l)
	}
	for _, l := range d.uncertain {
		out = append(out, "! "+l)
	}
	return out
}
