package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// quotaArgs starts claude as a bare control peer: print mode over
// stream-json, waiting for an initialize request before doing anything,
// with every startup surface that could run code or cost tokens turned
// off — no tools, no settings sources (so no hooks), an empty strict MCP
// config, no session written to disk. No user message is ever sent, so
// no inference runs.
func quotaArgs() []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--safe-mode",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--tools", "",
		"--permission-mode", "dontAsk",
		"--await-initialize",
	}
}

// ReadQuota asks a short-lived claude for its subscription usage via the
// `get_usage` control request. The interface is explicitly experimental
// upstream (the SDK names it usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET);
// a reply this parser doesn't recognise is reported as malformed rather
// than guessed at. skip_behaviors keeps the CLI from scanning local
// transcripts to build its behaviour report.
func (Agent) ReadQuota(ctx context.Context, r agent.QuotaRequest) (agent.Quota, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return agent.Quota{}, fmt.Errorf("%w: claude not found on PATH", agent.ErrQuotaUnsupported)
	}
	p, err := agent.StartJSONLines(ctx, bin, quotaArgs(), filteredEnv(r.ExtraEnv), os.TempDir())
	if err != nil {
		return agent.Quota{}, err
	}
	defer p.Close()
	if _, err := claudeControl(p, "moe-init", map[string]any{"subtype": "initialize"}); err != nil {
		return agent.Quota{}, err
	}
	raw, err := claudeControl(p, "moe-usage", map[string]any{"subtype": "get_usage", "skip_behaviors": true})
	if err != nil {
		return agent.Quota{}, err
	}
	return parseClaudeUsage(raw, r.Model)
}

// claudeControl sends one control request and returns the response body
// of the control_response carrying the same request id. Every other line
// (system init, keep-alives) is skipped.
func claudeControl(p *agent.JSONLines, id string, request map[string]any) (json.RawMessage, error) {
	if err := p.Send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	for {
		line, err := p.Next()
		if err != nil {
			return nil, fmt.Errorf("claude %s: %w", request["subtype"], err)
		}
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != id {
			continue
		}
		if msg.Response.Subtype != "success" {
			// An older CLI that doesn't know the subtype answers with an
			// error here; there is nothing to retry.
			return nil, fmt.Errorf("%w: claude %s: %s", agent.ErrQuotaUnsupported, request["subtype"], msg.Response.Error)
		}
		return msg.Response.Response, nil
	}
}

// claudeAccountWindows are the account-wide buckets every claude turn
// draws on. claudeModelWindows are the model-family weekly buckets, keyed
// by a substring of the model id they meter. Both are structural JSON
// keys; the human display names elsewhere in the reply are never read.
// An unset model (the CLI's default) can't be matched to a family, so
// only the account-wide windows apply to it.
var (
	claudeAccountWindows = []string{"five_hour", "seven_day"}
	claudeModelWindows   = map[string]string{"seven_day_opus": "opus", "seven_day_sonnet": "sonnet"}
)

type claudeWindow struct {
	Utilization  *float64 `json:"utilization"`
	ResetsAt     *string  `json:"resets_at"`
	LockedReason *string  `json:"locked_reason"`
}

// parseClaudeUsage turns a get_usage reply into the windows that apply
// to model. rate_limits_available=false is the API-key / unsupported
// plan answer; it is not a zero reading.
func parseClaudeUsage(raw json.RawMessage, model string) (agent.Quota, error) {
	var reply struct {
		Available  *bool                    `json:"rate_limits_available"`
		RateLimits map[string]*claudeWindow `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return agent.Quota{}, fmt.Errorf("%w: claude get_usage: %v", agent.ErrQuotaMalformed, err)
	}
	if reply.Available == nil {
		return agent.Quota{}, fmt.Errorf("%w: claude get_usage: no rate_limits_available field", agent.ErrQuotaMalformed)
	}
	if !*reply.Available {
		return agent.Quota{}, fmt.Errorf("%w: claude reports no subscription rate limits for this login", agent.ErrQuotaUnsupported)
	}
	ids := append([]string(nil), claudeAccountWindows...)
	for id, family := range claudeModelWindows {
		if strings.Contains(strings.ToLower(model), family) {
			ids = append(ids, id)
		}
	}
	var q agent.Quota
	for _, id := range ids {
		w := reply.RateLimits[id]
		if w == nil {
			// Absent or null: this plan doesn't meter that bucket. The
			// five-hour window is the one every subscription carries; a
			// reply without it doesn't establish anything.
			if id == "five_hour" {
				return agent.Quota{}, fmt.Errorf("%w: claude get_usage: no five_hour window", agent.ErrQuotaMalformed)
			}
			continue
		}
		if w.Utilization == nil {
			return agent.Quota{}, fmt.Errorf("%w: claude get_usage: %s has no utilization", agent.ErrQuotaMalformed, id)
		}
		qw := agent.QuotaWindow{ID: id, UsedPercent: *w.Utilization}
		if w.ResetsAt != nil && *w.ResetsAt != "" {
			t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt)
			if err != nil {
				return agent.Quota{}, fmt.Errorf("%w: claude get_usage: %s resets_at: %v", agent.ErrQuotaMalformed, id, err)
			}
			qw.ResetsAt = t
		}
		if w.LockedReason != nil && *w.LockedReason != "" {
			q.Denied = true
			q.Reason = id + " locked: " + *w.LockedReason
		}
		q.Windows = append(q.Windows, qw)
	}
	return q, nil
}
