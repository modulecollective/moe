package managed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is the Anthropic API host. Callers can override with
// MOE_ANTHROPIC_API_BASE for staging endpoints or local mocks.
const DefaultBaseURL = "https://api.anthropic.com"

// Client is a thin typed wrapper over the subset of Anthropic's
// Managed Agents REST API moe needs: create a session, fetch its
// current state, list its output files, download one, and stream its
// event log.
//
// The surface is deliberately tiny — the agent loop runs server-side,
// so moe just kicks it off and reconciles results when it's done.
type Client struct {
	// BaseURL is the API host (no trailing slash). Empty means
	// DefaultBaseURL.
	BaseURL string
	// APIKey authenticates moe to Anthropic. Sent as x-api-key.
	APIKey string
	// HTTP lets tests and callers override the transport. Nil means
	// http.DefaultClient.
	HTTP *http.Client
}

// NewClientFromEnv reads ANTHROPIC_API_KEY and optional
// MOE_ANTHROPIC_API_BASE from the environment. Returns an error if the
// key is missing so callers fail fast on misconfiguration instead of
// receiving 401s mid-dispatch.
func NewClientFromEnv() (*Client, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("managed: ANTHROPIC_API_KEY is not set")
	}
	return &Client{
		BaseURL: os.Getenv("MOE_ANTHROPIC_API_BASE"),
		APIKey:  key,
	}, nil
}

// SessionResponse is the subset of POST/GET /v1/sessions we consume.
// Fields we don't use (billing, region, capabilities) are ignored
// during decode; adding them later is additive.
type SessionResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // e.g. "pending", "running", "completed", "failed"
	AgentID   string    `json:"agent_id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Terminal is true when the session has reached a state moe can
// reconcile (no more events will arrive). "completed" and "failed" are
// the documented terminal states; unknown states default to
// non-terminal so a spec change doesn't cause us to drop an in-flight
// run on the floor.
func (s *SessionResponse) Terminal() bool {
	switch s.Status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// Event is one line off the session's event stream. The managed
// events channel carries many shapes (assistant text, tool calls, tool
// results, status changes); moe renders the ones it understands and
// prints the raw type for the rest so no events are silently dropped.
type Event struct {
	ID   string          `json:"id,omitempty"`
	Type string          `json:"type"`
	Time time.Time       `json:"time,omitempty"`
	Raw  json.RawMessage `json:"-"`
}

// CreateSession POSTs /v1/sessions with s as the body and returns the
// decoded response.
func (c *Client) CreateSession(ctx context.Context, s *Session) (*SessionResponse, error) {
	var resp SessionResponse
	if err := c.do(ctx, "POST", "/v1/sessions", s, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSession fetches the current state for a session id.
func (c *Client) GetSession(ctx context.Context, id string) (*SessionResponse, error) {
	var resp SessionResponse
	if err := c.do(ctx, "GET", "/v1/sessions/"+id, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StreamEvents opens the session's event stream and emits one Event
// per JSON line. The stream closes when the session reaches a terminal
// state or the context is cancelled.
//
// Implementation note: the real API documents a Server-Sent Events
// stream; this reader is tolerant of both raw JSONL and SSE "data:"
// framing so the same code path works against lightweight test doubles.
func (c *Client) StreamEvents(ctx context.Context, id string, out chan<- Event) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url("/v1/sessions/"+id+"/events"), nil)
	if err != nil {
		return err
	}
	c.authHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return bodyError(resp)
	}
	r := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := r.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			if data := strings.TrimPrefix(line, "data: "); data != line || strings.HasPrefix(line, "{") {
				if strings.HasPrefix(data, "{") {
					ev := Event{Raw: json.RawMessage(data)}
					if err := json.Unmarshal([]byte(data), &ev); err == nil {
						select {
						case out <- ev:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// do is the shared request/response plumbing. It JSON-encodes body
// when non-nil, sets auth headers, decodes the response into out when
// non-nil, and lifts non-2xx responses into errors that quote the
// server's error body so misconfiguration is diagnosable from one line
// of stderr.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("managed: marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), reader)
	if err != nil {
		return err
	}
	c.authHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("managed: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return bodyError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) authHeaders(req *http.Request) {
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) url(path string) string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/") + path
}

// bodyError turns a non-2xx response into an error that includes the
// (truncated) response body — the single most useful piece of info
// when debugging a misshapen request.
func bodyError(resp *http.Response) error {
	const maxBody = 4 << 10
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	body := strings.TrimSpace(string(b))
	if body == "" {
		return fmt.Errorf("managed: %s: %d %s", resp.Request.URL, resp.StatusCode, resp.Status)
	}
	return fmt.Errorf("managed: %s: %d %s: %s", resp.Request.URL, resp.StatusCode, resp.Status, body)
}
