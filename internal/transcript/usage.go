package transcript

import (
	"fmt"
	"io"
)

// Usage is the token accounting for one transcript file, split by the
// model that produced each turn. A stage that switched models mid-run
// (a resume under a different backend default) has one entry per model.
//
// It rides alongside the Event stream rather than inside it. The events
// are what an operator *reads*; usage is what the turn *cost*, and the
// two have no consumer in common — folding a KindUsage event into the
// stream would push a non-renderable variant through every renderer
// switch for the benefit of one caller. Same registry shape as the
// Event parsers, so a third backend still slots in as one adapter.
type Usage map[string]ModelUsage

// ModelUsage is one model's share of a transcript. The four token
// buckets are the ones every provider prices separately; Steps is the
// number of assistant turns that reported usage, which is the useful
// denominator for "how much context did each step drag along".
//
// CacheWrite is zero for a backend that doesn't report it (codex), which
// is a gap in the source, not a claim that nothing was cached.
type ModelUsage struct {
	Input      int64
	CacheWrite int64
	CacheRead  int64
	Output     int64
	Steps      int
}

// Add folds one turn's numbers into the model's running total.
func (u Usage) Add(model string, m ModelUsage) {
	cur := u[model]
	cur.Input += m.Input
	cur.CacheWrite += m.CacheWrite
	cur.CacheRead += m.CacheRead
	cur.Output += m.Output
	cur.Steps += m.Steps
	u[model] = cur
}

// Merge folds another transcript's usage into u — the aggregator's one
// combining step, so callers don't re-spell the per-field addition.
func (u Usage) Merge(other Usage) {
	for model, m := range other {
		u.Add(model, m)
	}
}

// Total collapses every model into one bucket. Steps sums too, so a
// mixed-model stage still reports its real step count.
func (u Usage) Total() ModelUsage {
	var t ModelUsage
	for _, m := range u {
		t.Input += m.Input
		t.CacheWrite += m.CacheWrite
		t.CacheRead += m.CacheRead
		t.Output += m.Output
		t.Steps += m.Steps
	}
	return t
}

// UsageParser is an adapter that extracts token accounting from one
// backend's JSONL stream. Stateless per call, like Parser.
type UsageParser func(r io.Reader) (Usage, error)

var usageParsers = map[string]UsageParser{}

// RegisterUsage adds a usage parser under name. Called from each
// adapter's init() next to Register.
func RegisterUsage(name string, p UsageParser) {
	if _, dup := usageParsers[name]; dup {
		panic("transcript: duplicate usage parser registration for " + name)
	}
	usageParsers[name] = p
}

// ParseUsage runs the usage parser registered under agent against r.
func ParseUsage(agent string, r io.Reader) (Usage, error) {
	p, ok := usageParsers[agent]
	if !ok {
		return nil, fmt.Errorf("transcript: no usage parser for agent %q", agent)
	}
	return p(r)
}
