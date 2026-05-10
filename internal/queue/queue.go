// Package queue implements the operator playlist behind `moe queue`.
// Items are structured (workflow, project, run) triples persisted to
// .moe/queue.json (operator-local, never committed). The package
// owns the JSON-on-disk shape, identity-matched mutation, and the
// liveness classification that decides whether a head item should be
// dispatched or dropped.
//
// The cli/queue.go entry-point handler keeps the walker proper: the
// SIGINT-aware countdown, dispatch into runResume, and the
// repolock wrapping of peek/pop. Functions here are pure load-and-
// save primitives plus a read-only Classify that reads run.json off
// disk; the lock is the caller's responsibility, applied at the
// peek/pop boundaries the walker design calls out.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/run"
)

// Item is one entry in .moe/queue.json. Workflow + Project + Run is
// the identity used for duplicate refusal and identity-matched pop.
type Item struct {
	Workflow string `json:"workflow"`
	Project  string `json:"project"`
	Run      string `json:"run"`
}

// String renders the item the way the walker logs it.
func (q Item) String() string {
	return fmt.Sprintf("%s %s/%s", q.Workflow, q.Project, q.Run)
}

// Path is the on-disk JSON file. Lives under .moe/ alongside
// clones/ and worktrees/ — operator-local, never committed.
func Path(root string) string {
	return filepath.Join(root, ".moe", "queue.json")
}

// Load reads .moe/queue.json. A missing or empty file is a normal
// state (no runs queued yet) and returns (nil, nil).
func Load(root string) ([]Item, error) {
	p := Path(root)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: read %s: %w", p, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("queue: parse %s: %w", p, err)
	}
	return items, nil
}

// Save writes items to .moe/queue.json with a deterministic indent.
// Always writes a JSON array — empty queue persists as `[]` rather
// than a missing file so the caller can tell "explicitly empty"
// from "never used."
func Save(root string, items []Item) error {
	p := Path(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("queue: mkdir %s: %w", filepath.Dir(p), err)
	}
	if items == nil {
		items = []Item{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("queue: marshal: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return fmt.Errorf("queue: write %s: %w", p, err)
	}
	return nil
}

// IndexOf returns the 1-based position of an identity-matching item,
// or 0 if not present.
func IndexOf(items []Item, target Item) int {
	for i, it := range items {
		if it == target {
			return i + 1
		}
	}
	return 0
}

// RemoveFirst returns items with the first identity-match of target
// dropped, plus a flag indicating whether anything was removed. Pure
// over the slice — the caller persists the result via Save.
func RemoveFirst(items []Item, target Item) ([]Item, bool) {
	out := items[:0]
	removed := false
	for _, it := range items {
		if !removed && it == target {
			removed = true
			continue
		}
		out = append(out, it)
	}
	return out, removed
}

// AddItem returns items with item appended to the back (front=false)
// or prepended to the head (front=true). Pure over the slice.
func AddItem(items []Item, item Item, front bool) []Item {
	if front {
		return append([]Item{item}, items...)
	}
	return append(items, item)
}

// Liveness is the verdict for a queued item: ready to dispatch, or
// the reason it should be dropped.
type Liveness int

const (
	LivenessReady        Liveness = iota
	LivenessDropMissing           // run.json gone
	LivenessDropTerminal          // already merged/closed/promoted/pushed
	LivenessDropOther             // load error or workflow mismatch
)

// Classify decides whether the walker should dispatch an item or
// drop it. Returns the verdict and a short reason suitable for the
// walker's drop log line. Read-only over disk — the run.json read
// doesn't need the bureaucracy lock.
func Classify(root string, it Item) (Liveness, string) {
	md, err := run.Load(root, it.Project, it.Run)
	if errors.Is(err, run.ErrRunNotFound) {
		return LivenessDropMissing, "run not found"
	}
	if err != nil {
		return LivenessDropOther, err.Error()
	}
	if md.Workflow != it.Workflow {
		return LivenessDropOther, "workflow=" + md.Workflow
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted, run.StatusPushed:
		return LivenessDropTerminal, "already " + md.Status
	}
	return LivenessReady, ""
}
