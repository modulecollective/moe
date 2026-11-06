package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// claudeUsageFixture is a sanitized get_usage reply in the shape claude
// 2.1.316 returned during design: windows as 0–100 utilisation with ISO
// resets, unrelated product buckets null, a model-scoped list keyed by
// display names that must never select a window.
const claudeUsageFixture = `{"rate_limits_available":true,"rate_limits":{
 "five_hour":{"utilization":65,"resets_at":"2026-11-06T06:10:00.314102+00:00","locked_reason":null},
 "seven_day":{"utilization":8,"resets_at":"2026-11-09T08:00:00.314124+00:00","locked_reason":null},
 "seven_day_opus":{"utilization":97,"resets_at":"2026-11-09T08:00:00+00:00","locked_reason":null},
 "seven_day_sonnet":null,"seven_day_oauth_apps":null,"tangelo":null,"model_scoped":[]},
 "behaviors":null,"session":{"total_cost_usd":0}}`

func TestParseClaudeUsageAppliesAccountAndMatchingModelWindows(t *testing.T) {
	q, err := parseClaudeUsage(json.RawMessage(claudeUsageFixture), "sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 2 || q.Windows[0].ID != "five_hour" || q.Windows[1].ID != "seven_day" {
		t.Fatalf("sonnet model windows = %+v, want five_hour + seven_day only", q.Windows)
	}
	if q.Windows[0].UsedPercent != 65 || !q.Windows[0].ResetsAt.Equal(time.Date(2026, 11, 6, 6, 10, 0, 314102000, time.UTC)) {
		t.Fatalf("five_hour = %+v", q.Windows[0])
	}
	q, err = parseClaudeUsage(json.RawMessage(claudeUsageFixture), "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 3 || q.Windows[2].ID != "seven_day_opus" || q.Windows[2].UsedPercent != 97 {
		t.Fatalf("opus windows = %+v, want the opus bucket added", q.Windows)
	}
	q, err = parseClaudeUsage(json.RawMessage(claudeUsageFixture), "claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 2 {
		t.Fatalf("fable windows = %+v: a display-name match must not apply model_scoped", q.Windows)
	}
}

func TestParseClaudeUsageRefusesUnestablishedReadings(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want error
	}{
		"api key login":    {`{"rate_limits_available":false,"rate_limits":null}`, agent.ErrQuotaUnsupported},
		"no availability":  {`{"rate_limits":{"five_hour":{"utilization":1}}}`, agent.ErrQuotaMalformed},
		"no five hour":     {`{"rate_limits_available":true,"rate_limits":{"seven_day":{"utilization":1}}}`, agent.ErrQuotaMalformed},
		"no utilization":   {`{"rate_limits_available":true,"rate_limits":{"five_hour":{"resets_at":null}}}`, agent.ErrQuotaMalformed},
		"bad reset":        {`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":1,"resets_at":"soon"}}}`, agent.ErrQuotaMalformed},
		"not json":         {`[`, agent.ErrQuotaMalformed},
		"string percent":   {`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":"1"}}}`, agent.ErrQuotaMalformed},
		"windows not maps": {`{"rate_limits_available":true,"rate_limits":[]}`, agent.ErrQuotaMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClaudeUsage(json.RawMessage(tc.raw), ""); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseClaudeUsageLockedWindowIsDenial(t *testing.T) {
	q, err := parseClaudeUsage(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":3,"locked_reason":"limit_reached"},"model_scoped":[]}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !q.Denied || !strings.Contains(q.Reason, "limit_reached") {
		t.Fatalf("quota = %+v, want an explicit denial", q)
	}
}

// fakeClaudeQuota puts a claude on PATH that speaks just enough of the
// control protocol: records its argv and every stdin line, answers the
// initialize and get_usage requests by id.
func fakeClaudeQuota(t *testing.T, usageReply string) (argv, stdin string) {
	t.Helper()
	dir := t.TempDir()
	argv, stdin = filepath.Join(dir, "argv"), filepath.Join(dir, "stdin")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + argv + `
read l; printf '%s\n' "$l" >> ` + stdin + `
echo '{"type":"system","subtype":"init"}'
echo '{"type":"control_response","response":{"subtype":"success","request_id":"moe-init","response":{}}}'
read l; printf '%s\n' "$l" >> ` + stdin + `
echo '` + usageReply + `'
cat >> ` + stdin + `
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv, stdin
}

func TestReadQuotaSpeaksControlProtocolWithoutAUserTurn(t *testing.T) {
	reply := `{"type":"control_response","response":{"subtype":"success","request_id":"moe-usage","response":{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":96,"resets_at":"2026-11-06T06:10:00Z"},"model_scoped":[]}}}}`
	argvPath, stdinPath := fakeClaudeQuota(t, reply)
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-reach-probe")
	q, err := Agent{}.ReadQuota(context.Background(), agent.QuotaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 1 || q.Windows[0].UsedPercent != 96 {
		t.Fatalf("quota = %+v", q)
	}
	argv, _ := os.ReadFile(argvPath)
	for _, flag := range []string{"--await-initialize", "--no-session-persistence", "--strict-mcp-config", "--safe-mode"} {
		if !strings.Contains(string(argv), flag+"\n") {
			t.Errorf("argv missing %s:\n%s", flag, argv)
		}
	}
	in, _ := os.ReadFile(stdinPath)
	if strings.Contains(string(in), `"type":"user"`) || strings.Count(string(in), "control_request") != 2 {
		t.Fatalf("stdin = %s, want exactly the two control requests", in)
	}
	if !strings.Contains(string(in), `"skip_behaviors":true`) {
		t.Fatalf("get_usage did not skip behaviours: %s", in)
	}
}

func TestReadQuotaErrorReplyIsUnsupported(t *testing.T) {
	fakeClaudeQuota(t, `{"type":"control_response","response":{"subtype":"error","request_id":"moe-usage","error":"Unknown subtype"}}`)
	if _, err := (Agent{}).ReadQuota(context.Background(), agent.QuotaRequest{}); !errors.Is(err, agent.ErrQuotaUnsupported) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestReadQuotaDeadlineKillsSilentChild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Agent{}.ReadQuota(ctx, agent.QuotaRequest{})
	if err == nil || agent.QuotaPermanent(err) {
		t.Fatalf("err = %v, want a transient failure", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("read outlived its deadline by %v", time.Since(start))
	}
}

func TestParseClaudeUsageRefusesCachedAndInvalidReadings(t *testing.T) {
	for name, raw := range map[string]string{
		"cached low reading": `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":1}}}`,
	} {
		if _, err := parseClaudeUsage(json.RawMessage(raw), ""); !errors.Is(err, agent.ErrQuotaUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, p := range []string{"-1", "101"} {
		raw := `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":` + p + `},"model_scoped":[]}}`
		if _, err := parseClaudeUsage(json.RawMessage(raw), ""); !errors.Is(err, agent.ErrQuotaMalformed) {
			t.Errorf("percent %s: %v", p, err)
		}
	}
}

func TestParseClaudeUsagePreservesUnidentifiedScopeWithoutGuessing(t *testing.T) {
	raw := `{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":1},"model_scoped":[{"display_name":"Fable","utilization":12}]}}`
	q, err := parseClaudeUsage(json.RawMessage(raw), "claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 1 || len(q.Unresolved) != 1 || q.Unresolved[0].UsedPercent != 12 {
		t.Fatalf("quota %+v", q)
	}
	q, err = parseClaudeUsage(json.RawMessage(claudeUsageFixture), "")
	if err != nil || len(q.Unresolved) != 1 || q.Unresolved[0].ID != "seven_day_opus" {
		t.Fatalf("default quota %+v err %v", q, err)
	}
}
