package transcript

import (
	"os"
	"testing"
	"time"
)

func TestParseClaude_TestdataFixture(t *testing.T) {
	f, err := os.Open("testdata/claude.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	events, err := parseClaude(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Expected events, in order, after dropping bookkeeping
	// (permission-mode, file-history-snapshot, queue-operation) and
	// the assistant `thinking` block.
	want := []Event{
		{Kind: KindUserText, Text: "please look at the canvas"},
		{Kind: KindToolCall, Tool: "Read", Args: "/abs/path/canvas.md", CallID: "tu-1"},
		{Kind: KindToolResult, CallID: "tu-1", Output: "line1\nline2\nline3"},
		{Kind: KindToolCall, Tool: "Bash", Args: "npm test", CallID: "tu-2"},
		{Kind: KindToolResult, CallID: "tu-2", Output: "exit code 1\noops", Error: true},
		{Kind: KindAssistantText, Text: "All set."},
		{Kind: KindSystem, Text: "/remote-control is active"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
		for i, e := range events {
			t.Logf("[%d] kind=%v text=%q tool=%q", i, e.Kind, e.Text, e.Tool)
		}
	}
	for i, w := range want {
		got := events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool ||
			got.Args != w.Args || got.CallID != w.CallID || got.Output != w.Output ||
			got.Error != w.Error {
			t.Errorf("event[%d] = %+v, want %+v", i, got, w)
		}
		if got.Time.IsZero() {
			t.Errorf("event[%d] missing timestamp", i)
		}
	}

	// Spot-check the first event's timestamp parsed correctly.
	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-05-16T21:17:27.689Z")
	if !events[0].Time.Equal(wantTime) {
		t.Errorf("event[0].Time = %v, want %v", events[0].Time, wantTime)
	}
}

func TestSummariseClaudeInput(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"Read", map[string]any{"file_path": "/x/y"}, "/x/y"},
		{"Bash", map[string]any{"command": "echo  hi\nthere"}, "echo hi there"},
		{"Grep", map[string]any{"pattern": "foo.*bar"}, "foo.*bar"},
		{"WebSearch", map[string]any{"query": "anthropic api"}, "anthropic api"},
		{"Unknown", map[string]any{"file_path": "/x"}, ""},
	}
	for _, c := range cases {
		got := summariseClaudeInput(c.name, c.in)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecodeClaudeResultContent_StringAndBlocks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello"`, "hello"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"empty", ``, ""},
		{"garbage", `{"weird":true}`, ""},
	}
	for _, c := range cases {
		got := decodeClaudeResultContent([]byte(c.raw))
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
