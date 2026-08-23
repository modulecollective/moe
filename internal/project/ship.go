package project

import (
	"fmt"
	"path/filepath"

	"github.com/modulecollective/moe/internal/git"
)

// The per-project ship setting: the operator's standing answer to "how
// does a finished run land here".
//
// Orthogonal to mode. Mode caps what the clock may *start*; ship picks
// how a finished run *lands*, whoever started it. A safe project the
// operator cascades by hand still ships by its ship setting; an auto
// project's heartbeat rides land as PRs unless ship says merge.
//
// Stored as one field in project.json, absent meaning pr. That is
// deliberately *not* mode's "absent means today's behaviour" property:
// bare push and every machine-driven ship used to merge, so adopting
// this flips them to PR for every project that has never heard of the
// setting. That flip is the point — straight-to-merge becomes the
// exception you opt into.

// ShipMode is how a project's finished runs reach its default branch.
type ShipMode string

const (
	// ShipPR: push the branch and open (or re-use) a PR, leaving the run
	// `pushed` and its sandbox in place until the PR merges. The default.
	ShipPR ShipMode = "pr"
	// ShipMerge: fast-forward the default branch, delete the remote
	// branch, drop the sandbox.
	ShipMerge ShipMode = "merge"
)

// Ships lists both in the order every surface offers them — the
// reversible route first, matching Modes' most-cautious-first reading.
var Ships = []ShipMode{ShipPR, ShipMerge}

// ParseShip validates an operator-typed ship word.
func ParseShip(s string) (ShipMode, error) {
	switch ShipMode(s) {
	case ShipPR, ShipMerge:
		return ShipMode(s), nil
	}
	return "", fmt.Errorf("project: unknown ship route %q (want one of: pr, merge)", s)
}

// ShipOf reports md's ship route. Absent reads as pr.
//
// So does an unrecognised value, which can only arrive by hand-editing:
// the write path validates through ParseShip, and the fail-safe
// direction here is the reversible route — a PR can be closed unmerged,
// a merge has landed.
func ShipOf(md *Metadata) ShipMode {
	if md == nil {
		return ShipPR
	}
	if ShipMode(md.Ship) == ShipMerge {
		return ShipMerge
	}
	return ShipPR
}

// ReadShip loads one project's ship route from disk, for the callers
// that arrive with only an id. A project.json that won't load reads as
// the default rather than failing the caller: every one of these sites
// is about to hit a louder error of its own if the project is really
// broken.
func ReadShip(root, id string) ShipMode {
	md, err := Load(root, id)
	if err != nil {
		return ShipPR
	}
	return ShipOf(md)
}

// SetShip writes a project's ship route and commits it on main.
//
// Same shape as SetMode, and for the same reasons: no machine trailers
// (this is an operator act, and it reads as one to every journal
// consumer), and the default is written as the absent field so the two
// spellings can't drift apart on surfaces that read project.json
// directly.
//
// The caller owns the repolock and the journal push (see
// sync.WithJournalPush); this is the disk write and the commit.
func SetShip(root, id string, ship ShipMode) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("project: id %q must match %s", id, idPattern)
	}
	if _, err := ParseShip(string(ship)); err != nil {
		return err
	}
	md, err := Load(root, id)
	if err != nil {
		return err
	}
	if ShipOf(md) == ship {
		return nil
	}
	md.Ship = ""
	if ship != ShipPR {
		md.Ship = string(ship)
	}
	rel := filepath.Join(Dir(id), "project.json")
	if err := writeJSON(filepath.Join(root, rel), md); err != nil {
		return err
	}
	if err := git.Run(root, "add", rel); err != nil {
		return fmt.Errorf("project: git add: %w", err)
	}
	msg := fmt.Sprintf("Set project %s ship: %s", id, ship)
	if err := git.Run(root, "commit", "-m", msg); err != nil {
		return fmt.Errorf("project: git commit: %w", err)
	}
	return nil
}
