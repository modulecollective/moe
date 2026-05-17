package transcript

import (
	"os"
	"testing"
)

func TestParseCodex_TestdataFixture(t *testing.T) {
	f, err := os.Open("testdata/codex.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	events, err := parseCodex(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Dropped: session_meta, task_started, token_count, reasoning,
	// the developer-role message (codex system prompt). turn_aborted
	// surfaces as KindSystem; the agent_message duplicate of the
	// assistant message isn't in the fixture (production already has
	// the response_item, so we only need to verify we don't
	// double-render — the fixture's single assistant message
	// produces a single event).
	want := []Event{
		{Kind: KindUserText, Text: "start the session"},
		{Kind: KindAssistantText, Text: "On it."},
		{Kind: KindToolCall, Tool: "exec_command", Args: "sed -n '1,10p' canvas.md", CallID: "call_1"},
		{Kind: KindToolResult, CallID: "call_1", Output: "sed: can't read canvas.md: No such file or directory\n", Error: true},
		{Kind: KindToolCall, Tool: "apply_patch", Args: "internal/foo.go", CallID: "call_2"},
		{Kind: KindToolResult, CallID: "call_2", Output: "Success. Updated the following files:\nM internal/foo.go\n", Error: false},
		{Kind: KindSystem, Text: "codex turn aborted: interrupted"},
	}
	if len(events) != len(want) {
		for i, e := range events {
			t.Logf("[%d] kind=%v text=%q tool=%q args=%q output=%q err=%v", i, e.Kind, e.Text, e.Tool, e.Args, e.Output, e.Error)
		}
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, w := range want {
		got := events[i]
		if got.Kind != w.Kind || got.Text != w.Text || got.Tool != w.Tool ||
			got.Args != w.Args || got.CallID != w.CallID || got.Output != w.Output ||
			got.Error != w.Error {
			t.Errorf("event[%d] = %+v, want %+v", i, got, w)
		}
	}
}

func TestParseCodexFunctionOutput(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantOut   string
		wantError bool
	}{
		{
			"exit-zero",
			"Chunk ID: x\nProcess exited with code 0\nOutput:\nhello\n",
			"hello\n",
			false,
		},
		{
			"exit-nonzero",
			"Chunk ID: x\nProcess exited with code 2\nOutput:\nboom\n",
			"boom\n",
			true,
		},
		{
			"no-envelope",
			"just text",
			"just text",
			false,
		},
	}
	for _, c := range cases {
		out, isErr := parseCodexFunctionOutput(c.in)
		if out != c.wantOut || isErr != c.wantError {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, out, isErr, c.wantOut, c.wantError)
		}
	}
}

func TestParseCodexCustomOutput(t *testing.T) {
	out, isErr := parseCodexCustomOutput(`{"output":"ok\n","metadata":{"exit_code":0}}`)
	if out != "ok\n" || isErr {
		t.Errorf("got (%q, %v), want (\"ok\\n\", false)", out, isErr)
	}
	out, isErr = parseCodexCustomOutput(`{"output":"fail","metadata":{"exit_code":127}}`)
	if out != "fail" || !isErr {
		t.Errorf("got (%q, %v), want (\"fail\", true)", out, isErr)
	}
	out, isErr = parseCodexCustomOutput(`not json`)
	if out != "not json" || isErr {
		t.Errorf("bad-json fallback: got (%q, %v), want (\"not json\", false)", out, isErr)
	}
}

func TestSummariseCustomToolInput(t *testing.T) {
	in := "*** Begin Patch\n*** Update File: a/b/c.go\n@@\n-x\n+y\n"
	if got := summariseCustomToolInput(in); got != "a/b/c.go" {
		t.Errorf("got %q, want a/b/c.go", got)
	}
	in = "*** Add File: new.txt\nhello\n"
	if got := summariseCustomToolInput(in); got != "new.txt" {
		t.Errorf("got %q, want new.txt", got)
	}
}
