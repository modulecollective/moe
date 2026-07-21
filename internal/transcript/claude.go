package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func init() {
	Register("claude", parseClaude)
}

// claudeEvent is the subset of a `thread-claude.jsonl` line we care
// about. Claude logs many bookkeeping shapes (file-history snapshots,
// queue ops, bridge state, the cached deferred-tool list, …); they
// have no `message` and no `subtype` we render, so they fall through
// the switch in parseClaude and produce zero events. Unknown top-level
// fields are ignored.
type claudeEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	Content   string         `json:"content,omitempty"`
	Timestamp string         `json:"timestamp,omitempty"`
	Message   *claudeMessage `json:"message,omitempty"`
}

type claudeMessage struct {
	Role    string          `json:"role,omitempty"`
	Model   string          `json:"model,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type claudeBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// tool_result.content is sometimes a string and sometimes an
	// array of content blocks ({"type":"text","text":"…"}); decode
	// it as RawMessage and split the cases in code.
	ResultContent json.RawMessage `json:"content,omitempty"`
}

// parseClaude turns a `thread-claude.jsonl` reader into normalised
// Events. The Claude SDK transcript is one JSON object per line; a
// single assistant `message` carrying both `text` and `tool_use`
// blocks fans out to multiple Events so the renderer's per-event
// switch stays simple. Malformed lines and unknown event types are
// skipped — the file is forensic, not authoritative, and a parse
// error a third of the way through shouldn't lose the head and tail
// the operator most wants to see.
func parseClaude(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	// Tool inputs and outputs can be much larger than the 64 KiB
	// default — bash command output, file edits, web fetches. Allow
	// 8 MiB per line; past that we drop the line rather than fail
	// the whole parse.
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var out []Event
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		ts := parseClaudeTimestamp(ev.Timestamp)
		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			out = append(out, claudeAssistantEvents(ev.Message, ts)...)
		case "user":
			if ev.Message == nil {
				continue
			}
			out = append(out, claudeUserEvents(ev.Message, ts)...)
		case "system":
			// Bridge-status and similar `system` events carry a
			// flat `content` string; surface them as KindSystem so
			// the operator sees sandbox denials and remote-control
			// notices without digging into the JSONL.
			if ev.Content != "" {
				out = append(out, Event{Kind: KindSystem, Time: ts, Text: ev.Content})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("transcript: scan claude jsonl: %w", err)
	}
	return out, nil
}

func claudeAssistantEvents(m *claudeMessage, ts time.Time) []Event {
	var blocks []claudeBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	// "<synthetic>" is an SDK-internal placeholder observed on real
	// assistant messages the model never actually produced (queued-input
	// echoes and the like); treat it as "no model" so it doesn't show up
	// as a spurious chip.
	model := m.Model
	if model == "<synthetic>" {
		model = ""
	}
	var out []Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			out = append(out, Event{Kind: KindAssistantText, Time: ts, Text: b.Text, Model: model})
		case "tool_use":
			out = append(out, Event{
				Kind:   KindToolCall,
				Time:   ts,
				Tool:   b.Name,
				Args:   summariseClaudeInput(b.Name, b.Input),
				CallID: b.ID,
				Model:  model,
			})
			// "thinking" is intentionally dropped in v1 — the operator
			// asked for context after a one-shot bails, not a replay
			// of the agent's internal monologue. Add a verbose mode
			// later if it turns out to be useful.
		}
	}
	return out
}

func claudeUserEvents(m *claudeMessage, ts time.Time) []Event {
	// A user message's content is either a plain string (the operator
	// typed something) or an array of tool_result blocks (the harness
	// fed tool output back to the model). Try string first; on any
	// failure fall through to block-array decoding.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if s == "" {
			return nil
		}
		return []Event{{Kind: KindUserText, Time: ts, Text: s}}
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var out []Event
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		out = append(out, Event{
			Kind:   KindToolResult,
			Time:   ts,
			CallID: b.ToolUseID,
			Output: decodeClaudeResultContent(b.ResultContent),
			Error:  b.IsError,
		})
	}
	return out
}

// decodeClaudeResultContent flattens tool_result.content, which the
// Claude SDK sometimes serialises as a plain string and sometimes as
// an array of typed blocks (`{"type":"text","text":"…"}`). Anything
// else turns into an empty string so the renderer prints the call
// without dumping a JSON blob.
func decodeClaudeResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// summariseClaudeInput produces the short "(args)" string the renderer
// shows next to a tool name. Mirrors the per-tool cases in
// internal/agent/claude/oneshot_progress.go so the one-shot progress
// line and the `moe <workflow> log` line read the same. Unknown tools render as
// an empty summary — better blank than a noisy JSON dump.
func summariseClaudeInput(name string, in map[string]any) string {
	switch name {
	case "Read", "Edit", "Write", "NotebookEdit":
		if p, ok := stringField(in, "file_path"); ok {
			return p
		}
	case "Bash":
		if c, ok := stringField(in, "command"); ok {
			return collapseWhitespace(c)
		}
	case "Grep":
		if p, ok := stringField(in, "pattern"); ok {
			return p
		}
	case "Glob":
		if p, ok := stringField(in, "pattern"); ok {
			return p
		}
	case "Task", "Agent":
		if d, ok := stringField(in, "description"); ok {
			return d
		}
	case "WebFetch":
		if u, ok := stringField(in, "url"); ok {
			return u
		}
	case "WebSearch":
		if q, ok := stringField(in, "query"); ok {
			return q
		}
	}
	return ""
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func parseClaudeTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func init() {
	RegisterUsage("claude", parseClaudeUsage)
}

// claudeUsageLine is the usage-bearing subset of a `thread-claude.jsonl`
// assistant line.
type claudeUsageLine struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id,omitempty"`
		Model string `json:"model,omitempty"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`
}

// parseClaudeUsage sums the per-message `usage` blocks in a
// `thread-claude.jsonl`.
//
// The load-bearing detail is the dedupe. Claude Code writes one line per
// *content block*, not per message: an assistant turn with a thinking
// block, some text and three tool calls lands as five lines, each
// carrying the same `message.id` and a verbatim copy of the same `usage`
// object. Summing lines therefore overcounts by the mean blocks-per-turn
// — on real stage transcripts that is a factor of two or more, which is
// enough to reverse a "which workflow costs most" comparison. Keying on
// message.id collapses each turn back to one.
//
// A line with usage but no id (a shape the SDK doesn't currently write,
// but the file is forensic and unversioned) counts once rather than
// being dropped: under-reporting cost is the worse failure.
func parseClaudeUsage(r io.Reader) (Usage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	out := Usage{}
	seen := map[string]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev claudeUsageLine
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type != "assistant" || ev.Message == nil || ev.Message.Usage == nil {
			continue
		}
		if id := ev.Message.ID; id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		// "<synthetic>" marks an SDK-internal placeholder the model never
		// produced (queued-input echoes and the like). The event parser
		// blanks its model; here it's dropped outright — a message nobody
		// generated has no cost, and keeping it would open an
		// unattributed model bucket in every report.
		if ev.Message.Model == "<synthetic>" {
			continue
		}
		u := ev.Message.Usage
		out.Add(ev.Message.Model, ModelUsage{
			Input:      u.InputTokens,
			CacheWrite: u.CacheCreationInputTokens,
			CacheRead:  u.CacheReadInputTokens,
			Output:     u.OutputTokens,
			Steps:      1,
		})
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("transcript: scan claude jsonl for usage: %w", err)
	}
	return out, nil
}
