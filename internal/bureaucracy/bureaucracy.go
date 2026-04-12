// Package bureaucracy locates the bureaucracy repo that moe operates on.
//
// A bureaucracy root is any directory containing a bureaucracy.conf file.
// Discovery checks $MOE_HOME first, then walks up from the current working
// directory. The CLI and the bureaucracy live in separate repos (see
// README §"Two Repos: CLI and Bureaucracy"), so the binary has no compiled-in
// pointer to any particular bureaucracy.
package bureaucracy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Marker is the sentinel filename that identifies a bureaucracy repo root.
const Marker = "bureaucracy.conf"

// EnvHome is the environment variable that pins the bureaucracy root,
// bypassing the $PWD walk.
const EnvHome = "MOE_HOME"

// ErrNotFound is returned when no bureaucracy root is discoverable.
var ErrNotFound = errors.New("bureaucracy: no " + Marker + " found in $MOE_HOME or any parent of $PWD")

// Find returns the absolute path to the bureaucracy root, or ErrNotFound.
//
// Resolution order:
//  1. $MOE_HOME, if set — must contain Marker or the call errors.
//  2. Walk up from startDir looking for Marker.
func Find(startDir string, getenv func(string) string) (string, error) {
	if home := getenv(EnvHome); home != "" {
		abs, err := filepath.Abs(home)
		if err != nil {
			return "", fmt.Errorf("bureaucracy: resolve %s=%q: %w", EnvHome, home, err)
		}
		if _, err := os.Stat(filepath.Join(abs, Marker)); err != nil {
			return "", fmt.Errorf("bureaucracy: %s=%q has no %s: %w", EnvHome, home, Marker, err)
		}
		return abs, nil
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("bureaucracy: resolve start dir: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, Marker)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}
