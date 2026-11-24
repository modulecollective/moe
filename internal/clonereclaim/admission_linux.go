//go:build linux

package clonereclaim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

// CheckRunRecovery is shared execution/reopen admission after taking ownership.
// A missing clone is safe only with no unresolved external generation evidence.
// It never guesses whether an absent tree was safely removed.
func CheckRunRecovery(h *runowner.Handle) error {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) || !project.ValidID(h.Project) || !run.ValidID(h.Run) {
		return errors.New("recovery admission requires exact held run ownership")
	}
	if err := CheckNamedShells(h); err != nil {
		return err
	}
	path := filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run)
	if _, err := os.Lstat(path); err == nil {
		r, err := LoadRegistry(h)
		if err != nil {
			return &HoldError{
				Reason:              fmt.Sprintf("cannot admit clone for %s/%s until host enrollment/recovery: %v; after stopping all old MoE binaries, shells, descendants and unmanaged users of this clone/resources, run `moe clone recover %s/%s` from an interactive host terminal to inspect and validate recovery", h.Project, h.Run, err, h.Project, h.Run),
				Receipt:             registryPath(h.Root, h.Project, h.Run),
				recordsLocationOnly: true,
			}
		}
		defer r.Close()
		if _, pending, err := r.PendingRemoval(); err != nil {
			return err
		} else if pending {
			return &HoldError{Reason: "authorized clone removal is pending", Receipt: r.Receipt()}
		}
		return r.ValidateEnvironment(h)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := openAbsoluteDir(filepath.Dir(path))
	if err == nil {
		parent.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return completedGenerations(h)
}

func completedGenerations(h *runowner.Handle) error {
	records, err := Registries(h)
	if err != nil {
		return err
	}
	defer func() {
		for _, r := range records {
			r.Close()
		}
	}()
	for _, r := range records {
		if _, pending, err := r.PendingRemoval(); err != nil {
			return err
		} else if pending {
			return &HoldError{Reason: "clone removal is pending", Receipt: r.Receipt()}
		}
		done, err := r.Removed()
		if err != nil {
			return err
		}
		if !done {
			return &HoldError{Reason: "clone absence has no completed removal receipt", Receipt: r.Receipt()}
		}
	}
	return nil
}
