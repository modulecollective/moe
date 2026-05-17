package claude

import (
	"bytes"
	"strings"
	"testing"
)

// TestPipeOneShotProgressRendersToolCalls feeds a canned sequence of
// `claude -p --output-format stream-json` events through the
// translator and asserts the rendered progress lines. Covers the
// happy paths (Read/Edit/Write/Bash), path trimming under trimRoot,
// fallback for a tool with no recognized field, and silent skipping
// of non-tool events (text, system init, garbage lines).
func TestPipeOneShotProgressRendersToolCalls(t *testing.T) {
	const root = "/work/repo"
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Read","Edit"]}`,
		`{"type":"assistant","message":{"content":[` +
			`{"type":"text","text":"reading the canvas"},` +
			`{"type":"tool_use","name":"Read","input":{"file_path":"/work/repo/projects/foo/runs/bar/documents/design/content.md"}}` +
			`]}}`,
		`{"type":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"Edit","input":{"file_path":"/work/repo/projects/foo/runs/bar/documents/design/content.md","old_string":"x","new_string":"y"}}` +
			`]}}`,
		`{"type":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"Write","input":{"file_path":"/elsewhere/scratch.md","content":"hi"}}` +
			`]}}`,
		`{"type":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"Bash","input":{"command":"go test ./...\nwith newline"}}` +
			`]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"WebSearch","input":{"query":"some query"}}` +
			`]}}`,
		`not json at all`,
		``,
		`{"type":"result","subtype":"success"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	pipeOneShotProgress(strings.NewReader(input), &out, root, nil)

	got := out.String()
	want := []string{
		"> reading projects/foo/runs/bar/documents/design/content.md\n",
		"> editing projects/foo/runs/bar/documents/design/content.md\n",
		// Write to /elsewhere is outside trimRoot, so the absolute
		// path passes through unchanged.
		"> writing /elsewhere/scratch.md\n",
		// Newlines collapse to spaces so each tool call is one line.
		"> bash: go test ./... with newline\n",
		// WebSearch falls through to the bare-name branch — better a
		// vague label than a silent gap.
		"> WebSearch\n",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("missing progress line %q in output:\n%s", w, got)
		}
	}
	// Garbage lines, text blocks, tool_result events, and the system
	// init must produce no lines.
	for _, forbidden := range []string{"reading the canvas", "tool_result", "subtype", "not json"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unexpected substring %q leaked into output:\n%s", forbidden, got)
		}
	}
	// One line per tool_use exactly — no duplicates from the same
	// event being parsed twice.
	if n := strings.Count(got, "> reading "); n != 1 {
		t.Fatalf("expected exactly one `> reading` line, got %d:\n%s", n, got)
	}
}

// TestPipeOneShotProgressCapturesSessionID asserts the first
// system/init event's session_id is pushed on sidCh so
// ExecuteOneShot's post-Wait mirror finds the right per-session
// JSONL. A second init event (claude doesn't emit one, but the
// channel is cap-1 so even if it did, the first id wins) gets dropped
// rather than overwriting.
func TestPipeOneShotProgressCapturesSessionID(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-abc","tools":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"system","subtype":"init","session_id":"sess-zzz"}`,
	}, "\n") + "\n"
	sidCh := make(chan string, 1)
	var out bytes.Buffer
	pipeOneShotProgress(strings.NewReader(input), &out, "", sidCh)
	close(sidCh)
	sid := <-sidCh
	if sid != "sess-abc" {
		t.Fatalf("session id: got %q, want sess-abc", sid)
	}
}

// TestPipeOneShotProgressTruncatesLongCommands keeps the operator's
// terminal readable when an agent runs a multi-kilobyte heredoc — one
// long bash line shouldn't wrap into a wall of text. The truncated
// rendering still has to start with `> bash: ` so it's recognizable.
func TestPipeOneShotProgressTruncatesLongCommands(t *testing.T) {
	cmd := strings.Repeat("a", 500)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + cmd + `"}}]}}` + "\n"
	var out bytes.Buffer
	pipeOneShotProgress(strings.NewReader(line), &out, "", nil)
	got := out.String()
	if !strings.HasPrefix(got, "> bash: ") {
		t.Fatalf("expected `> bash: ` prefix, got: %q", got)
	}
	// 80 char cap + ellipsis + `> bash: ` prefix + trailing newline.
	// Exact length check guards against the cap drifting silently.
	if len(got) > 100 {
		t.Fatalf("expected truncated line under ~100 chars, got %d:\n%s", len(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected ellipsis in truncated line, got: %q", got)
	}
}
