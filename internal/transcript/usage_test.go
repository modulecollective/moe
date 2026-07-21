package transcript

import (
	"os"
	"strings"
	"testing"
)

// claudeUsageFixture is two assistant turns. The first is written as
// three lines — one per content block — all carrying the same message id
// and a verbatim copy of the same usage object, which is what Claude
// Code actually writes. A parser that sums lines reports the first turn
// three times.
const claudeUsageFixture = `{"type":"assistant","message":{"id":"msg_a","model":"claude-fable-5","content":[{"type":"thinking","thinking":"…"}],"usage":{"input_tokens":7,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000,"output_tokens":300}}}
{"type":"assistant","message":{"id":"msg_a","model":"claude-fable-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000,"output_tokens":300}}}
{"type":"assistant","message":{"id":"msg_a","model":"claude-fable-5","content":[{"type":"tool_use","id":"t1","name":"Read"}],"usage":{"input_tokens":7,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000,"output_tokens":300}}}
{"type":"user","message":{"role":"user","content":"go on"}}
{"type":"assistant","message":{"id":"msg_b","model":"claude-opus-4-8","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":500,"cache_read_input_tokens":40000,"output_tokens":120}}}
not json
`

// TestParseClaudeUsageDedupesByMessageID is the whole reason this parser
// isn't a three-line sum: one turn's usage is repeated once per content
// block, so counting lines inflates every figure by the mean blocks per
// turn.
func TestParseClaudeUsageDedupesByMessageID(t *testing.T) {
	u, err := ParseUsage("claude", strings.NewReader(claudeUsageFixture))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	fable, ok := u["claude-fable-5"]
	if !ok {
		t.Fatalf("no fable entry in %v", u)
	}
	want := ModelUsage{Input: 7, CacheWrite: 1000, CacheRead: 20000, Output: 300, Steps: 1}
	if fable != want {
		t.Errorf("fable usage = %+v, want %+v (one turn, not three lines)", fable, want)
	}
	opus, ok := u["claude-opus-4-8"]
	if !ok {
		t.Fatalf("no opus entry in %v", u)
	}
	if opus.Output != 120 || opus.Steps != 1 {
		t.Errorf("opus usage = %+v, want the second turn alone", opus)
	}
	if got := u.Total(); got.Steps != 2 || got.Output != 420 {
		t.Errorf("total = %+v, want 2 steps and 420 output", got)
	}
}

// TestParseClaudeUsageCountsUnkeyedTurns: a usage block with no message
// id counts once. The file is forensic and unversioned, and
// under-reporting cost is the worse way to be wrong.
func TestParseClaudeUsageCountsUnkeyedTurns(t *testing.T) {
	const fixture = `{"type":"assistant","message":{"model":"m","content":[],"usage":{"output_tokens":5}}}
{"type":"assistant","message":{"model":"m","content":[],"usage":{"output_tokens":5}}}
`
	u, err := ParseUsage("claude", strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if got := u["m"]; got.Output != 10 || got.Steps != 2 {
		t.Errorf("usage = %+v, want both unkeyed turns counted", got)
	}
}

// codexUsageFixture is a rollout with a turn_context and three
// token_count events whose running total climbs. Only the last one is
// the answer.
const codexUsageFixture = `{"type":"turn_context","payload":{"model":"gpt-5.5"}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":50}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":9000,"cached_input_tokens":8000,"output_tokens":400},"last_token_usage":{"input_tokens":8000,"cached_input_tokens":7200,"output_tokens":350}}}}
`

// TestParseCodexUsageTakesTheRunningTotal: codex reports a cumulative
// figure on every token_count, so the last event is the whole rollout.
// Summing the per-event deltas instead overshoots — codex repeats the
// same delta across several events per turn.
func TestParseCodexUsageTakesTheRunningTotal(t *testing.T) {
	u, err := ParseUsage("codex", strings.NewReader(codexUsageFixture))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	got, ok := u["gpt-5.5"]
	if !ok {
		t.Fatalf("usage not attributed to the turn_context model: %v", u)
	}
	want := ModelUsage{Input: 1000, CacheRead: 8000, Output: 400, Steps: 2}
	if got != want {
		t.Errorf("codex usage = %+v, want %+v", got, want)
	}
}

// TestParseCodexUsageEmptyRollout: a rollout with no token_count at all
// reports nothing rather than a zero-valued phantom model.
func TestParseCodexUsageEmptyRollout(t *testing.T) {
	u, err := ParseUsage("codex", strings.NewReader(`{"type":"turn_context","payload":{"model":"gpt-5.5"}}`+"\n"))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if len(u) != 0 {
		t.Errorf("usage = %v, want no entries", u)
	}
}

func TestParseUsageUnknownAgent(t *testing.T) {
	if _, err := ParseUsage("nope", strings.NewReader("")); err == nil {
		t.Fatal("want an error for an unregistered agent")
	}
}

// TestParseUsageReal runs the usage parsers over the live transcripts in
// the bureaucracy, the same safety net TestParseReal gives the event
// parsers: an upstream schema change that renames a usage field would
// otherwise show up as a silently free stage. Skips when the files
// aren't reachable.
func TestParseUsageReal(t *testing.T) {
	cases := []struct {
		agent string
		path  string
	}{
		{"claude", realPath(t, "more-context-when-one-shot-bails-2026-05-16", "design", "thread-claude.jsonl")},
		{"codex", realPath(t, "headless-codex-fails-with-permissions-2026-05-15-4", "design", "thread-codex.jsonl")},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			if c.path == "" {
				t.Skip("real transcript not reachable from clone")
			}
			f, err := os.Open(c.path)
			if err != nil {
				t.Skipf("open %s: %v", c.path, err)
			}
			defer f.Close()
			u, err := ParseUsage(c.agent, f)
			if err != nil {
				t.Fatalf("ParseUsage %s: %v", c.path, err)
			}
			total := u.Total()
			if total.Steps == 0 || total.Output == 0 {
				t.Fatalf("usage %+v from %s: a real stage transcript is never free", total, c.path)
			}
			for model := range u {
				if model == "" {
					t.Errorf("usage from %s has an unattributed model bucket", c.path)
				}
			}
		})
	}
}
