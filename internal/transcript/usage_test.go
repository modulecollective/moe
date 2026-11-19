package transcript

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
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

func TestParseCodexUsageOversizedRecords(t *testing.T) {
	large := `{"type":"event_msg","payload":{"type":"item_completed","padding":"` + strings.Repeat("x", 8*1024*1024+1) + `"}}` + "\n"
	fixture := codexUsageFixture + large + large + `{"type":"turn_context","payload":{"model":"gpt-6.1-sol"}}
{"type":"turn_context","payload":{"model":""}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":17000,"cached_input_tokens":15000,"output_tokens":900}}}}
`
	u, err := ParseUsage("codex", strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	want := ModelUsage{Input: 2000, CacheRead: 15000, Output: 900, Steps: 3}
	if len(u) != 1 || u["gpt-6.1-sol"] != want {
		t.Fatalf("usage = %+v, want final model gpt-6.1-sol with %+v", u, want)
	}
}

func TestParseCodexUsageOversizedAccountingRecords(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(codexUsageFixture, "\n"), "\n")
	for _, tc := range []struct {
		name string
		line int
	}{
		{"model", 0},
		{"tokens", len(lines) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			padded := append([]string(nil), lines...)
			padded[tc.line] = strings.TrimSuffix(padded[tc.line], "}") + `,"padding":"` + strings.Repeat("x", 8*1024*1024+1) + `"}`
			// No trailing newline: the oversized accounting record must
			// also be processed when ReadBytes returns its bytes with EOF.
			u, err := ParseUsage("codex", strings.NewReader(strings.Join(padded, "\n")))
			if err != nil {
				t.Fatalf("ParseUsage: %v", err)
			}
			want := ModelUsage{Input: 1000, CacheRead: 8000, Output: 400, Steps: 2}
			if len(u) != 1 || u["gpt-5.5"] != want {
				t.Fatalf("usage = %+v, want gpt-5.5 with %+v", u, want)
			}
		})
	}
}

func TestParseCodexUsageRecoveryAndEOF(t *testing.T) {
	want := ModelUsage{Input: 1000, CacheRead: 8000, Output: 400, Steps: 2}
	for _, tc := range []struct {
		name    string
		fixture string
		empty   bool
	}{
		{"empty", "", true},
		{"malformed before usage", "\nnot json\n{bad json}\n" + codexUsageFixture, false},
		{"final usage without newline", strings.TrimSuffix(codexUsageFixture, "\n"), false},
		{"malformed final tail", codexUsageFixture + `{"type":`, false},
		{"non JSON final tail", codexUsageFixture + "not json", false},
		{"blank final tail", codexUsageFixture + "\n", false},
		{"oversized final record without newline", codexUsageFixture + `{"padding":"` + strings.Repeat("x", 8*1024*1024+1) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseUsage("codex", strings.NewReader(tc.fixture))
			if err != nil {
				t.Fatalf("ParseUsage: %v", err)
			}
			if tc.empty {
				if len(u) != 0 {
					t.Fatalf("usage = %+v, want no entries", u)
				}
			} else if len(u) != 1 || u["gpt-5.5"] != want {
				t.Fatalf("usage = %+v, want gpt-5.5 with %+v", u, want)
			}
		})
	}
}

func TestParseCodexUsageShortReads(t *testing.T) {
	r := iotest.OneByteReader(strings.NewReader(strings.TrimSuffix(codexUsageFixture, "\n")))
	u, err := ParseUsage("codex", r)
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	want := ModelUsage{Input: 1000, CacheRead: 8000, Output: 400, Steps: 2}
	if len(u) != 1 || u["gpt-5.5"] != want {
		t.Fatalf("usage = %+v, want gpt-5.5 with %+v", u, want)
	}
}

func TestParseCodexUsageReadError(t *testing.T) {
	readErr := errors.New("read interrupted")
	for _, tail := range []string{"", "\n", "not json", "{bad json}", strings.TrimSuffix(codexUsageFixture, "\n")} {
		t.Run(tail, func(t *testing.T) {
			r := io.MultiReader(strings.NewReader(codexUsageFixture+tail), iotest.ErrReader(readErr))
			u, err := ParseUsage("codex", r)
			if !errors.Is(err, readErr) || u != nil {
				t.Fatalf("ParseUsage = (%+v, %v), want nil usage and wrapped read error", u, err)
			}
		})
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
			paid := false
			for model, m := range u {
				if model == "" {
					t.Errorf("usage from %s has an unattributed model bucket", c.path)
				}
				paid = paid || (m.Steps > 0 && m.Output > 0)
			}
			if !paid {
				t.Fatalf("usage %+v from %s: a real stage transcript is never free", u, c.path)
			}
		})
	}
}
