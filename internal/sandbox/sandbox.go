// Package sandbox gives every request a private, copy-on-write working
// copy of its project's submodule.
//
// On first `moe work` against a request, Ensure clones
// projects/<project>/ to .moe/clones/<project>/<request>/. Subsequent
// turns for the same request reuse that clone — it is the session's
// workspace for the life of the request. Two requests against the same
// project get two independent clones, so concurrent activities can't
// step on each other's working tree, index, or refs.
//
// On macOS the clone uses APFS clonefile(2): instant, no data copied,
// blocks shared with the source until either side writes. Other
// platforms fall back to a recursive copy. The public API is the same
// either way.
//
// Submodules (the common case) store their git data outside the working
// tree, reached via a .git gitfile pointer. Ensure handles that by also
// cloning the real gitdir to a sibling path under .moe/ and fixing up
// both the gitfile and core.worktree in the clone so git commands run
// against the clone's own refs/objects — the canonical submodule is
// never written to from a sandboxed session.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Path returns the clone directory for this request, whether or not it
// currently exists.
func Path(root, projectID, requestID string) string {
	return filepath.Join(root, ".moe", "clones", projectID, requestID)
}

// gitDirPath returns the clone-local gitdir used when the source submodule
// points its .git at an external directory (standard git-submodule layout).
func gitDirPath(root, projectID, requestID string) string {
	return filepath.Join(root, ".moe", "clones", projectID, ".git-modules", requestID)
}

// Ensure makes sure the clone for (projectID, requestID) exists and
// returns its absolute path. First call clones projects/<projectID>/;
// subsequent calls are a stat.
func Ensure(root, projectID, requestID string) (string, error) {
	src := filepath.Join(root, "projects", projectID)
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("sandbox: source submodule %s: %w", src, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: source submodule %s is not a directory", src)
	}

	dst := Path(root, projectID, requestID)
	if _, err := os.Stat(dst); err == nil {
		return absPath(dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("sandbox: stat %s: %w", dst, err)
	}

	if err := ensureGitignore(root); err != nil {
		return "", err
	}

	srcGitDir, gitIsFile, err := resolveGitDir(src)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("sandbox: mkdir %s: %w", filepath.Dir(dst), err)
	}
	if err := Clone(src, dst); err != nil {
		return "", fmt.Errorf("sandbox: clone working tree: %w", err)
	}

	if gitIsFile {
		if err := cloneGitDir(dst, srcGitDir, root, projectID, requestID); err != nil {
			// Roll back the worktree clone so Ensure is retryable.
			_ = os.RemoveAll(dst)
			return "", err
		}
	}

	return absPath(dst)
}

// cloneGitDir clones the submodule's real gitdir alongside the worktree
// clone and repoints the clone's gitfile and core.worktree at it.
func cloneGitDir(worktreeClone, srcGitDir, root, projectID, requestID string) error {
	dstGitDir := gitDirPath(root, projectID, requestID)
	if err := os.MkdirAll(filepath.Dir(dstGitDir), 0o755); err != nil {
		return fmt.Errorf("sandbox: mkdir %s: %w", filepath.Dir(dstGitDir), err)
	}
	if err := Clone(srcGitDir, dstGitDir); err != nil {
		return fmt.Errorf("sandbox: clone gitdir: %w", err)
	}
	rel, err := filepath.Rel(worktreeClone, dstGitDir)
	if err != nil {
		_ = os.RemoveAll(dstGitDir)
		return fmt.Errorf("sandbox: relpath gitdir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(worktreeClone, ".git"), []byte("gitdir: "+rel+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(dstGitDir)
		return fmt.Errorf("sandbox: write gitfile: %w", err)
	}
	abs, err := absPath(worktreeClone)
	if err != nil {
		_ = os.RemoveAll(dstGitDir)
		return err
	}
	// The submodule's gitdir config has core.worktree pointing at the
	// canonical working tree. Repoint it at the clone so git commands
	// run inside the clone treat the clone's files as the worktree.
	//
	// We use "git config -f" to edit the cloned gitdir's config file
	// directly. Using "git -C" would fail because git tries to resolve
	// the old core.worktree (a relative path baked into the source
	// gitdir) before writing the new value, and that relative path is
	// invalid from the clone's new location.
	cfgPath := filepath.Join(dstGitDir, "config")
	cmd := exec.Command("git", "config", "-f", cfgPath, "core.worktree", abs)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dstGitDir)
		return fmt.Errorf("sandbox: set core.worktree: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Remove tears down the clone and its sibling gitdir, if any. Idempotent.
func Remove(root, projectID, requestID string) error {
	var firstErr error
	for _, p := range []string{
		Path(root, projectID, requestID),
		gitDirPath(root, projectID, requestID),
	} {
		if err := os.RemoveAll(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Exists reports whether a clone directory currently exists for this request.
// A truthy return means Ensure would be a no-op.
func Exists(root, projectID, requestID string) bool {
	_, err := os.Stat(Path(root, projectID, requestID))
	return err == nil
}

// resolveGitDir returns the absolute gitdir for a working tree and whether
// the working tree's .git is a gitfile pointer (true) vs a real directory
// (false). Errors if .git is missing.
func resolveGitDir(worktree string) (string, bool, error) {
	p := filepath.Join(worktree, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: stat %s: %w", p, err)
	}
	if info.IsDir() {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", false, fmt.Errorf("sandbox: resolve %s: %w", p, err)
		}
		return abs, false, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: read %s: %w", p, err)
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", false, fmt.Errorf("sandbox: %s is not a gitfile (no 'gitdir:' prefix)", p)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktree, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", true, fmt.Errorf("sandbox: resolve gitdir %s: %w", target, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", true, fmt.Errorf("sandbox: gitfile target %s: %w", abs, err)
	}
	return abs, true, nil
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s: %w", p, err)
	}
	return abs, nil
}

// ensureGitignore drops a `*` .gitignore under .moe/ so clones, locks,
// and other transient artifacts never leak into the bureaucracy's git
// history. Writing it lazily (on first Ensure) keeps bureaucracy.Init
// uncoupled from sandbox internals.
func ensureGitignore(root string) error {
	dir := filepath.Join(root, ".moe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("sandbox: mkdir %s: %w", dir, err)
	}
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: stat %s: %w", p, err)
	}
	return os.WriteFile(p, []byte("*\n"), 0o644)
}
