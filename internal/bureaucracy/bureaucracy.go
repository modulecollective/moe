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

	"github.com/modulecollective/moe/internal/git"
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
// the marker file and the empty projects/ scaffolding, and stages them.
// If remote is non-empty, it's set as `origin`.
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
	if err := git.Run(abs, "init", "-b", "main"); err != nil {
		return fmt.Errorf("bureaucracy: git init: %w", err)
	}

	if err := os.WriteFile(filepath.Join(abs, Marker), []byte(markerBody), 0o644); err != nil {
		return fmt.Errorf("bureaucracy: write %s: %w", Marker, err)
	}
	// Ignore moe's per-repo scratch dir (locks, session worktrees,
	// sandbox clones). Written up-front rather than lazily so
	// freshly-initialized repos don't surface .moe/ as an untracked
	// path on first `moe run new` or `moe sdlc design`.
	if err := ensureRootGitignore(abs); err != nil {
		return err
	}
	// Git doesn't track empty dirs, so a .gitkeep reserves the path and
	// makes future project registration land in a tracked spot.
	// projects/ is the sole top-level state directory — project.json and
	// runs/ both live under projects/<id>/.
	if err := os.MkdirAll(filepath.Join(abs, "projects"), 0o755); err != nil {
		return fmt.Errorf("bureaucracy: mkdir projects: %w", err)
	}
	if err := os.WriteFile(filepath.Join(abs, "projects", ".gitkeep"), nil, 0o644); err != nil {
		return fmt.Errorf("bureaucracy: write projects/.gitkeep: %w", err)
	}

	if remote != "" {
		// Use `get-url` as a silent probe so the common fresh-init path
		// doesn't spew "No such remote" to the operator's stderr. If origin
		// already exists (git init is idempotent, so we might land on top of
		// a pre-existing repo), repoint it; otherwise add it.
		probe := exec.Command("git", "remote", "get-url", "origin")
		probe.Dir = abs
		if probe.Run() == nil {
			if err := git.Run(abs, "remote", "set-url", "origin", remote); err != nil {
				return fmt.Errorf("bureaucracy: set remote: %w", err)
			}
		} else {
			if err := git.Run(abs, "remote", "add", "origin", remote); err != nil {
				return fmt.Errorf("bureaucracy: set remote: %w", err)
			}
		}
	}

	if err := git.Run(abs, "add", Marker, ".gitignore", "projects/.gitkeep"); err != nil {
		return fmt.Errorf("bureaucracy: git add: %w", err)
	}
	return nil
}

// ensureRootGitignore writes a .gitignore at the bureaucracy root
// listing .moe/ so per-repo scratch state never leaks into git
// history. If a .gitignore already exists, the .moe/ entry is
// appended when missing; otherwise a fresh file is written.
func ensureRootGitignore(root string) error {
	const wanted = ".moe/"
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("bureaucracy: read .gitignore: %w", err)
	}
	body := string(existing)
	if hasGitignoreLine(body, wanted) {
		return nil
	}
	if len(body) > 0 && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += wanted + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("bureaucracy: write .gitignore: %w", err)
	}
	return nil
}

// hasGitignoreLine reports whether body already contains line as a
// standalone, uncommented entry.
func hasGitignoreLine(body, line string) bool {
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

