//go:build linux

package clonereclaim

import (
	"errors"
	"io"
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
	path := filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run)
	if _, err := os.Lstat(path); err == nil {
		r, err := LoadRegistry(h)
		if err != nil {
			return &HoldError{Reason: "clone generation needs host enrollment/recovery", Receipt: registryPath(h.Root, h.Project, h.Run)}
		}
		defer r.Close()
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
	records, err := openRegistry(h, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer records.Close()
	entries, err := records.ReadDir(1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 0 {
		return &HoldError{Reason: "clone absence has no completed removal receipt", Receipt: records.Name()}
	}
	return nil
}
