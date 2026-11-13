package ui

import (
	"strings"
	"time"
)

const consoleLimit = 100
const messageLimit = 16 << 10 // runes; one failure cannot consume unbounded history

type event struct {
	at   time.Time
	text string
}

type console struct {
	events []event
	top    int
	follow bool
	unread int
}

// say is the only entry to the UI's transient history. It never receives
// note or capture bodies, and never changes focus or the active editor.
func (m *model) say(text string) {
	m.message = text
	if text == "" {
		return
	}
	rs := []rune(text)
	if len(rs) > messageLimit {
		text = string(rs[:messageLimit]) + " [message truncated]"
	}
	c := &m.console
	if len(c.events) == consoleLimit {
		if !c.follow {
			c.top = max(0, c.top-len(eventRows(c.events[0], max(m.width-1, 1))))
		}
		copy(c.events, c.events[1:])
		c.events = c.events[:consoleLimit-1]
	}
	c.events = append(c.events, event{time.Now(), text})
	if !c.follow {
		c.unread++
	}
}

func eventRows(e event, width int) []string {
	var rows []string
	for i, line := range strings.Split(e.text, "\n") {
		prefix := "  "
		if i == 0 {
			prefix = e.at.Local().Format("15:04:05") + " "
		}
		rows = append(rows, wrapRows(prefix+line, width)...)
	}
	return rows
}

func (m *model) consoleRows() []string {
	var rows []string
	for _, e := range m.console.events {
		rows = append(rows, eventRows(e, max(m.width-1, 1))...)
	}
	return rows
}

func (m *model) consoleLines() []string {
	rows := m.consoleRows()
	c := &m.console
	end := max(len(rows)-m.consoleHeight(), 0)
	if c.follow {
		c.top = end
	} else {
		c.top = min(c.top, end)
	}
	return rows[c.top:min(c.top+m.consoleHeight(), len(rows))]
}

func (m *model) scrollConsole(k Key) {
	c := &m.console
	end := max(len(m.consoleRows())-m.consoleHeight(), 0)
	if c.follow {
		c.top = end
	}
	switch k.Code {
	case KeyUp:
		c.top--
	case KeyDown:
		c.top++
	case KeyPgUp:
		c.top -= m.consoleHeight()
	case KeyPgDn:
		c.top += m.consoleHeight()
	case KeyHome:
		c.top = 0
	case KeyEnd:
		c.top, c.follow, c.unread = end, true, 0
		return
	default:
		return
	}
	c.top = min(max(c.top, 0), end)
	c.follow = false // only End resumes following, including at the current end
}
