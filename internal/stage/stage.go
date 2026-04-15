// Package stage defines MoE's request-lifecycle checkpoints.
//
// A stage is a named point in a request's life that the operator signs off
// on — "design settled, implementation can start," "ready to open a PR,"
// and so on. Sign-offs are recorded as commit trailers on the bureaucracy
// repo's main branch (MoE-Stage-Signed / MoE-Stage-Unsigned), so the journal
// itself is the source of truth; there is no status field to keep in sync.
//
// The active set is small on purpose. `design` and `code` have real meaning
// today; `review`, `test`, `retro`, and `deploy` are reserved names with no
// side-effects yet, so real workflows can start accumulating the trailers
// immediately and we can wire behavior in later without renaming history.
package stage

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
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
	"review": {Name: "review", Help: "reviewed (trailer only; behavior TBD)"},
	"test":   {Name: "test", Help: "tests run/passed (trailer only; behavior TBD)"},
	"retro":  {Name: "retro", Help: "retrospective captured (trailer only; behavior TBD)"},
	"deploy": {Name: "deploy", Help: "shipped to an environment (trailer only; behavior TBD)"},
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
	signedAt, err := latestTrailerTime(root, requestID, "MoE-Stage-Signed", name)
	if err != nil {
		return false, err
	}
	if signedAt == "" {
		return false, nil
	}
	unsignedAt, err := latestTrailerTime(root, requestID, "MoE-Stage-Unsigned", name)
	if err != nil {
		return false, err
	}
	return unsignedAt == "" || signedAt > unsignedAt, nil
}

// latestTrailerTime returns the committer epoch (%ct) of the most recent
// commit mentioning both MoE-Request: <requestID> and <trailer>: <value>,
// or "" if no such commit exists.
func latestTrailerTime(root, requestID, trailer, value string) (string, error) {
	cmd := exec.Command("git",
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("%s: %s", trailer, value),
		"--grep", fmt.Sprintf("MoE-Request: %s", requestID),
		"--format=%ct",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("stage: git log: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
