package ui

import (
	"strings"
	"unicode"
)

// Keep the original prefix's matches while Tab replaces the word, so an
// ambiguous prefix can still cycle after the first match is inserted.
type commandCompletion struct {
	start, end int
	matches    []string
	index      int
}

func commandMatches(e *editor) *commandCompletion {
	start, end := e.cur, e.cur
	for start > 0 && !unicode.IsSpace(e.text[start-1]) {
		start--
	}
	for end < len(e.text) && !unicode.IsSpace(e.text[end]) {
		end++
	}
	var words []string
	before := strings.Fields(string(e.text[:start]))
	if len(before) == 0 {
		words = []string{"close", "edit", "finish", "handoff", "help", "new-idea", "new-intent", "park", "pause", "quit", "resume", "tag-design", "tag-sdlc", "terminal", "untag"}
	}
	c := &commandCompletion{start: start, end: end, index: -1}
	prefix := string(e.text[start:e.cur])
	for _, word := range words {
		if strings.HasPrefix(word, prefix) {
			c.matches = append(c.matches, word)
		}
	}
	return c
}

func (m *model) completeCommand(backward bool) {
	if m.completion == nil {
		m.completion = commandMatches(m.cmd)
	}
	c, e := m.completion, m.cmd
	if len(c.matches) == 0 {
		return
	}
	next := c.index + 1
	if backward {
		next = c.index - 1
		if c.index == -1 {
			next = len(c.matches) - 1
		}
	}
	next = (next + len(c.matches)) % len(c.matches)
	word := []rune(c.matches[next])
	if len(e.text)-(c.end-c.start)+len(word) > maxText {
		m.say("command text limit reached; nothing inserted")
		return
	}
	e.text = append(e.text[:c.start], append(word, e.text[c.end:]...)...)
	e.cur, e.want = c.start+len(word), -1
	c.end, c.index = e.cur, next
}
