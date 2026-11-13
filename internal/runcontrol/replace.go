package runcontrol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Private operation seams let tests fail preparation and publication without
// relying on filesystem permissions (which behave differently under root).
var (
	replaceCreateTemp = os.CreateTemp
	replaceWrite      = (*os.File).Write
	replaceClose      = (*os.File).Close
	replaceChmod      = os.Chmod
	replaceRename     = os.Rename
)

// replaceFile publishes a complete generation for lock-free readers, including
// during rollback. The repository lock still serializes writers.
func replaceFile(path string, body []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := replaceCreateTemp(dir, ".control-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if cleanupErr := os.Remove(tmp); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("runcontrol: remove temporary file %s: %w", tmp, cleanupErr))
			}
		}
	}()
	_, writeErr := replaceWrite(f, body)
	closeErr := replaceClose(f)
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := replaceChmod(tmp, 0o644); err != nil {
		return err
	}
	return replaceRename(tmp, path)
}
