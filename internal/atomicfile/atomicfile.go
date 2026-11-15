// Package atomicfile publishes whole files by writing a temporary sibling
// and renaming it over the destination, so a reader never opens a
// truncated or half-written generation.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Private operation seams let tests fail preparation and publication without
// relying on filesystem permissions (which behave differently under root).
var (
	createTemp = os.CreateTemp
	write      = (*os.File).Write
	closeFile  = (*os.File).Close
	chmod      = os.Chmod
	rename     = os.Rename
)

// Write publishes body at path with mode perm. The parent directory is
// created if missing; the temporary file lives beside path so the rename
// stays on one filesystem. On failure the temporary file is removed and a
// failed removal is joined to the returned error. Concurrent writers are
// last-writer-wins; callers that need more serialize themselves.
func Write(path string, body []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := createTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if cleanupErr := os.Remove(tmp); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("atomicfile: remove temporary file %s: %w", tmp, cleanupErr))
			}
		}
	}()
	_, writeErr := write(f, body)
	closeErr := closeFile(f)
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := chmod(tmp, perm); err != nil {
		return err
	}
	return rename(tmp, path)
}
