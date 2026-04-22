// Package abstract maintains the 2–3 sentence `abstract` field on a
// run's run.json. It's invoked after each work turn, right before the
// commit, so the refreshed abstract rides along in the same commit as
// the document edits that produced it.
//
// The call is non-fatal by design: an API outage or oversized-context
// failure logs a warning and leaves the prior abstract in place. That's
// also our throttle — a pathologically large run that overruns the
// model's context will surface as a warning, which is our cue to cap
// it if it ever actually happens. Don't pre-build a cap; typical run
// docs are well under a few thousand tokens.
package abstract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// DefaultModel is the Sonnet tier used for abstract synthesis. Cheap,
// fast, no reasoning depth needed — this is pure summarisation over
// bounded input with no tool use.
const DefaultModel = "claude-sonnet-4-6"

// DefaultBaseURL is the Anthropic Messages API host.
const DefaultBaseURL = "https://api.anthropic.com"

// maxOutputTokens caps the response so a misbehaving model can't
// generate a novella. 300 tokens is comfortably above 2–3 sentences.
const maxOutputTokens = 300

// Summarizer produces the abstract text from the run's current docs.
// Exposed as an interface so tests can inject a deterministic stub
// without standing up an HTTP server.
type Summarizer interface {
	Summarize(ctx context.Context, md *run.Metadata, docs map[string]string) (string, error)
}

// Update reads every documents/<doc>/content.md for the run, asks the
// summarizer for a fresh abstract, and mutates md.Abstract in place.
// The caller is responsible for persisting md (run.Save) and staging
// run.json for the turn commit.
//
// Returns an error only when the summarizer call fails or the run
// cannot be read. Callers should treat errors as non-fatal: log a
// warning, skip the update, continue the commit. md.Abstract is left
// unchanged on any error path.
func Update(ctx context.Context, root string, md *run.Metadata, s Summarizer) error {
	docs, err := readAllDocs(root, md)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		// No docs written yet — nothing to summarize. Leave any prior
		// abstract in place (shouldn't happen pre-first-turn, but
		// harmless if it does).
		return nil
	}
	out, err := s.Summarize(ctx, md, docs)
	if err != nil {
		return err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return errors.New("abstract: summarizer returned empty text")
	}
	md.Abstract = out
	return nil
}

// readAllDocs walks md.Documents and returns a map[docID] -> content.
// Missing files are skipped — a document entry can exist before the
// first edit lands on disk.
func readAllDocs(root string, md *run.Metadata) (map[string]string, error) {
	out := make(map[string]string, len(md.Documents))
	for docID := range md.Documents {
		path := filepath.Join(root, run.ContentPath(md.Project, md.ID, docID))
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("abstract: read %s: %w", path, err)
		}
		out[docID] = string(b)
	}
	return out, nil
}

// AnthropicSummarizer calls POST /v1/messages. Construct with NewFromEnv
// to read ANTHROPIC_API_KEY; construct directly for tests that supply
// their own http.Client.
type AnthropicSummarizer struct {
	APIKey  string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// NewFromEnv builds a summarizer from ANTHROPIC_API_KEY (and the
// optional MOE_ANTHROPIC_API_BASE override that moe's managed-agents
// client also honors). Returns an error when the key is unset so
// callers fail fast and the post-turn hook can skip cleanly rather
// than silently swallow misconfiguration.
func NewFromEnv() (*AnthropicSummarizer, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("abstract: ANTHROPIC_API_KEY is not set")
	}
	return &AnthropicSummarizer{
		APIKey:  key,
		BaseURL: os.Getenv("MOE_ANTHROPIC_API_BASE"),
		Model:   DefaultModel,
	}, nil
}

// Summarize implements Summarizer.
func (s *AnthropicSummarizer) Summarize(ctx context.Context, md *run.Metadata, docs map[string]string) (string, error) {
	user := buildUserPrompt(md, docs)
	body := messagesRequest{
		Model:     s.model(),
		MaxTokens: maxOutputTokens,
		System:    systemPrompt,
		Messages: []messagesMessage{
			{Role: "user", Content: user},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("abstract: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.url()+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("abstract: POST /v1/messages: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("abstract: %d %s: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}
	var decoded messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("abstract: decode response: %w", err)
	}
	var out strings.Builder
	for _, blk := range decoded.Content {
		if blk.Type == "text" {
			out.WriteString(blk.Text)
		}
	}
	return out.String(), nil
}

func (s *AnthropicSummarizer) model() string {
	if s.Model != "" {
		return s.Model
	}
	return DefaultModel
}

func (s *AnthropicSummarizer) url() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return DefaultBaseURL
}

// systemPrompt narrows the model to the one task: 2–3 sentences of
// prose about the run's substance, no preamble, no workflow or status
// framing (those live structured in run.json already).
const systemPrompt = `You write a 2–3 sentence prose abstract of a Ministry of Everything run, summarising the substance of the documents produced so far.

Rules:
- 2 or 3 sentences, no more. Prose, not bullets.
- Content-first: what the run is about and where it stands substantively.
- Do not start with "This run is about…" or similar preamble.
- Do not mention the workflow name, the stage, or the status — those fields are already structured in run.json.
- Do not invent facts not supported by the documents.
- Output the abstract only. No surrounding quotes, no headings.`

func buildUserPrompt(md *run.Metadata, docs map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s/%s\nTitle: %s\n\n", md.Project, md.ID, md.Title)
	// Stable order makes the prompt reproducible across runs and easier
	// to diff when diagnosing a bad abstract.
	keys := make([]string, 0, len(docs))
	for k := range docs {
		keys = append(keys, k)
	}
	// Alphabetical; the stage-ladder order would require a workflow
	// lookup and isn't worth the coupling for a summarisation prompt.
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "--- document: %s ---\n%s\n\n", k, docs[k])
	}
	return b.String()
}

// sortStrings keeps the package zero-import other than stdlib it already uses.
func sortStrings(s []string) {
	// Tiny insertion sort — document counts per run are O(stages).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// messagesRequest / messagesResponse are the tiny slices of the
// Messages API shape that the abstract call uses. Fields not listed
// are ignored on decode; if the API grows additive fields, they land
// unsurfaced and the call still succeeds.
type messagesRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []messagesMessage `json:"messages"`
}

type messagesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// DefaultTimeout bounds the summarizer call so a wedged API doesn't
// block the commit indefinitely. Callers that want a different budget
// can build their own context.
const DefaultTimeout = 30 * time.Second
