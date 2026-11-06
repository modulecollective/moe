package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// ReadQuota asks a short-lived `codex app-server` for the account's rate
// limits: initialize, then `account/rateLimits/read`. The method is in
// the app-server's stable (non-experimental) protocol and reads usage
// without starting a thread, so no inference runs.
func (Agent) ReadQuota(ctx context.Context, r agent.QuotaRequest) (agent.Quota, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return agent.Quota{}, fmt.Errorf("%w: codex not found on PATH", agent.ErrQuotaUnsupported)
	}
	p, err := agent.StartJSONLines(ctx, bin, []string{"app-server", "--listen", "stdio://"}, filteredEnv(r.ExtraEnv), os.TempDir())
	if err != nil {
		return agent.Quota{}, err
	}
	defer p.Close()
	if _, err := codexCall(p, 1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "moe", "version": "0"},
	}); err != nil {
		return agent.Quota{}, err
	}
	if err := p.Send(map[string]any{"method": "initialized"}); err != nil {
		return agent.Quota{}, err
	}
	raw, err := codexCall(p, 2, "account/rateLimits/read", nil)
	if err != nil {
		return agent.Quota{}, err
	}
	return parseCodexRateLimits(raw)
}

// codexCall sends one request and returns the result of the response
// with the same id, skipping notifications and anything else.
func codexCall(p *agent.JSONLines, id int, method string, params any) (json.RawMessage, error) {
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := p.Send(req); err != nil {
		return nil, err
	}
	for {
		line, err := p.Next()
		if err != nil {
			return nil, fmt.Errorf("codex %s: %w", method, err)
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.ID == nil || *msg.ID != id || msg.Method != "" {
			continue
		}
		if msg.Error != nil {
			// The server answered and refused: an unknown method on an
			// older codex, or a login without ChatGPT rate limits (API
			// key). Neither improves on retry.
			return nil, fmt.Errorf("%w: codex %s: %s", agent.ErrQuotaUnsupported, method, msg.Error.Message)
		}
		return msg.Result, nil
	}
}

type codexWindow struct {
	UsedPercent *float64 `json:"usedPercent"`
	ResetsAt    *int64   `json:"resetsAt"`
}

// parseCodexRateLimits reads the `rateLimits` snapshot — the protocol's
// backward-compatible default bucket, which is the one a plain codex
// turn meters. `rateLimitsByLimitId` can carry further buckets, but the
// protocol gives no structural way to tell which of them this turn's
// model draws on, so they are not guessed into the reading.
//
// ordinaryUsageAllowed=false and a non-null rateLimitReachedType are the
// server's explicit denials; the protocol says clients must not infer
// recovery from percentages alone, which is why they outrank a low
// reading.
func parseCodexRateLimits(raw json.RawMessage) (agent.Quota, error) {
	var reply struct {
		Allowed    *bool `json:"ordinaryUsageAllowed"`
		RateLimits *struct {
			Reached   *string      `json:"rateLimitReachedType"`
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return agent.Quota{}, fmt.Errorf("%w: codex rateLimits: %v", agent.ErrQuotaMalformed, err)
	}
	if reply.RateLimits == nil {
		return agent.Quota{}, fmt.Errorf("%w: codex rateLimits: no rateLimits snapshot", agent.ErrQuotaMalformed)
	}
	var q agent.Quota
	for _, w := range []struct {
		id  string
		win *codexWindow
	}{{"codex.primary", reply.RateLimits.Primary}, {"codex.secondary", reply.RateLimits.Secondary}} {
		if w.win == nil {
			continue
		}
		if w.win.UsedPercent == nil {
			return agent.Quota{}, fmt.Errorf("%w: codex rateLimits: %s has no usedPercent", agent.ErrQuotaMalformed, w.id)
		}
		qw := agent.QuotaWindow{ID: w.id, UsedPercent: *w.win.UsedPercent}
		if w.win.ResetsAt != nil {
			qw.ResetsAt = time.Unix(*w.win.ResetsAt, 0)
		}
		q.Windows = append(q.Windows, qw)
	}
	if len(q.Windows) == 0 {
		return agent.Quota{}, fmt.Errorf("%w: codex rateLimits: no usage windows", agent.ErrQuotaMalformed)
	}
	if reply.Allowed != nil && !*reply.Allowed {
		q.Denied, q.Reason = true, "ordinary usage not allowed"
	}
	if r := reply.RateLimits.Reached; r != nil && *r != "" {
		q.Denied, q.Reason = true, *r
	}
	return q, nil
}
