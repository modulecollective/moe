package runcontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// An owner that knows how a request ended but cannot commit the receipt
// (the repository lock timed out, a hook refused the commit) must not
// leave the record saying the request is still open: an accepted finish
// that reads as open is a grant the next owner would act on. The
// settlement is kept here instead — local state beside the run guards,
// written without the repository lock or a commit, so it survives the
// failure that refused the journal write.
//
// It never grants anything. A request listed here is settled as far as
// owners are concerned (callers skip it when looking for an open finish),
// and the next guard holder retries the journal write before admitting
// work. Settle drops an entry once the record carries the request as
// settled, so the journal stays the authority whenever it can be written.

// unrecordedPath is the local file of settlements still owed to a run's
// record.
func unrecordedPath(root, projectID, runID string) string {
	return filepath.Join(root, ".moe", "control-unrecorded", projectID, runID+".json")
}

// Unrecorded returns the settlements owed to a run's record, by request
// id. None reads as an empty map.
func Unrecorded(root, projectID, runID string) (map[string]Settlement, error) {
	body, err := os.ReadFile(unrecordedPath(root, projectID, runID))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Settlement{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runcontrol: read %s: %w", unrecordedPath(root, projectID, runID), err)
	}
	m := map[string]Settlement{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrMalformed, unrecordedPath(root, projectID, runID), err)
	}
	return m, nil
}

// KeepUnrecorded saves a terminal settlement whose commit failed, to be
// retried by the next guard holder. Only an owner holding the run's guard
// calls it, so there is no concurrent writer.
func KeepUnrecorded(root, projectID, runID, id string, s Settlement) error {
	if s.State != StateCompleted && s.State != StateFailed {
		return fmt.Errorf("runcontrol: only a final receipt is kept unrecorded, not %q", s.State)
	}
	m, err := Unrecorded(root, projectID, runID)
	if err != nil {
		return err
	}
	m[id] = s
	return writeUnrecorded(root, projectID, runID, m)
}

// dropUnrecorded forgets id once the record carries it settled.
func dropUnrecorded(root, projectID, runID, id string) error {
	m, err := Unrecorded(root, projectID, runID)
	if err != nil {
		return err
	}
	if _, ok := m[id]; !ok {
		return nil
	}
	delete(m, id)
	if len(m) == 0 {
		err := os.Remove(unrecordedPath(root, projectID, runID))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeUnrecorded(root, projectID, runID, m)
}

// Private operation seams allow deterministic preparation/publication failures
// in tests while keeping real temporary files and their private permissions.
var (
	unrecordedWrite  = (*os.File).Write
	unrecordedClose  = (*os.File).Close
	unrecordedRename = os.Rename
)

func writeUnrecorded(root, projectID, runID string, m map[string]Settlement) (err error) {
	path := unrecordedPath(root, projectID, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".unrecorded-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			if cleanupErr := os.Remove(name); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("runcontrol: remove temporary file %s: %w", name, cleanupErr))
			}
		}
	}()
	_, werr := unrecordedWrite(tmp, append(b, '\n'))
	if err := errors.Join(werr, unrecordedClose(tmp)); err != nil {
		return err
	}
	return unrecordedRename(name, path)
}
