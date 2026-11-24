package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// pulseGate is the machine-readable verdict the survey writes to the
// canvas's `## Gate` section: what to open and what is next. A non-empty
// status is all the auto-close needs. The grammar is closed, so a retired
// key such as `threads` or `park` refuses the whole gate by name.
type pulseGate struct {
	Status string `json:"status"`
	// Loose carries runs to open. Each slug is `<project>/<slug>`.
	Loose []pulseRunSpec `json:"loose"`
	// Next is the one run to ride when this sweep is dynamic.
	Next *pulseNext `json:"next"`
}

// pulseNext names the one run to ride: a live parked run or a tagged idea,
// as `<project>/<slug>`, with the reason it comes before everything else.
// Repair "ci" continues a pushed run's existing PR instead.
type pulseNext struct {
	Run    string `json:"run"`
	Why    string `json:"why"`
	Repair string `json:"repair"`
}

// pulseRunSpec is one run the survey asks the harness to open. Slug is
// the slug base (the harness dates it on collision); Title and Why are
// what the operator reads on the chain canvas before kicking; Design
// seeds the new run's design canvas, so the design stage starts from
// the survey's findings instead of re-deriving them.
//
// Slug is `<project>/<slug>`, since one survey covers every project.
//
// Workflow picks the mint path and defaults to "sdlc" when empty. The
// allowlist is sdlc alone: the only workflow a pulse has a reason to
// propose fresh (chat is perpetual, pulse would be recursion). The
// field outlives the twin reflect it once also named so a survey that
// still writes `"workflow": "twin"` is told it isn't spawnable rather
// than having the key silently ignored.
//
// Chore names a judged chore instead, and is exclusive with every other
// field: the survey's claim is only "the condition the operator wrote
// holds", and everything about the resulting run — workflow, seed,
// cooldown — comes from the chore's own definition. Why stays the one
// line the operator reads.
//
// DesignOnly lowers the bar and shortens the ride in one move: the
// Design body is a *brief*, the harness rides the run exactly one
// stage — a headless design turn — and then parks it for the operator
// the way an operator-minted run whose design closed is parked. It is a
// field on the grammar that already carried the seed rather than a
// fifth gate key, so slug validation and the live-slug dedupe come
// along for free. Four shapes warn and skip it (see mint and
// applyPulseGate): a chore entry, a slug matching a live idea,
// a thread position, and — on a fresh slug only — a spec with no
// Design body. A slug matching a design-only *tagged* idea is the one
// live match that promotes: the idea's canvas is the brief, so the
// missing body costs nothing.
type pulseRunSpec struct {
	Slug       string `json:"slug"`
	Workflow   string `json:"workflow"`
	Title      string `json:"title"`
	Why        string `json:"why"`
	Design     string `json:"design"`
	Chore      string `json:"chore"`
	DesignOnly bool   `json:"design_only"`
}

// decodeStrict unmarshals one JSON value with unknown keys rejected. The
// gate grammar is closed: a key nobody defined means the survey wrote
// something the harness can't see, and the keys it does recognise are no
// basis for guessing what the rest meant.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

// readPulseGate reads the survey canvas and parses its `## Gate` JSON
// fence (the shared `stageGateJSON` grammar). The error is non-nil for
// every no-op shape the auto-close refusal keys on: a missing/unreadable
// canvas, an absent or empty fence (the skeleton placeholder),
// unparseable JSON, or an empty status. A read error reads as unfilled —
// the run lingers rather than auto-closing on a canvas we couldn't
// inspect. The reason comes back as an error rather than flattening to a
// bool so the refusal can name the shape it hit; the operator would
// otherwise diff the fence by eye.
//
// The decode is strict, so an unknown key anywhere in the grammar
// refuses the whole gate too. Grammar failures refuse everything —
// nothing in the fence said what the writer meant, so there is no
// trustworthy remainder. Semantic failures stay warn-and-continue at
// apply time (a slug that collides, a named run that doesn't exist):
// those are per-entry judgements the rest of the gate survives.
func readPulseGate(root, projectID, runID string) (pulseGate, error) {
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(projectID, runID, pulseDoc)))
	if err != nil {
		return pulseGate{}, err
	}
	payload, ok := stageGateJSON(string(body))
	if !ok {
		return pulseGate{}, errors.New("no `## Gate` json fence")
	}
	var g pulseGate
	if err := decodeStrict(payload, &g); err != nil {
		return pulseGate{}, err
	}
	if g.Status == "" {
		return pulseGate{}, errors.New("gate has no status")
	}
	if n := g.Next; n != nil {
		if _, _, err := splitProjectRun(n.Run); err != nil {
			return pulseGate{}, fmt.Errorf("next: %w", err)
		}
		if strings.TrimSpace(n.Why) == "" {
			return pulseGate{}, errors.New(`next: "why" is required`)
		}
		if n.Repair != "" && n.Repair != "ci" {
			return pulseGate{}, errors.New(`next: repair must be "ci"`)
		}
	}
	return g, nil
}
