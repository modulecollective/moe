// Package abstract maintains the 2–3 sentence `abstract` field on a
// run's run.json. It's invoked after each work turn, right before the
// commit, so the refreshed abstract rides along in the same commit as
// the document edits that produced it.
//
// The summarizer shells out to `claude --print` rather than calling the
// Messages API directly. That lets the abstract call inherit whatever
// auth the operator has already set up for the interactive `claude`
// CLI — OAuth, subscription keychain, or ANTHROPIC_API_KEY — instead
// of requiring ANTHROPIC_API_KEY specifically. See the no-abstract run
// for the history behind this choice.
//
// The call is non-fatal by design: a missing binary, a subprocess
// failure, or an empty response leaves the prior abstract in place.
// That's also our throttle — a pathologically large run that overruns
// the model's context will surface as a warning, which is our cue to
// cap it if it ever actually happens. Don't pre-build a cap; typical
// run docs are well under a few thousand tokens.
package abstract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// DefaultModel is the Haiku tier used for abstract synthesis. Cheap,
// fast, no reasoning depth needed — this is pure summarisation over
// bounded input with no tool use.
const DefaultModel = "claude-haiku-4-5"

// Summarizer produces the abstract text from the run's current docs.
// Exposed as an interface so tests can inject a deterministic stub
// without standing up a subprocess.
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

// ClaudeCLISummarizer invokes `claude --print` as a one-shot subprocess.
// Binary is the resolved absolute path to the binary; NewCLI fills it
// from exec.LookPath. Tests construct the struct directly with a fake
// binary path.
type ClaudeCLISummarizer struct {
	Binary string
	Model  string
}

// NewCLI resolves `claude` on PATH and returns a summarizer bound to
// it. Returns an error when the binary is missing so callers fail fast
// and the post-turn hook can surface the situation rather than silently
// no-op (which was the original bug motivating this rewrite).
func NewCLI() (*ClaudeCLISummarizer, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found on PATH: %w", err)
	}
	return &ClaudeCLISummarizer{Binary: bin, Model: DefaultModel}, nil
}

// Summarize implements Summarizer. It invokes `claude --print` with
// the abstract system prompt, pipes the user prompt on stdin (so large
// doc bodies don't hit argv limits), disables tools and session
// persistence, and returns trimmed stdout.
func (s *ClaudeCLISummarizer) Summarize(ctx context.Context, md *run.Metadata, docs map[string]string) (string, error) {
	user := buildUserPrompt(md, docs)
	args := []string{
		"--print",
		"--model", s.model(),
		"--system-prompt", systemPrompt,
		"--tools", "",
		"--output-format", "text",
		"--no-session-persistence",
	}
	cmd := exec.CommandContext(ctx, s.Binary, args...)
	cmd.Stdin = strings.NewReader(user)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("claude --print: %w: %s", err, msg)
		}
		return "", fmt.Errorf("claude --print: %w", err)
	}
	return stdout.String(), nil
}

func (s *ClaudeCLISummarizer) model() string {
	if s.Model != "" {
		return s.Model
	}
	return DefaultModel
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

// DefaultTimeout bounds the summarizer call so a wedged subprocess
// doesn't block the commit indefinitely. Callers that want a different
// budget can build their own context.
const DefaultTimeout = 30 * time.Second
