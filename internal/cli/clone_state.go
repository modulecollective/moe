package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/session"
)

func cloneRunState(h *runowner.Handle) (*run.Metadata, error) { return readOwnedRunState(h, false) }

func readOwnedRunState(h *runowner.Handle, allowShipRecord bool) (*run.Metadata, error) {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return nil, errors.New("clone operation requires explicit held ownership")
	}
	var md *run.Metadata
	err := repolock.With(h.Root, repolock.Options{Purpose: "clone-preflight", Run: h.Project + "/" + h.Run}, func() error {
		var err error
		md, err = run.Load(h.Root, h.Project, h.Run)
		if err != nil {
			return err
		}
		return checkRunDebts(h.Root, md, allowShipRecord)
	})
	return md, err
}

// Caller holds repolock. A branch-less pending close is still a reservation;
// only the existing push roll-forward path may enter with unrecorded shipment.
func checkRunDebts(root string, md *run.Metadata, allowShipRecord bool) error {
	if err := session.RequireUnreserved(root, md.Project, md.ID); err != nil {
		return err
	}
	path := filepath.Join(root, ".moe", "session-closes", md.Project, md.ID)
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unreadable pending session close authority")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		entries, err := f.ReadDir(1)
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries) > 0 {
			return errors.New("pending session close requires session recovery")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !allowShipRecord {
		return requireRecordedShip(root, md)
	}
	return nil
}

func requireRecordedShip(root string, md *run.Metadata) error {
	for _, path := range []string{prRecordPendingPath(root, md), mergeRecordPendingPath(root, md)} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("unrecorded shipment requires push record recovery")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
