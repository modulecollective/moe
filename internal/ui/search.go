package ui

import (
	"fmt"
	"slices"
	"strings"
)

type boardSearch struct {
	ed                               *editor
	candidates                       []int // occurrences in board.Items, in board order
	anchor                           itemKey
	hadAnchor                        bool
	index, line, boardTop, detailTop int
	focus                            pane
}

func (m *model) openSearch() {
	it, _ := m.selected()
	m.search = &boardSearch{
		ed: newEditor("", true), anchor: m.sel, hadAnchor: m.has,
		index: m.selectedIndex(), line: it.Line,
		boardTop: m.boardTop, detailTop: m.detailTop, focus: m.focus,
	}
	m.mode = modeSearch
	m.reconcileSearch(m.sel, m.has)
	m.boardTop = m.search.boardTop
}

// reconcileSearch uses identity from before ordinary board reconciliation,
// so a refresh keeps the matching occurrence even when a save selects elsewhere.
func (m *model) reconcileSearch(previous itemKey, had bool) {
	s := m.search
	query := strings.ToLower(strings.TrimSpace(s.ed.String()))
	s.candidates = nil
	chosen := -1
	for i, it := range m.board.Items {
		if !strings.Contains(strings.ToLower(it.Label()), query) {
			continue
		}
		s.candidates = append(s.candidates, i)
		if had && keyOf(it) == previous {
			chosen = i
		}
	}
	if chosen < 0 && had {
		for _, i := range s.candidates {
			it := m.board.Items[i]
			if it.Project == previous.project && it.Run == previous.run {
				chosen = i
				break
			}
		}
	}
	if chosen < 0 && len(s.candidates) > 0 {
		chosen = s.candidates[0]
	}
	if chosen < 0 {
		if had {
			m.detailTop = 0
		}
		m.has = false
		return
	}
	k := keyOf(m.board.Items[chosen])
	if !had || previous != k {
		m.detailTop = 0
	}
	m.sel, m.has = k, true
	m.scrollToSelection()
}

func (m *model) searchKey(k Key) effect {
	s := m.search
	switch k.Code {
	case KeyEsc, KeyCtrlC:
		m.cancelSearch()
	case KeyEnter:
		if m.has {
			m.search, m.mode, m.focus = nil, modeNormal, paneBoard
		}
	case KeyUp, KeyDown:
		pos := slices.Index(s.candidates, m.selectedIndex())
		if pos >= 0 {
			delta := 1
			if k.Code == KeyUp {
				delta = -1
			}
			i := s.candidates[min(max(pos+delta, 0), len(s.candidates)-1)]
			if keyOf(m.board.Items[i]) != m.sel {
				m.sel, m.detailTop = keyOf(m.board.Items[i]), 0
			}
			m.scrollToSelection()
		}
	default:
		before := s.ed.String()
		if !s.ed.key(k) && (k.Code == KeyRune || k.Code == KeyPaste) {
			m.say("search text limit reached; nothing inserted")
		}
		if k.Code == KeyPaste && k.Truncated {
			m.say("search paste truncated at 256 KiB")
		}
		if s.ed.String() != before {
			m.reconcileSearch(m.sel, m.has)
		}
	}
	return effNone
}

func (m *model) cancelSearch() {
	s := m.search
	m.search, m.mode, m.focus = nil, modeNormal, s.focus
	m.has = false
	i := -1
	if s.hadAnchor {
		for n, it := range m.board.Items {
			if keyOf(it) == s.anchor {
				i = n
				break
			}
		}
		if i < 0 {
			for n, it := range m.board.Items {
				if it.Project == s.anchor.project && it.Run == s.anchor.run {
					i = n
					break
				}
			}
		}
	}
	if i < 0 && len(m.board.Items) > 0 {
		i = min(max(s.index, 0), len(m.board.Items)-1)
		if s.hadAnchor {
			m.say(s.anchor.project + "/" + s.anchor.run + " left the board; selected " + m.board.Items[i].Label())
		}
	}
	m.boardTop, m.detailTop = s.boardTop, s.detailTop
	m.clampBoard()
	if i >= 0 {
		it := m.board.Items[i]
		m.sel, m.has = keyOf(it), true
		if !s.hadAnchor || m.sel != s.anchor || it.Line != s.line {
			m.scrollToSelection()
		}
	}
}

func (m *model) searchFooter(w int) (string, int) {
	s := m.search
	status := "No matches"
	if len(s.candidates) > 0 {
		status = fmt.Sprintf("%d/%d", slices.Index(s.candidates, m.selectedIndex())+1, len(s.candidates))
	}
	hint := " " + status + " Enter/Esc"
	// Pad the query window so the hints never compete with query text.
	queryWidth := max(w-2-textWidth(hint), 1)
	line, col := s.ed.horizontal(queryWidth)
	return plain("/ "+line+strings.Repeat(" ", max(queryWidth-textWidth(line), 0))+hint, w), 2 + col
}
