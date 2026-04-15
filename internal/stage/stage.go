// Package stage defines MoE's request-lifecycle checkpoints.
//
// A stage is a named point in a request's life that the operator signs off
// on — "design settled, implementation can start," "ready to open a PR,"
// and so on. Sign-offs are recorded as commit trailers on the bureaucracy
// repo's main branch (MoE-Stage-Signed / MoE-Stage-Unsigned), so the journal
// itself is the source of truth; there is no status field to keep in sync.
//
// The active set is small on purpose. `design` and `code` have real meaning
// today; additional stages get added when a concrete use case forces the
// question, not in anticipation. Stage names are permanent history via
// commit trailers, so reserving labels before their semantics are settled
// is a cost, not a hedge.
package stage

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stage describes one named checkpoint in a request's lifecycle.
type Stage struct {
	Name     string
	Requires []string // stages that must be signed before this one
	Help     string
}

var all = map[string]Stage{
	"design": {Name: "design", Help: "design is settled; implementation can start"},
	"code":   {Name: "code", Requires: []string{"design"}, Help: "code is done; ready to push the submodule and open a PR"},
}

// Lookup returns the stage definition for name.
func Lookup(name string) (Stage, bool) {
	s, ok := all[name]
	return s, ok
}

// Names returns all known stage names in a stable order.
func Names() []string {
	out := make([]string, 0, len(all))
	for n := range all {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Dependents returns the stages that list name in their Requires, in stable
// order. Used to cascade unsign: if the operator reopens design, any stage
// that required design must also come unsigned, since its precondition is
// no longer met.
func Dependents(name string) []string {
	var out []string
	for n, s := range all {
		for _, dep := range s.Requires {
			if dep == name {
				out = append(out, n)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsSigned reports whether the named stage is currently signed for requestID
// in the bureaucracy repo rooted at root. A stage is signed iff its most
// recent MoE-Stage-Signed commit is newer than its most recent
// MoE-Stage-Unsigned commit (or no unsign exists).
func IsSigned(root, requestID, name string) (bool, error) {
	_, signedAt, err := latestTrailerCommit(root, requestID, "MoE-Stage-Signed", name)
	if err != nil {
		return false, err
	}
	if signedAt.IsZero() {
		return false, nil
	}
	_, unsignedAt, err := latestTrailerCommit(root, requestID, "MoE-Stage-Unsigned", name)
	if err != nil {
		return false, err
	}
	return unsignedAt.IsZero() || signedAt.After(unsignedAt), nil
}

// LatestSign returns the commit SHA and committer time of the most recent
// MoE-Stage-Signed: <name> commit for requestID, or ("", time.Time{}, nil)
// if no such commit exists. Used by upstream-change detection so a
// downstream agent can diff from the last-known SHA against HEAD.
//
// Note: this is "latest sign" not "currently signed" — a later
// MoE-Stage-Unsigned commit doesn't suppress this result. Callers that need
// "currently signed" should compose with IsSigned.
func LatestSign(root, requestID, name string) (sha string, when time.Time, err error) {
	return latestTrailerCommit(root, requestID, "MoE-Stage-Signed", name)
}

// Active returns the stage the request is currently working in: the
// stage that is unsigned and whose prerequisites are all signed. Returns
// (Stage{}, false, nil) when every defined stage is signed (work is
// finished) or when no stage is yet ready (shouldn't happen with the
// current stage graph, but the no-progress case is reported rather than
// guessed).
//
// Resolution is deterministic: iterate Names() (alphabetical) and pick the
// first ready+unsigned candidate. Today the graph is
// design (no prereqs) → code (requires design), so this collapses to
// "design until signed, then code until signed, then nothing." New stages
// inherit the same logic for free as long as their Requires are accurate.
func Active(root, requestID string) (Stage, bool, error) {
	signed := make(map[string]bool, len(all))
	for _, n := range Names() {
		ok, err := IsSigned(root, requestID, n)
		if err != nil {
			return Stage{}, false, err
		}
		signed[n] = ok
	}
	for _, n := range Names() {
		s := all[n]
		if signed[n] {
			continue
		}
		ready := true
		for _, dep := range s.Requires {
			if !signed[dep] {
				ready = false
				break
			}
		}
		if ready {
			return s, true, nil
		}
	}
	return Stage{}, false, nil
}

// latestTrailerCommit returns the SHA and committer time of the most recent
// commit mentioning both MoE-Request: <requestID> and <trailer>: <value>,
// or ("", time.Time{}, nil) if no such commit exists. Times come from %ct
// (unix epoch seconds); zero-time signals "not found" so callers can stay
// in time.Time-land for comparisons.
func latestTrailerCommit(root, requestID, trailer, value string) (sha string, when time.Time, err error) {
	cmd := exec.Command("git",
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("%s: %s", trailer, value),
		"--grep", fmt.Sprintf("MoE-Request: %s", requestID),
		"--format=%H %ct",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("stage: git log: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", time.Time{}, nil
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("stage: unexpected git log output %q", line)
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("stage: parse %%ct %q: %w", parts[1], err)
	}
	return parts[0], time.Unix(epoch, 0).UTC(), nil
}
