//go:build !linux

package clonereclaim

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/runowner"
)

// Ordinary exclusive execution stays usable on other platforms. Recovery
// evidence created on a supported host cannot be discharged from this view.
func CheckRunRecovery(h *runowner.Handle) error {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return errors.New("recovery admission requires held run ownership")
	}
	_, err := os.Lstat(filepath.Join(h.Root, ".moe", "clone-reclamation", h.Project, h.Run))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("clone recovery requires the verified Linux host view")
}
