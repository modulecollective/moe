package transcript

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestParseCodexOversizedContinuation(t *testing.T) {
	padding := strings.Repeat("x", 9*1024*1024)
	bookkeeping := `{"type":"event_msg","payload":{"type":"item_completed","padding":"` + padding + `"}}`
	message := `{"timestamp":"2026-11-18T12:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible"}]}}`
	input := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"model-a"}}`, message,
		bookkeeping, bookkeeping,
		`{"type":"turn_context","payload":{"model":"model-b"}}`, message,
		`{"timestamp":"2026-11-18T12:00:01Z","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}`,
	}, "\n")
	events, err := Parse("codex", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	want := []Event{
		{Kind: KindAssistantText, Text: "visible", Model: "model-a", Time: ts},
		{Kind: KindAssistantText, Text: "visible", Model: "model-b", Time: ts},
		{Kind: KindSystem, Text: "codex turn aborted: interrupted", Model: "model-b", Time: ts.Add(time.Second)},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

func TestParseCodexOversizedRetention(t *testing.T) {
	padding := strings.Repeat("x", 9*1024*1024)
	input := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"large-context","padding":"` + padding + `"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"large-call","output":"Process exited with code 2\nOutput:\n` + padding + `"}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted","reason":"sentinel"}}`,
		// A visible oversized final record must also survive without a newline.
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + padding + `"}]}}`,
	}, "\n")
	events, err := Parse("codex", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	result := events[0]
	if result.Kind != KindToolResult || result.Output != padding || result.CallID != "large-call" || !result.Error {
		t.Fatalf("tool result: kind=%v output bytes=%d call=%q error=%v", result.Kind, len(result.Output), result.CallID, result.Error)
	}
	if events[1].Kind != KindSystem || events[1].Text != "codex turn aborted: sentinel" {
		t.Fatalf("sentinel = %+v", events[1])
	}
	if events[2].Kind != KindAssistantText || events[2].Text != padding {
		t.Fatalf("final event: kind=%v text bytes=%d", events[2].Kind, len(events[2].Text))
	}
	for i, ev := range events {
		if ev.Model != "large-context" {
			t.Errorf("event %d model = %q", i, ev.Model)
		}
	}
}

func TestParseCodexLineRecovery(t *testing.T) {
	visible := `{"type":"event_msg","payload":{"type":"turn_aborted"}}`
	for _, tail := range []string{"", "\n", "\r\n", "{broken", "   ", "\n\n"} {
		t.Run("tail="+tail, func(t *testing.T) {
			input := "\nnot json\n" + " " + visible + "\n{broken\n" +
				`{"type":"event_msg","payload":[]}` + "\n" +
				`{"type":"event_msg","payload":{"type":123}}` + "\n" +
				`{"type":"turn_context","payload":{"model":"old"}}` + "\n" +
				`{"type":"turn_context","payload":{"model":""}}` + "\r\n" +
				visible + "\r\n" + tail
			events, err := Parse("codex", strings.NewReader(input))
			want := []Event{{Kind: KindSystem, Text: "codex turn aborted"}}
			if err != nil || !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%+v error=%v", events, err)
			}
		})
	}
	for _, input := range []string{"", "\n", "{broken", "   "} {
		if events, err := Parse("codex", strings.NewReader(input)); err != nil || len(events) != 0 {
			t.Fatalf("empty/malformed input: %d events, error=%v", len(events), err)
		}
	}
	input := `{"broken":"` + strings.Repeat("x", 9*1024*1024) + "\n" + visible
	if events, err := Parse("codex", strings.NewReader(input)); err != nil || len(events) != 1 || events[0].Text != "codex turn aborted" {
		t.Fatalf("oversized malformed recovery: %d events, error=%v", len(events), err)
	}
}

// Return the terminal bytes and error together, rather than relying on a
// reader that reports its error only on a subsequent call.
type codexTerminalReader struct {
	data string
	err  error
}

func (r *codexTerminalReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestParseCodexReaderBoundary(t *testing.T) {
	visible := `{"type":"event_msg","payload":{"type":"turn_aborted"}}`
	readErr := errors.New("interrupted read")
	for _, terminalErr := range []error{io.EOF, readErr} {
		for _, tail := range []string{visible, visible + "\n", "{broken", "{broken\n", " ", "\n"} {
			for _, shortChunks := range []bool{false, true} {
				t.Run(fmt.Sprintf("error=%v/tail=%q/short=%v", terminalErr, tail, shortChunks), func(t *testing.T) {
					var r io.Reader = &codexTerminalReader{data: visible + "\n" + tail, err: terminalErr}
					if shortChunks {
						r = iotest.OneByteReader(r)
					}
					events, err := Parse("codex", r)
					if terminalErr == io.EOF && err != nil || terminalErr != io.EOF && !errors.Is(err, readErr) {
						t.Fatalf("error=%v, terminal=%v", err, terminalErr)
					}
					wantCount := 1
					if strings.HasPrefix(tail, visible) {
						wantCount++
					}
					if len(events) != wantCount {
						t.Fatalf("got %d events, want %d", len(events), wantCount)
					}
					for _, ev := range events {
						if ev.Kind != KindSystem || ev.Text != "codex turn aborted" {
							t.Fatalf("event=%+v", ev)
						}
					}
				})
			}
		}
	}
}

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
	// Every event carries the turn_context model — codex stamps it once
	// per turn and it applies to the whole turn.
	want := []Event{
		{Kind: KindUserText, Text: "start the session", Model: "gpt-5-codex"},
		{Kind: KindAssistantText, Text: "On it.", Model: "gpt-5-codex"},
		{Kind: KindToolCall, Tool: "exec_command", Args: "sed -n '1,10p' canvas.md", CallID: "call_1", Model: "gpt-5-codex"},
		{Kind: KindToolResult, CallID: "call_1", Output: "sed: can't read canvas.md: No such file or directory\n", Error: true, Model: "gpt-5-codex"},
		{Kind: KindToolCall, Tool: "apply_patch", Args: "internal/foo.go", CallID: "call_2", Model: "gpt-5-codex"},
		{Kind: KindToolResult, CallID: "call_2", Output: "Success. Updated the following files:\nM internal/foo.go\n", Error: false, Model: "gpt-5-codex"},
		{Kind: KindSystem, Text: "codex turn aborted: interrupted", Model: "gpt-5-codex"},
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
			got.Error != w.Error || got.Model != w.Model {
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

func TestParseCodexTaskCompleteError(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{
			// Verbatim shape from the 2026-09-29 update-sol incident.
			"json-envelope",
			`{"timestamp":"2026-09-29T11:23:52.000Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":null,"error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.\"}}","codex_error_info":"other"}}}`,
			[]string{"codex turn failed (400 invalid_request_error): The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account."},
		},
		{
			"plain-text",
			`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null,"error":{"message":"This content was flagged for possible cybersecurity risk.","codex_error_info":"other"}}}`,
			[]string{"codex turn failed: This content was flagged for possible cybersecurity risk."},
		},
		{
			// Every successful turn ends like this; it must stay silent.
			"no-error",
			`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done","error":null}}`,
			nil,
		},
		{
			"no-error-field",
			`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}`,
			nil,
		},
	}
	for _, c := range cases {
		events, err := parseCodex(strings.NewReader(c.line + "\n"))
		if err != nil {
			t.Fatalf("%s: parse: %v", c.name, err)
		}
		var got []string
		for _, e := range events {
			if e.Kind != KindSystem {
				t.Errorf("%s: event kind = %v, want KindSystem", c.name, e.Kind)
			}
			got = append(got, e.Text)
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") || len(got) != len(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
