package managed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCreateSessionSendsBodyAndAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sess_123","status":"pending"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	s := &Session{
		AgentID:       "agt_x",
		EnvironmentID: "env_x",
		Resources:     []Resource{{Type: "github_repository", URL: "u", MountPath: "/m"}},
	}
	resp, err := c.CreateSession(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/sessions" {
		t.Errorf("want POST /v1/sessions, got %s %s", gotMethod, gotPath)
	}
	if gotAuth != "k" {
		t.Errorf("x-api-key = %q, want k", gotAuth)
	}
	if !strings.Contains(gotBody, `"agent_id":"agt_x"`) {
		t.Errorf("request body missing agent_id: %s", gotBody)
	}
	if resp.ID != "sess_123" || resp.Status != "pending" {
		t.Errorf("decoded response = %+v", resp)
	}
}

func TestClientCreateSessionReportsServerErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad agent_id"}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	_, err := c.CreateSession(context.Background(), &Session{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad agent_id") {
		t.Errorf("error missing server body: %v", err)
	}
}

func TestClientGetSessionRoundtrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/sess_abc" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sess_abc","status":"completed","updated_at":"2026-04-17T12:00:00Z"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	resp, err := c.GetSession(context.Background(), "sess_abc")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Terminal() {
		t.Errorf("completed should be terminal: %+v", resp)
	}
	if resp.UpdatedAt.IsZero() {
		t.Errorf("updated_at not decoded")
	}
}

func TestClientStreamEventsParsesJSONLAndSSE(t *testing.T) {
	// Mix SSE "data: " framing and raw JSONL on the same stream so we
	// know the reader handles both — the real API documents SSE; test
	// doubles usually emit JSONL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"e1","type":"assistant.message","time":"2026-04-17T12:00:00Z"}`+"\n")
		_, _ = io.WriteString(w, "\n")
		_, _ = io.WriteString(w, `{"id":"e2","type":"tool.call"}`+"\n")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	ch := make(chan Event, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.StreamEvents(ctx, "sess", ch); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var got []string
	for ev := range ch {
		got = append(got, ev.Type)
	}
	want := []string{"assistant.message", "tool.call"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestClientSessionRequestRoundtripsJSON(t *testing.T) {
	// Confirm the Session marshal shape matches what a server would
	// need to read: primary first, checkout objects nested under each
	// resource, no stray fields.
	s := &Session{
		AgentID:       "agt",
		EnvironmentID: "env",
		Resources: []Resource{
			{Type: "github_repository", URL: "u", MountPath: "/m", Checkout: &Checkout{Type: "branch", Name: "main"}},
		},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	res := got["resources"].([]any)[0].(map[string]any)
	if res["checkout"].(map[string]any)["name"] != "main" {
		t.Errorf("checkout.name lost in marshal: %s", b)
	}
}
