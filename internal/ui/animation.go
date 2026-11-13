package ui

import (
	"bytes"
	"fmt"
)

// animate touches only the cached, visible art span. In particular it must
// never call view: layout reads drafts and wraps details, and belongs to
// normal input/observation paints. last holds the cursor from that paint.
func (m *model) animate(last *frame) []byte {
	art := m.board.Art
	if len(art.Frames) < 2 {
		return nil
	}
	m.animation = (m.animation + 1) % len(art.Frames)
	if m.tooSmall() || m.mode == modeConfirm {
		return nil
	}
	var b bytes.Buffer
	sel, hasSel := m.selected()
	for i := 0; i < art.Height; i++ {
		line := art.Start + i
		if line < m.boardTop || line >= m.boardTop+m.boardHeight() {
			continue
		}
		row := 1 + line - m.boardTop
		next := boardLine(art.Frames[m.animation][i], hasSel && sel.Line == line, m.width-1)
		if next == last.rows[row] {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\x1b[?2026h\x1b[?25l")
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[0m\x1b[K", row+1, next)
		last.rows[row] = next
	}
	if b.Len() == 0 {
		return nil
	}
	if last.cursor {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", last.curRow+1, last.curCol+1)
	}
	b.WriteString("\x1b[?2026l")
	return b.Bytes()
}
