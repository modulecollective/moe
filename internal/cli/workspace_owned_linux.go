//go:build linux

package cli

import (
	"errors"
	"os"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func ensureRunClone(h *runowner.Handle) (string, error) {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return "", errors.New("clone attach requires explicit held run ownership")
	}
	if err := clonereclaim.CheckRunRecovery(h); err != nil {
		return "", err
	}
	path := sandbox.Path(h.Root, h.Project, h.Run)
	_, err := os.Lstat(path)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return "", err
	}
	if !fresh {
		return path, nil
	}
	path, err = sandbox.EnsureRetained(h.Root, h.Project, h.Run)
	if err != nil {
		return "", err
	}
	r, err := clonereclaim.Enroll(h)
	if err != nil {
		return "", err
	}
	r.Close()
	return path, nil
}
