package transcript

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRender_SmokeAllKinds(t *testing.T) {
	ts := time.Date(2026, 5, 16, 21, 17, 30, 0, time.UTC)
	events := []Event{
		{Kind: KindUserText, Time: ts, Text: "do the thing"},
		{Kind: KindAssistantText, Time: ts, Text: "I am doing the thing.\nIt has two lines."},
		{Kind: KindToolCall, Time: ts, Tool: "Bash", Args: "echo hi", CallID: "c1"},
		{Kind: KindToolResult, Time: ts, CallID: "c1", Output: "hi\n"},
		{Kind: KindToolCall, Time: ts, Tool: "Bash", Args: "false", CallID: "c2"},
		{Kind: KindToolResult, Time: ts, CallID: "c2", Output: "", Error: true},
		{Kind: KindSystem, Time: ts, Text: "sandbox denied"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()

	// Spot-check that each event surfaced.
	for _, want := range []string{
		"user",
		"do the thing",
		"assistant",
		"I am doing the thing.",
		"It has two lines.",
		"Bash(echo hi)",
		"hi",
		"Bash(false)",
		"✗",
		"system",
		"sandbox denied",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output missing %q\n---output---\n%s", want, got)
		}
	}
}

func TestRender_ElidesLongToolOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line "+string(rune('a'+(i%26))))
	}
	output := strings.Join(lines, "\n")
	events := []Event{
		{Kind: KindToolCall, Tool: "Bash", Args: "find .", CallID: "c1"},
		{Kind: KindToolResult, CallID: "c1", Output: output},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "[84 lines elided]") {
		t.Errorf("expected elision marker for 100-line output (head=8, tail=8 → 84 elided); got:\n%s", got)
	}
}

func TestRender_NoElisionWhenDisabled(t *testing.T) {
	output := strings.Repeat("x\n", 100)
	events := []Event{
		{Kind: KindToolResult, CallID: "c1", Output: output},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{MaxOutputLines: -1}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "elided") {
		t.Errorf("expected no elision with MaxOutputLines=-1")
	}
}

func TestRender_DropsTimestampWhenZero(t *testing.T) {
	events := []Event{
		{Kind: KindAssistantText, Text: "hi"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	// Should start with the kind label, not a time.
	if !strings.HasPrefix(got, "assistant\n") {
		t.Errorf("expected output to start with bare 'assistant' label, got: %q", got)
	}
}

func TestRender_ModelMarkerOnceAtTop(t *testing.T) {
	events := []Event{
		{Kind: KindAssistantText, Text: "one", Model: "claude-opus-4-8"},
		{Kind: KindToolCall, Tool: "Read", Args: "a.go", CallID: "c1", Model: "claude-opus-4-8"},
		{Kind: KindToolResult, CallID: "c1", Output: "ok\n"},
		{Kind: KindAssistantText, Text: "two", Model: "claude-opus-4-8"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "model: claude-opus-4-8\n\n") {
		t.Errorf("expected output to open with the model marker, got:\n%s", got)
	}
	if n := strings.Count(got, "model: claude-opus-4-8"); n != 1 {
		t.Errorf("single-model transcript should announce once, got %d markers:\n%s", n, got)
	}
}

func TestRender_ModelMarkerOnSwitch(t *testing.T) {
	events := []Event{
		{Kind: KindAssistantText, Text: "before", Model: "claude-opus-4-8"},
		{Kind: KindAssistantText, Text: "after", Model: "claude-sonnet-5"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	want := "model: claude-opus-4-8\n\nassistant\n  before\n\nmodel: claude-sonnet-5\n\nassistant\n  after\n"
	if got != want {
		t.Errorf("mid-stream switch mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_EmptyModelEventsDontResetTracker(t *testing.T) {
	// A user seed (empty model) opens the transcript, then the first
	// modelled event announces; a later empty-model system event must
	// not cause the same model to re-announce.
	events := []Event{
		{Kind: KindUserText, Text: "seed"},
		{Kind: KindAssistantText, Text: "reply", Model: "claude-opus-4-8"},
		{Kind: KindSystem, Text: "sandbox note"},
		{Kind: KindAssistantText, Text: "more", Model: "claude-opus-4-8"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if n := strings.Count(got, "model: claude-opus-4-8"); n != 1 {
		t.Errorf("expected a single announce despite intervening empty-model events, got %d:\n%s", n, got)
	}
	// Per the behaviour rules the marker fires at the first modelled
	// event, so it lands after the empty-model user seed, not above it.
	if !strings.HasPrefix(got, "user\n  seed\n\nmodel: claude-opus-4-8\n") {
		t.Errorf("marker should follow the empty-model seed, got:\n%s", got)
	}
}

func TestRender_NoModelUnchanged(t *testing.T) {
	// Older JSONL predating the model fields: no event carries a model,
	// so the output must contain no marker at all.
	events := []Event{
		{Kind: KindUserText, Text: "hi"},
		{Kind: KindAssistantText, Text: "there"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "model:") {
		t.Errorf("no-model transcript should render without a marker, got:\n%s", buf.String())
	}
}

func TestRender_MarkerNeverSplitsCallResult(t *testing.T) {
	// A tool result folds under its call and never bears a header, so
	// even if a result somehow carried a model it must not emit a
	// marker between the call and its own output.
	events := []Event{
		{Kind: KindToolCall, Tool: "Read", Args: "a.go", CallID: "c1", Model: "claude-opus-4-8"},
		{Kind: KindToolResult, CallID: "c1", Output: "contents\n", Model: "claude-sonnet-5"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, events, RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	want := "model: claude-opus-4-8\n\ntool: Read(a.go)\n    contents\n"
	if got != want {
		t.Errorf("marker must not split a call/result pair\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestTail(t *testing.T) {
	ev := []Event{
		{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"},
	}
	if got := Tail(ev, 2); len(got) != 2 || got[0].Text != "d" || got[1].Text != "e" {
		t.Errorf("Tail(5, 2) = %v, want [d e]", got)
	}
	if got := Tail(ev, 10); len(got) != 5 {
		t.Errorf("Tail(5, 10) = %d, want 5", len(got))
	}
	if got := Tail(ev, 0); len(got) != 5 {
		t.Errorf("Tail(5, 0) = %d, want 5", len(got))
	}
}
