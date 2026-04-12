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
	"os/exec"
	"path/filepath"
	"strings"
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

// markerBody is the seed contents of bureaucracy.conf on fresh init.
// The file's existence is the load-bearing signal; the body is a note for
// a human opening it.
const markerBody = `# Bureaucracy root marker. Presence of this file identifies the directory as
# a MoE bureaucracy repo. Keys below are reserved for future config; for now
# the file's existence is the whole signal.
`

// Init scaffolds a bureaucracy repo at dir: initializes git if needed, writes
// the marker file and the empty projects/ and requests/ scaffolding, and
// stages them. If remote is non-empty, it's set as `origin`.
//
// Init stops short of committing so the operator can inspect the initial
// state (add a .gitignore, tweak the marker file, etc.) before the first
// commit. Later mutations (add-project, remove-project) do commit on their
// own — they're small and routine.
//
// Refuses if bureaucracy.conf already exists — init is one-shot.
func Init(dir, remote string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("bureaucracy: resolve %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("bureaucracy: mkdir %s: %w", abs, err)
	}
	if _, err := os.Stat(filepath.Join(abs, Marker)); err == nil {
		return fmt.Errorf("bureaucracy: %s already exists in %s", Marker, abs)
	}

	// `git init` is idempotent on an existing repo, so just run it — it
	// covers both "empty dir" and "pre-existing git repo" in one call.
	if err := runGit(abs, "init", "-b", "main"); err != nil {
		return fmt.Errorf("bureaucracy: git init: %w", err)
	}

	if err := os.WriteFile(filepath.Join(abs, Marker), []byte(markerBody), 0o644); err != nil {
		return fmt.Errorf("bureaucracy: write %s: %w", Marker, err)
	}
	// Git doesn't track empty dirs, so a .gitkeep reserves the path and
	// makes future submodule-add / request-creation land in a tracked spot.
	for _, sub := range []string{"projects", "requests"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o755); err != nil {
			return fmt.Errorf("bureaucracy: mkdir %s: %w", sub, err)
		}
		if err := os.WriteFile(filepath.Join(abs, sub, ".gitkeep"), nil, 0o644); err != nil {
			return fmt.Errorf("bureaucracy: write %s/.gitkeep: %w", sub, err)
		}
	}

	if remote != "" {
		// `remote add` fails if origin already exists; set-url handles both.
		if err := runGit(abs, "remote", "remove", "origin"); err != nil {
			// Ignore — remote probably didn't exist.
		}
		if err := runGit(abs, "remote", "add", "origin", remote); err != nil {
			return fmt.Errorf("bureaucracy: set remote: %w", err)
		}
	}

	if err := runGit(abs, "add", Marker, "projects/.gitkeep", "requests/.gitkeep"); err != nil {
		return fmt.Errorf("bureaucracy: git add: %w", err)
	}
	return nil
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
