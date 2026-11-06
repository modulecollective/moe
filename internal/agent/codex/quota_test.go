package codex

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

// codexRateLimitsFixture follows the app-server protocol's documented
// account/rateLimits/read result, with an unrelated second bucket that
// must not be read.
const codexRateLimitsFixture = `{
 "rateLimits":{"limitId":"codex","limitName":null,
   "primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1794412152},
   "secondary":{"usedPercent":96,"windowDurationMins":10080,"resetsAt":1794900000},
   "rateLimitReachedType":null},
 "rateLimitsByLimitId":{"codex_other":{"limitId":"codex_other","primary":{"usedPercent":100,"resetsAt":1}}},
 "ordinaryUsageAllowed":null}`

func TestParseCodexRateLimitsReadsDefaultBucket(t *testing.T) {
	q, err := parseCodexRateLimits(json.RawMessage(codexRateLimitsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if q.Denied || len(q.Windows) != 2 {
		t.Fatalf("quota = %+v", q)
	}
	if w := q.Windows[1]; w.ID != "codex.secondary" || w.UsedPercent != 96 || !w.ResetsAt.Equal(time.Unix(1794900000, 0)) {
		t.Fatalf("secondary = %+v", w)
	}
}

func TestParseCodexRateLimitsDenials(t *testing.T) {
	for name, raw := range map[string]string{
		"not allowed": `{"ordinaryUsageAllowed":false,"rateLimits":{"primary":{"usedPercent":3}}}`,
		"reached":     `{"rateLimits":{"rateLimitReachedType":"rate_limit_reached","primary":{"usedPercent":3}}}`,
	} {
		q, err := parseCodexRateLimits(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !q.Denied {
			t.Fatalf("%s: quota = %+v, want denial", name, q)
		}
	}
}

func TestParseCodexRateLimitsRefusesMissingData(t *testing.T) {
	for name, raw := range map[string]string{
		"no snapshot":    `{}`,
		"no windows":     `{"rateLimits":{"primary":null,"secondary":null}}`,
		"no percent":     `{"rateLimits":{"primary":{"resetsAt":1}}}`,
		"wrong type":     `{"rateLimits":{"primary":{"usedPercent":"9"}}}`,
		"not an object":  `[]`,
		"truncated json": `{"rateLimits":`,
	} {
		if _, err := parseCodexRateLimits(json.RawMessage(raw)); !errors.Is(err, agent.ErrQuotaMalformed) {
			t.Fatalf("%s: err = %v, want malformed", name, err)
		}
	}
}

func fakeCodexAppServer(t *testing.T, readReply string) (argv, stdin string) {
	t.Helper()
	dir := t.TempDir()
	argv, stdin = filepath.Join(dir, "argv"), filepath.Join(dir, "stdin")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + argv + `
read l; printf '%s\n' "$l" >> ` + stdin + `
echo '{"id":1,"result":{"userAgent":"fake"}}'
read l; printf '%s\n' "$l" >> ` + stdin + `
read l; printf '%s\n' "$l" >> ` + stdin + `
echo '{"method":"account/rateLimits/updated","params":{}}'
echo '` + readReply + `'
cat >> ` + stdin + `
`
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv, stdin
}

func TestReadQuotaSpeaksAppServerProtocol(t *testing.T) {
	argvPath, stdinPath := fakeCodexAppServer(t, `{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":40,"resetsAt":1794412152}}}}`)
	q, err := Agent{}.ReadQuota(context.Background(), agent.QuotaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 1 || q.Windows[0].UsedPercent != 40 {
		t.Fatalf("quota = %+v", q)
	}
	argv, _ := os.ReadFile(argvPath)
	if string(argv) != "app-server\n--listen\nstdio://\n" {
		t.Fatalf("argv = %q", argv)
	}
	in, _ := os.ReadFile(stdinPath)
	lines := strings.Split(strings.TrimSpace(string(in)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"initialize"`) || !strings.Contains(lines[1], `"initialized"`) || !strings.Contains(lines[2], `"account/rateLimits/read"`) {
		t.Fatalf("stdin = %q", in)
	}
	if strings.Contains(string(in), "thread/start") || strings.Contains(string(in), "turn/start") {
		t.Fatal("quota read started a thread")
	}
}

func TestReadQuotaServerErrorIsUnsupported(t *testing.T) {
	fakeCodexAppServer(t, `{"id":2,"error":{"code":-32601,"message":"method not found"}}`)
	if _, err := (Agent{}).ReadQuota(context.Background(), agent.QuotaRequest{}); !errors.Is(err, agent.ErrQuotaUnsupported) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}
