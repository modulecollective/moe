package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modulecollective/moe/internal/cliout"
)

// maxText bounds what one editor holds. Notes and captures are short;
// typing or pasting past this is refused and said, never cut silently.
const maxText = 256 << 10

// editor is the small plain-text buffer behind notes, answers, capture
// bodies and the command line: insertion, arrows, Home/End,
// Backspace/Delete, and paste. No selection, undo or rendering of
// Markdown — the design trades a full editor for a bounded one.
type editor struct {
	text   []rune
	cur    int  // insertion point, 0..len(text)
	want   int  // column Up/Down aims for; -1 for "current"
	single bool // one line: newlines are refused (the command line)
	width  int  // last layout width, for Up/Down
}

func newEditor(text string, single bool) *editor {
	e := &editor{text: []rune(text), want: -1, single: single}
	e.cur = len(e.text)
	return e
}

func (e *editor) String() string { return string(e.text) }

// insert adds s at the cursor. It refuses (false) text that would push
// the buffer past maxText.
func (e *editor) insert(s string) bool {
	if e.single {
		s = strings.ReplaceAll(s, "\n", " ")
	}
	if utf8.RuneCountInString(s)+len(e.text) > maxText {
		return false
	}
	rs := []rune(s)
	e.text = append(e.text[:e.cur], append(rs, e.text[e.cur:]...)...)
	e.cur += len(rs)
	e.want = -1
	return true
}

// key applies an editing key. It reports false for keys it does not
// handle (the caller's: submit, escape, pane switches) and for an insert
// refused at maxText — the caller says which.
func (e *editor) key(k Key) bool {
	switch k.Code {
	case KeyRune:
		return e.insert(string(k.Rune))
	case KeyPaste:
		return e.insert(k.Text)
	case KeyEnter:
		if e.single {
			return false
		}
		return e.insert("\n")
	case KeyWordLeft:
		e.cur = e.wordBack()
	case KeyWordRight:
		for e.cur < len(e.text) && unicode.IsSpace(e.text[e.cur]) {
			e.cur++
		}
		for e.cur < len(e.text) && !unicode.IsSpace(e.text[e.cur]) {
			e.cur++
		}
	case KeyWordBackspace:
		start := e.wordBack()
		e.text = append(e.text[:start], e.text[e.cur:]...)
		e.cur = start
	case KeyBackspace:
		if e.cur > 0 {
			e.text = append(e.text[:e.cur-1], e.text[e.cur:]...)
			e.cur--
		}
	case KeyDelete:
		if e.cur < len(e.text) {
			e.text = append(e.text[:e.cur], e.text[e.cur+1:]...)
		}
	case KeyLeft:
		if e.cur > 0 {
			e.cur--
		}
	case KeyRight:
		if e.cur < len(e.text) {
			e.cur++
		}
	case KeyHome:
		for e.cur > 0 && e.text[e.cur-1] != '\n' {
			e.cur--
		}
	case KeyEnd:
		for e.cur < len(e.text) && e.text[e.cur] != '\n' {
			e.cur++
		}
	case KeyUp, KeyDown:
		if e.single {
			return false
		}
		e.vertical(k.Code == KeyUp)
		return true
	default:
		return false
	}
	e.want = -1
	return true
}

func (e *editor) wordBack() int {
	i := e.cur
	for i > 0 && unicode.IsSpace(e.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.text[i-1]) {
		i--
	}
	return i
}

// vertical moves the cursor one visual row, keeping its column.
func (e *editor) vertical(up bool) {
	width := e.width
	if width <= 0 {
		width = maxText // not laid out yet: no wrapping
	}
	pos := e.positions(width)
	row, col := pos[e.cur].row, pos[e.cur].col
	if e.want < 0 {
		e.want = col
	}
	target := row + 1
	if up {
		target = row - 1
	}
	best := -1
	for i, p := range pos {
		if p.row == target && p.col <= e.want {
			best = i
		}
	}
	if best >= 0 {
		e.cur = best
	} else if up {
		e.cur = 0
	} else {
		e.cur = len(e.text)
	}
}

type cell struct{ row, col int }

// positions is where each insertion point 0..len(text) sits when the text
// wraps at width cells.
func (e *editor) positions(width int) []cell {
	pos := make([]cell, len(e.text)+1)
	row, col := 0, 0
	for i, r := range e.text {
		w := displayWidth(r)
		if r != '\n' && col+w > width && col > 0 {
			row, col = row+1, 0
		}
		pos[i] = cell{row, col}
		if r == '\n' {
			row, col = row+1, 0
			continue
		}
		col += w
	}
	if col >= width && width > 0 {
		row, col = row+1, 0
	}
	pos[len(e.text)] = cell{row, col}
	return pos
}

// layout wraps the text at width and returns the visible rows with the
// cursor's row and column. Control characters show as ^X, never as
// terminal input.
func (e *editor) layout(width int) (rows []string, curRow, curCol int) {
	e.width = width
	pos := e.positions(width)
	var b strings.Builder
	row := 0
	for i, r := range e.text {
		for pos[i].row > row {
			rows = append(rows, b.String())
			b.Reset()
			row++
		}
		if r != '\n' {
			b.WriteString(visible(r))
		}
	}
	for pos[len(e.text)].row > row {
		rows = append(rows, b.String())
		b.Reset()
		row++
	}
	rows = append(rows, b.String())
	return rows, pos[e.cur].row, pos[e.cur].col
}

// visible is how r is shown: itself, or caret notation for a control.
func visible(r rune) string {
	switch {
	case r < 0x20:
		return "^" + string(rune(r+'@'))
	case r == 0x7f:
		return "^?"
	case r >= 0x80 && r < 0xa0:
		return "�"
	}
	return string(r)
}

func displayWidth(r rune) int {
	if r == '\n' {
		return 0
	}
	if v := visible(r); v != string(r) {
		return len(v)
	}
	return cliout.RuneWidth(r)
}

// sanitize makes external text (titles, notes, errors, bodies) safe to
// paint: every control character becomes visible caret notation.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		b.WriteString(visible(r))
	}
	return b.String()
}

// horizontal keeps the command cursor reachable without splitting runes.
func (e *editor) horizontal(width int) (string, int) {
	start, col := e.cur, 0
	for start > 0 && col+displayWidth(e.text[start-1]) < width-1 {
		start--
		col += displayWidth(e.text[start])
	}
	var b strings.Builder
	used := 0
	if start > 0 {
		b.WriteByte('<')
		used++
		col++
	}
	for _, r := range e.text[start:] {
		n := displayWidth(r)
		if used+n > width {
			break
		}
		b.WriteString(visible(r))
		used += n
	}
	return b.String(), col
}
