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
	Register("codex", parseCodex)
}

// codexLine is the top-level envelope codex writes one-per-line into
// the rollout JSONL. The fields we care about live inside `payload`,
// which is shape-dependent on `type` — see codexPayload.
type codexLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload covers the union of fields the renderer reads across
// every payload variant. Fields not relevant to a given payload type
// are absent (or default) and ignored — the per-type switch in
// parseCodex picks the right subset.
type codexPayload struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content,omitempty"`
	// function_call / custom_tool_call
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Input     string `json:"input,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	// function_call_output / custom_tool_call_output
	Output string `json:"output,omitempty"`
	// event_msg variants we surface
	Reason string `json:"reason,omitempty"`
	// turn_context: the model codex ran the turn under. Tracked as parse
	// state and stamped onto every event until the next turn_context.
	Model string `json:"model,omitempty"`
}

// parseCodex turns a codex rollout JSONL reader into normalised
// Events. Codex's vocabulary splits across `response_item` (the
// model-visible conversation) and `event_msg` (codex-internal turn
// state); we lift only what the operator can read, and drop the
// bookkeeping (token counts, task lifecycle, reasoning, the
// developer-role system prompt). Malformed lines and unknown
// payload types are skipped — same forensic-file rationale as the
// claude adapter.
func parseCodex(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var out []Event
	// model is the current turn's model, updated by each turn_context and
	// stamped onto the events every other line produces. A file that
	// resumes under a different model carries a second turn_context, so
	// later events pick up the new value.
	var model string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var env codexLine
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		var p codexPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		ts := parseClaudeTimestamp(env.Timestamp) // RFC3339Nano — same shape
		var evs []Event
		switch env.Type {
		case "response_item":
			evs = codexResponseItem(p, ts)
		case "event_msg":
			evs = codexEventMsg(p, ts)
		case "turn_context":
			// Pure bookkeeping except for the model, which we lift into
			// parse state. session_meta stays dropped (instructions blob,
			// sandbox policy recap — nothing the operator reads).
			model = p.Model
		}
		for i := range evs {
			evs[i].Model = model
		}
		out = append(out, evs...)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("transcript: scan codex jsonl: %w", err)
	}
	return out, nil
}

func codexResponseItem(p codexPayload, ts time.Time) []Event {
	switch p.Type {
	case "message":
		// role=developer is the codex system prompt blob — dropped
		// alongside claude's analogue. Assistant and user messages
		// both flatten content[].text into the body.
		if p.Role == "developer" {
			return nil
		}
		text := flattenCodexContent(p.Content)
		if text == "" {
			return nil
		}
		kind := KindAssistantText
		if p.Role == "user" {
			kind = KindUserText
		}
		return []Event{{Kind: kind, Time: ts, Text: text}}
	case "function_call":
		return []Event{{
			Kind:   KindToolCall,
			Time:   ts,
			Tool:   p.Name,
			Args:   summariseCodexFunctionArgs(p.Name, p.Arguments),
			CallID: p.CallID,
		}}
	case "custom_tool_call":
		// custom_tool_call is the apply_patch path: `input` is the
		// raw patch text. Show the first hunk header so the
		// operator sees which file is being touched.
		return []Event{{
			Kind:   KindToolCall,
			Time:   ts,
			Tool:   p.Name,
			Args:   summariseCustomToolInput(p.Input),
			CallID: p.CallID,
		}}
	case "function_call_output":
		out, isErr := parseCodexFunctionOutput(p.Output)
		return []Event{{
			Kind:   KindToolResult,
			Time:   ts,
			CallID: p.CallID,
			Output: out,
			Error:  isErr,
		}}
	case "custom_tool_call_output":
		out, isErr := parseCodexCustomOutput(p.Output)
		return []Event{{
			Kind:   KindToolResult,
			Time:   ts,
			CallID: p.CallID,
			Output: out,
			Error:  isErr,
		}}
		// "reasoning" carries codex's encrypted thinking blob —
		// nothing renderable.
	}
	return nil
}

func codexEventMsg(p codexPayload, ts time.Time) []Event {
	// Most event_msg variants duplicate response_item content
	// (agent_message ~= response_item.message assistant,
	// user_message ~= response_item.message user) or are pure
	// bookkeeping (token_count, task_started, task_complete,
	// patch_apply_end). turn_aborted is the one the operator needs:
	// it's the proximate signal that the one-shot bailed.
	if p.Type == "turn_aborted" {
		msg := "codex turn aborted"
		if p.Reason != "" {
			msg = "codex turn aborted: " + p.Reason
		}
		return []Event{{Kind: KindSystem, Time: ts, Text: msg}}
	}
	return nil
}

func flattenCodexContent(blocks []struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text == "" {
			continue
		}
		switch blk.Type {
		case "input_text", "output_text", "text":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// summariseCodexFunctionArgs picks the most informative one-line
// summary of a function-call argument blob. arguments is a JSON
// string; the value we care about depends on which tool it is.
// `exec_command` is by far the most common; its `cmd` field is the
// shell command codex ran. Fallback returns the trimmed JSON so the
// operator at least sees that an unknown tool fired.
func summariseCodexFunctionArgs(name, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return collapseWhitespace(argsJSON)
	}
	switch name {
	case "exec_command", "shell":
		if c, ok := stringField(m, "cmd"); ok {
			return collapseWhitespace(c)
		}
		if c, ok := stringField(m, "command"); ok {
			return collapseWhitespace(c)
		}
	case "read_file":
		if p, ok := stringField(m, "path"); ok {
			return p
		}
	case "write_file":
		if p, ok := stringField(m, "path"); ok {
			return p
		}
	case "update_plan":
		return "" // pure bookkeeping
	}
	// Unknown tool: collapse the first useful string value rather
	// than dumping the whole map.
	for _, k := range []string{"cmd", "command", "path", "query", "url"} {
		if s, ok := stringField(m, k); ok {
			return collapseWhitespace(s)
		}
	}
	return ""
}

// summariseCustomToolInput pulls the first file path from an
// apply_patch blob — the patch envelope is the only `custom_tool_call`
// shape in production, and the first "*** Update File:" / "*** Add
// File:" / "*** Delete File:" header is what tells the operator which
// file moved. Falls back to the first non-empty input line.
func summariseCustomToolInput(input string) string {
	for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
		if i := strings.Index(input, prefix); i >= 0 {
			rest := input[i+len(prefix):]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				return strings.TrimSpace(rest[:nl])
			}
			return strings.TrimSpace(rest)
		}
	}
	if nl := strings.IndexByte(input, '\n'); nl >= 0 {
		return strings.TrimSpace(input[:nl])
	}
	return strings.TrimSpace(input)
}

// parseCodexFunctionOutput peels the "Process exited with code N" /
// "Output:" envelope codex wraps shell output in. Returns the inner
// output plus an error flag derived from the exit code. The envelope
// shape is stable enough across codex versions that string-matching
// is cheaper than reflecting a struct.
func parseCodexFunctionOutput(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	isErr := false
	// "Process exited with code N\n" — N != 0 marks the call as an
	// error.
	if i := strings.Index(s, "Process exited with code "); i >= 0 {
		rest := s[i+len("Process exited with code "):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			code := strings.TrimSpace(rest[:nl])
			if code != "0" {
				isErr = true
			}
		}
	}
	// Trim the envelope so the operator just sees the inner output.
	if i := strings.Index(s, "\nOutput:\n"); i >= 0 {
		return s[i+len("\nOutput:\n"):], isErr
	}
	return s, isErr
}

// parseCodexCustomOutput decodes the JSON envelope custom_tool_call
// outputs use: `{"output":"…","metadata":{"exit_code":N,…}}`. The
// inner output is plain text. exit_code != 0 marks the result as an
// error. Bad JSON falls back to the raw string with no error flag —
// better to render something than to drop a result entirely.
func parseCodexCustomOutput(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	var env struct {
		Output   string `json:"output"`
		Metadata struct {
			ExitCode int `json:"exit_code"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return s, false
	}
	return env.Output, env.Metadata.ExitCode != 0
}

func init() {
	RegisterUsage("codex", parseCodexUsage)
}

// codexUsageLine is the usage-bearing subset of a codex rollout line:
// the `token_count` event_msg, plus the `turn_context` that names the
// model.
type codexUsageLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type  string `json:"type"`
		Model string `json:"model,omitempty"`
		Info  *struct {
			TotalTokenUsage struct {
				InputTokens       int64 `json:"input_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
			} `json:"total_token_usage"`
		} `json:"info,omitempty"`
	} `json:"payload"`
}

// parseCodexUsage reads a codex rollout's token accounting.
//
// Codex reports a running `total_token_usage` on every `token_count`
// event, so the last one is the whole rollout — that is what this takes.
// The tempting alternative, summing each event's `last_token_usage`,
// overshoots the reported total by 20–25% on real rollouts: codex emits
// several token_count events per turn and the "last" delta repeats
// across them.
//
// Two consequences of reading a cumulative total. Steps is the count of
// token_count events rather than of turns, so it is not comparable with
// the claude adapter's per-turn count — the CLI reports it per backend
// and never sums the two into one "steps" column. And the whole total is
// attributed to the last model `turn_context` named, since a cumulative
// figure carries no per-model split; codex rollouts are single-model in
// practice, and a resumed one that isn't will over-attribute to the
// later model rather than lose the tokens.
//
// Codex reports cached *reads* but no cache-write bucket, so CacheWrite
// stays zero — a gap in the source, not a claim that nothing was
// written.
func parseCodexUsage(r io.Reader) (Usage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var model string
	var last ModelUsage
	steps := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev codexUsageLine
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch {
		case ev.Type == "turn_context":
			if ev.Payload.Model != "" {
				model = ev.Payload.Model
			}
		case ev.Type == "event_msg" && ev.Payload.Type == "token_count" && ev.Payload.Info != nil:
			t := ev.Payload.Info.TotalTokenUsage
			steps++
			last = ModelUsage{
				// input_tokens is the whole prompt; the cached share is
				// broken out, so the uncached remainder is the difference.
				Input:     t.InputTokens - t.CachedInputTokens,
				CacheRead: t.CachedInputTokens,
				Output:    t.OutputTokens,
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("transcript: scan codex jsonl for usage: %w", err)
	}
	out := Usage{}
	if steps > 0 {
		last.Steps = steps
		out.Add(model, last)
	}
	return out, nil
}
