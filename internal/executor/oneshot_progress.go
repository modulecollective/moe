package executor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// streamEvent is the minimum subset of `claude -p --output-format
// stream-json` we care about for human-readable progress lines. The
// process emits one JSON object per line; unknown types and unknown
// fields are ignored — we lift only what the operator can act on.
type streamEvent struct {
	Type    string         `json:"type"`
	Message *streamMessage `json:"message,omitempty"`
}

type streamMessage struct {
	Content []streamBlock `json:"content,omitempty"`
}

type streamBlock struct {
	Type  string         `json:"type"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// pipeOneShotProgress reads claude's stream-json output from r and
// writes one short progress line per tool call to w, e.g. `> reading
// projects/foo/runs/bar/documents/design/content.md`. Absolute paths
// under trimRoot are rendered repo-relative so the lines stay short.
//
// The goal is "operator can see it's alive and roughly what it's
// doing", not "operator can debug from the terminal" — raw JSON is
// never surfaced. Malformed lines and unknown event types are dropped
// silently because the alternative is spamming the terminal with parse
// errors the operator can't act on. Returns when r reaches EOF.
func pipeOneShotProgress(r io.Reader, w io.Writer, trimRoot string) {
	scanner := bufio.NewScanner(r)
	// Stream-json messages can carry tool inputs much larger than
	// bufio.Scanner's 64KiB default (a Bash command + diff output, an
	// Edit's old/new strings, etc.). Allow up to 8 MiB per line — past
	// that the line is dropped, which is fine: we'd render nothing
	// useful for it anyway.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type != "assistant" || ev.Message == nil {
			continue
		}
		for _, b := range ev.Message.Content {
			if b.Type != "tool_use" {
				continue
			}
			fmt.Fprintf(w, "> %s\n", renderToolCall(b.Name, b.Input, trimRoot))
		}
	}
}

// renderToolCall returns the one-line summary for a tool_use block.
// Falls back to the bare tool name when the input shape doesn't match
// what we know how to summarise — better a vague "> WebSearch" than a
// silent gap that makes the agent look hung.
func renderToolCall(name string, input map[string]any, trimRoot string) string {
	switch name {
	case "Read":
		if p, ok := stringField(input, "file_path"); ok {
			return "reading " + relPath(p, trimRoot)
		}
	case "Edit":
		if p, ok := stringField(input, "file_path"); ok {
			return "editing " + relPath(p, trimRoot)
		}
	case "Write":
		if p, ok := stringField(input, "file_path"); ok {
			return "writing " + relPath(p, trimRoot)
		}
	case "Bash":
		if cmd, ok := stringField(input, "command"); ok {
			return "bash: " + truncate(cmd, 80)
		}
	case "Grep":
		if pat, ok := stringField(input, "pattern"); ok {
			return "grep: " + truncate(pat, 80)
		}
	case "Glob":
		if pat, ok := stringField(input, "pattern"); ok {
			return "glob: " + pat
		}
	case "Task":
		if d, ok := stringField(input, "description"); ok {
			return "task: " + truncate(d, 80)
		}
	}
	return name
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// relPath renders an absolute path as repo-relative when it's under
// trimRoot, so the operator's terminal isn't full of long absolute
// paths. Anything outside trimRoot (or non-absolute) passes through.
func relPath(p, trimRoot string) string {
	if trimRoot == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(trimRoot, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
