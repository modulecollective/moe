//go:build linux

package clonereclaim

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/runowner"
)

// CheckRunRecovery is shared execution/reopen admission after taking ownership.
// A missing clone is safe only with no unresolved external generation evidence.
// It never guesses whether an absent tree was safely removed.
func CheckRunRecovery(h *runowner.Handle) error {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) || !namePattern.MatchString(h.Project) || !namePattern.MatchString(h.Run) {
		return errors.New("recovery admission requires exact held run ownership")
	}
	if err := CheckNamedShells(h); err != nil {
		return err
	}
	path := filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run)
	if _, err := os.Lstat(path); err == nil {
		r, err := LoadRegistry(h)
		if err != nil {
			return &HoldError{Reason: "clone generation needs host enrollment/recovery", Receipt: registryPath(h.Root, h.Project, h.Run)}
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
