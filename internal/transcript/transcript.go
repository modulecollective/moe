// Package transcript turns the per-agent JSONL files MoE mirrors next
// to each document into operator-readable text. Two surfaces consume
// it: `moe log <stage>` (render the full file on demand) and the
// auto-tail printed after a one-shot stage exits. Both share one
// renderer and a single normalised Event type; per-agent code is
// confined to the adapter that produces those Events from a given
// backend's native event vocabulary.
//
// Why a normalised type rather than rendering each agent's events
// directly: claude and codex emit overlapping concepts (tool call,
// tool result, assistant text, system error) under wildly different
// shapes. Collapsing them at parse time means the renderer doesn't
// need to know which backend wrote the file, and a future third
// backend slots in by dropping a sibling adapter without touching
// the renderer or `moe log`.
package transcript

import (
	"fmt"
	"io"
	"time"
)

// Kind tags the variant of an Event. Each kind uses a different
// subset of the Event fields — see Event for the per-kind contract.
// A zero Kind is KindAssistantText, which is the most common event
// and the most reasonable default for "unknown" producers.
type Kind int

const (
	// KindAssistantText is a message from the agent. Text is the body.
	KindAssistantText Kind = iota
	// KindUserText is a message from the user. Text is the body.
	// First-turn boot prompts arrive here; the renderer surfaces them
	// because they bookend the conversation.
	KindUserText
	// KindToolCall is the agent invoking a tool. Tool is the tool
	// name, Args is a short human-readable summary of the input,
	// CallID pairs it with a later KindToolResult.
	KindToolCall
	// KindToolResult is the output of a tool call. CallID pairs with
	// the corresponding KindToolCall, Output is the stdout/stderr/
	// returned text, Error is set when the tool reported failure.
	KindToolResult
	// KindSystem is an out-of-band event the operator needs to see:
	// sandbox denials, timeouts, aborts, bridge status. Text is the
	// message.
	KindSystem
)

// Event is one normalised entry in the transcript. The active fields
// depend on Kind — see the constants above. Time is best-effort; an
// adapter that can't pull a timestamp leaves it zero and the renderer
// renders without one.
type Event struct {
	Kind   Kind
	Time   time.Time
	Text   string
	Tool   string
	Args   string
	CallID string
	Output string
	Error  bool
}

// Parser is an adapter that turns one backend's JSONL stream into a
// slice of normalised Events. Implementations are stateless (each
// call gets a fresh reader) so a caller can call the same Parser
// from multiple goroutines if needed.
type Parser func(r io.Reader) ([]Event, error)

var parsers = map[string]Parser{}

// Register adds a parser under name. Called from each adapter
// package's init().
func Register(name string, p Parser) {
	if _, dup := parsers[name]; dup {
		panic("transcript: duplicate parser registration for " + name)
	}
	parsers[name] = p
}

// Parse runs the parser registered under agent against r.
func Parse(agent string, r io.Reader) ([]Event, error) {
	p, ok := parsers[agent]
	if !ok {
		return nil, fmt.Errorf("transcript: unknown agent %q", agent)
	}
	return p(r)
}

// Tail returns the last n events of ev, or all of ev if n <= 0 or
// n >= len(ev). The slice shares ev's backing array — callers that
// mutate the result mutate ev.
func Tail(ev []Event, n int) []Event {
	if n <= 0 || n >= len(ev) {
		return ev
	}
	return ev[len(ev)-n:]
}
