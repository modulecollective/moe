package transcript

import (
	"fmt"
	"io"
	"strings"
)

// RenderOptions tune the renderer's output. Zero-value defaults are
// "show everything readable", which is what `moe log` and the
// auto-tail both want. Future verbose modes (thinking, raw JSON) hook
// in here without touching the call site.
type RenderOptions struct {
	// MaxOutputLines caps how many lines of tool output to print
	// before eliding the middle. Zero means use the package default.
	// Negative means no elision.
	MaxOutputLines int
}

const (
	// defaultMaxOutputLines is the "absurdly long" threshold from the
	// design. 40 keeps the most useful operator surface (the head and
	// tail of a Bash run, a small Read, an error message) intact
	// while collapsing the middle of a long file read or test-runner
	// dump that nobody scrolls through anyway.
	defaultMaxOutputLines = 40
	// elisionHead / elisionTail are the per-side counts when elision
	// kicks in. 8/8 keeps both ends meaningful for shell output where
	// the early lines establish what was happening and the late lines
	// carry the error or summary.
	elisionHead = 8
	elisionTail = 8
)

// Render writes ev to w in operator-readable plain text. Each event
// gets a header line ("12:34:56 assistant") and a body indented by
// two spaces. ToolResult events fold directly under the preceding
// event (no blank separator, no header of their own) so a call and
// its result read as one tight block — both backends emit them in
// call-then-result order, so adjacency is the pairing.
//
// Returns the first write error (io errors abort the render rather
// than silently dropping later events). Zero events render to an
// empty writer — callers print their own "no transcript" notice.
func Render(w io.Writer, ev []Event, opts RenderOptions) error {
	max := opts.MaxOutputLines
	if max == 0 {
		max = defaultMaxOutputLines
	}
	for i, e := range ev {
		if i > 0 && e.Kind != KindToolResult {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if err := renderEvent(w, e, max); err != nil {
			return err
		}
	}
	return nil
}

func renderEvent(w io.Writer, e Event, maxOut int) error {
	switch e.Kind {
	case KindUserText, KindAssistantText, KindSystem:
		if _, err := fmt.Fprintln(w, eventHeader(e)); err != nil {
			return err
		}
		return writeIndented(w, e.Text, "  ")
	case KindToolCall:
		// One-line tool call: "12:34:56 tool: Read(path/to/file)".
		// Args may be empty for tools we don't summarise; render
		// "Name()" rather than just "Name" so it's clear it's a
		// call.
		name := e.Tool
		if name == "" {
			name = "<tool>"
		}
		_, err := fmt.Fprintf(w, "%s %s(%s)\n", eventHeader(e), name, e.Args)
		return err
	case KindToolResult:
		// No header — the result folds under its preceding call.
		// Errored results get a single "  ✗ failed" marker line so
		// the operator sees the failure at a glance even when the
		// output is empty.
		if e.Error {
			if _, err := fmt.Fprintln(w, "  ✗ failed"); err != nil {
				return err
			}
		}
		return writeIndented(w, elide(e.Output, maxOut), "    ")
	}
	return nil
}

// eventHeader returns the per-event header line: "HH:MM:SS kind".
// Zero timestamps are rendered without the time prefix so the renderer
// still produces something useful on transcripts where an adapter
// couldn't recover one.
func eventHeader(e Event) string {
	label := kindLabel(e.Kind)
	if e.Time.IsZero() {
		return label
	}
	return e.Time.Local().Format("15:04:05") + " " + label
}

func kindLabel(k Kind) string {
	switch k {
	case KindUserText:
		return "user"
	case KindAssistantText:
		return "assistant"
	case KindToolCall:
		return "tool:"
	case KindSystem:
		return "system"
	}
	return "?"
}

// writeIndented prefixes every non-empty line of s with indent and
// writes it to w. An empty s writes nothing. Trailing whitespace per
// line is preserved (Bash output, for instance, sometimes ends in
// trailing spaces that matter to the operator scanning for stray
// characters).
func writeIndented(w io.Writer, s, indent string) error {
	if s == "" {
		return nil
	}
	// Trim a single trailing newline so the per-block separator
	// (the blank line Render writes between events) doesn't compound
	// with an output that already ends in \n.
	s = strings.TrimRight(s, "\n")
	for _, line := range strings.Split(s, "\n") {
		if _, err := fmt.Fprintf(w, "%s%s\n", indent, line); err != nil {
			return err
		}
	}
	return nil
}

// elide collapses s to its head and tail when it exceeds maxLines.
// Returns s unchanged when it's short enough or when maxLines is
// negative (caller opted out of elision). Adds a "[N lines elided]"
// marker between the kept halves so the operator knows something is
// missing rather than wondering why a 500-line file read looks tiny.
func elide(s string, maxLines int) string {
	if s == "" || maxLines < 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= maxLines {
		return s
	}
	if elisionHead+elisionTail >= len(lines) {
		return s
	}
	head := lines[:elisionHead]
	tail := lines[len(lines)-elisionTail:]
	elided := len(lines) - elisionHead - elisionTail
	parts := make([]string, 0, len(head)+1+len(tail))
	parts = append(parts, head...)
	parts = append(parts, fmt.Sprintf("[%d lines elided]", elided))
	parts = append(parts, tail...)
	return strings.Join(parts, "\n")
}
