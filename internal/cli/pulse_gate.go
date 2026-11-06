package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/run"
)

// pulseGate is the machine-readable verdict the survey agent writes to
// the canvas's `## Gate` section. A non-empty status is all the
// auto-close decision needs — a pulse has no ready/blocked advance
// vocabulary, only close-or-linger.
//
// Spawning and ordering are one grammar. The gate used to split them
// into two lists that named each other through a shared slug namespace,
// which is why it needed aliases — and why a twin entry's agent-chosen
// alias could silently shadow another entry, or a real parked run, and
// order the wrong thing. A run is now written *where it goes*: inline in
// a thread's `runs` at its position, or in `loose` when the survey has
// no ordering opinion. Placement is positional, so nothing has to name
// anything.
type pulseGate struct {
	Status string `json:"status"`
	// Loose carries runs to open with no ordering claim attached. They
	// park standalone and unchained — the normal outcome for work whose
	// order the survey isn't sure of. It stays a separate key rather
	// than folding into a self-rooted thread because "park it, I have no
	// opinion" and "root a new thread with this" are different
	// judgements.
	Loose []pulseRunSpec `json:"loose"`
	// Threads carries the survey's ordering opinion: runs in execution
	// order, each group placed after an existing run, under a freshly
	// named head, or self-rooted. See pulse_groom.go. The lane bar
	// prices this separately from the spawn bar.
	Threads []pulseThread `json:"threads"`
}

// pulseRunSpec is one run the survey asks the harness to open. Slug is
// the slug base (the harness dates it on collision); Title and Why are
// what the operator reads on the chain canvas before kicking; Design
// seeds the new run's design canvas, so the design stage starts from
// the survey's findings instead of re-deriving them.
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

// pulseThread is one entry in the gate's `threads` list: runs in
// execution order, plus where the thread goes.
//
// Onto attaches the group after that run, wherever it sits. Head mints
// a chain placeholder with that slug base and chains the group under it.
// Neither self-roots the group as its own headless thread — a dynamic
// sweep's kick loop starts it, anything else parks it. Onto and Head
// together is a warn-and-skip: they are two different answers to the
// same question.
//
// Park is the survey's one-line reason the operator should look at this
// thread before it runs. Non-empty parks the thread; absent lets it kick
// once grooming is done, which is the default under a dynamic sweep and
// impossible without one (see pulseSelfKick). Park is the marked case
// because the error ledger only ever ran one way — strandings, never a
// runaway — so the field the survey has to spend a sentence on is the
// one that stops motion, not the one that causes it.
type pulseThread struct {
	Onto string             `json:"onto"`
	Head string             `json:"head"`
	Runs []pulseThreadEntry `json:"runs"`
	Park string             `json:"park"`
}

// pulseThreadEntry is one position in a thread: a bare string naming a
// parked run that already exists, an object minting one right there, or
// an existing run carrying a question for the operator. The shapes are
// the whole reason the alias map could be deleted — a run that doesn't
// exist yet is described where it belongs rather than declared elsewhere
// and referenced by name, and a question is asked *at* the run whose
// future agent needs the answer rather than through a second gate field
// that names it.
type pulseThreadEntry struct {
	// Existing names a parked run in the project, resolved at apply
	// time. Set for the bare-string form and for the ask form.
	Existing string
	// Spec describes a run to mint at this position. Set only for the
	// mint form.
	Spec *pulseRunSpec
	// Ask is one prose question to open on Existing:
	//
	//	{"run": "change-auth-defaults", "ask": "Which policy — …?"}
	//
	// The `run` key is the discriminator: an inline mint spec has `slug`,
	// never `run`, so the two object forms can't be confused.
	// Deliberately positional — nothing here names a run in another gate
	// field.
	//
	// Asking holds nothing. A survey that needs the answer before the
	// work also writes the thread's `park`, naming the question; the two
	// are separate acts because a question the work need not wait for is
	// a question worth asking.
	Ask string
	// Repair nominates CI repair on Existing, exclusive with Ask and Spec.
	Repair string
}

// specs lists every run the gate asked the harness to open, in document
// order: the loose ones, then each thread's inline mints. The reader for
// anything that wants "what did this sweep propose" without caring where
// it landed.
func (g pulseGate) specs() []pulseRunSpec {
	out := append([]pulseRunSpec(nil), g.Loose...)
	for _, th := range g.Threads {
		for _, entry := range th.Runs {
			if entry.Spec != nil {
				out = append(out, *entry.Spec)
			}
		}
	}
	return out
}

func (e *pulseThreadEntry) UnmarshalJSON(b []byte) error {
	var slug string
	if err := json.Unmarshal(b, &slug); err == nil {
		e.Existing, e.Spec, e.Ask, e.Repair = slug, nil, "", ""
		return nil
	}
	// Probe for the `run` discriminator before committing to a branch,
	// rather than trying each shape in sequence. Try-in-sequence let an
	// ask form with a typo'd key drift into the spec branch and out the
	// other side as an empty spec: the question the survey wrote for the
	// operator vanished, and nothing said so. An object carrying `run` is
	// an ask form or an error — never a spec.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return fmt.Errorf("thread entry is neither a run slug nor a run spec: %w", err)
	}
	if _, isAsk := keys["run"]; isAsk {
		var ask struct {
			Run    string `json:"run"`
			Ask    string `json:"ask"`
			Repair string `json:"repair"`
		}
		if err := decodeStrict(b, &ask); err != nil {
			return fmt.Errorf("%s: %w", threadEntryLabel(keys, "run"), err)
		}
		if ask.Repair != "" && (ask.Repair != "ci" || len(keys) != 2) {
			return errors.New(`repair entry requires only "run" and "repair":"ci"`)
		}
		if _, present := keys["repair"]; present && ask.Repair == "" {
			return errors.New(`repair must be "ci"`)
		}
		if ask.Run == "" {
			return errors.New(`thread entry has an empty "run"`)
		}
		// `{"run": "x"}` with no ask is the string form written long-hand
		// and means exactly that: a position, no question.
		e.Existing, e.Spec, e.Ask, e.Repair = ask.Run, nil, ask.Ask, ask.Repair
		return nil
	}
	var spec pulseRunSpec
	if err := decodeStrict(b, &spec); err != nil {
		return fmt.Errorf("%s: %w", threadEntryLabel(keys, "slug"), err)
	}
	e.Existing, e.Spec, e.Ask, e.Repair = "", &spec, "", ""
	return nil
}

// threadEntryLabel names the offending entry in a decode error using
// whichever identifying key survived. A long thread's parse error is
// useless if the operator can't tell which position it came from.
func threadEntryLabel(keys map[string]json.RawMessage, key string) string {
	var s string
	if err := json.Unmarshal(keys[key], &s); err != nil || s == "" {
		return "thread entry"
	}
	return fmt.Sprintf("thread entry %q", s)
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
	return g, nil
}
