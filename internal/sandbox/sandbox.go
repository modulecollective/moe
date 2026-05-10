// Package sandbox gives every run a private working tree of its
// project's submodule, implemented as a `git worktree` linked off the
// canonical submodule.
//
// On first `moe sdlc code` against a run, Ensure registers a worktree
// at .moe/clones/<project>/<run>/, pointing at the same gitdir as the
// canonical submodule but with its own working tree, index, and HEAD.
// Subsequent turns reuse that worktree — it is the session's workspace
// for the life of the run. Two runs against the same project get two
// independent worktrees, so concurrent activities can't step on each
// other's working tree or index. Branch isolation comes from the
// per-run `moe/<run-id>` branch the CheckoutBranch step creates.
//
// First-touch on a fresh machine: if `<root>/projects/<project>/src/`
// is an empty submodule mountpoint and the bureaucracy's .gitmodules
// declares its url, Ensure runs `git submodule update --init` for that
// one submodule before adding the worktree. A failed init surfaces a
// SubmoduleInitError carrying the verbatim retry command, so the
// operator can copy it into a shell once the underlying issue (auth,
// network, URL) is resolved.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

// SubmoduleInitError is returned by Ensure / EnsureAt when the source
// submodule mount-point exists but is not initialised on this machine
// and the auto-init shell-out failed. The CLI catches it via errors.As
// to print a retry hint that the operator can paste into a shell.
type SubmoduleInitError struct {
	Root      string
	ProjectID string
	Output    string
	Err       error
}

func (e *SubmoduleInitError) Error() string {
	cmd := "git submodule update --init projects/" + e.ProjectID + "/src"
	msg := fmt.Sprintf(
		"sandbox: project %q submodule could not be initialised.\n"+
			"  %s\n"+
			"  failed: %v",
		e.ProjectID, cmd, e.Err)
	if trimmed := strings.TrimSpace(e.Output); trimmed != "" {
		msg += "\n  output:\n" + indent(trimmed, "    ")
	}
	msg += fmt.Sprintf("\nRun that command manually in %s once the underlying issue is resolved, then retry.", e.Root)
	return msg
}

func (e *SubmoduleInitError) Unwrap() error { return e.Err }

// Path returns the worktree directory for this run, whether or not it
// currently exists.
func Path(root, projectID, runID string) string {
	return filepath.Join(root, ".moe", "clones", projectID, runID)
}

// Ensure makes sure the worktree for (projectID, runID) exists and
// returns its absolute path. First call registers a worktree off
// projects/<projectID>/src; subsequent calls are a no-op when the
// worktree is already registered against the same path.
func Ensure(root, projectID, runID string) (string, error) {
	return EnsureAt(root, projectID, Path(root, projectID, runID))
}

// EnsureAt is the path-parameterised version of Ensure: it adds a
// worktree for the project's submodule at dst. Used by the workspace
// package so named workspaces share the same primitive without
// duplicating the auto-init pre-flight or the git-worktree-add call.
func EnsureAt(root, projectID, dst string) (string, error) {
	src := filepath.Join(root, "projects", projectID, "src")

	if err := autoInitSubmodule(root, projectID, src); err != nil {
		return "", err
	}

	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("sandbox: source submodule %s: %w", src, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: source submodule %s is not a directory", src)
	}

	absDst, err := filepath.Abs(dst)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s: %w", dst, err)
	}

	registered, err := worktreeRegisteredAt(src, absDst)
	if err != nil {
		return "", err
	}
	if registered {
		return absDst, nil
	}
	if _, err := os.Stat(absDst); err == nil {
		return "", fmt.Errorf(
			"sandbox: %s exists but is not a registered worktree of %s; "+
				"remove it manually before retrying", absDst, src)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("sandbox: stat %s: %w", absDst, err)
	}

	if err := ensureGitignore(root); err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
		return "", fmt.Errorf("sandbox: mkdir %s: %w", filepath.Dir(absDst), err)
	}

	if out, err := git.Combined(src, "worktree", "add", "--detach", absDst, "HEAD"); err != nil {
		return "", fmt.Errorf("sandbox: git worktree add %s: %w (%s)", absDst, err, out)
	}

	return absDst, nil
}

// CheckoutBranch makes branch the current HEAD in the worktree at
// clonePath, creating it off HEAD if it doesn't already exist. Used by
// the stage session setup so `moe code` lands the agent on
// moe/<run-id> without the agent having to remember the incantation.
func CheckoutBranch(clonePath, branch string) error {
	// -B creates the branch if missing and resets it to HEAD if it
	// exists, which is wrong for re-runs. Use a check-then-switch
	// instead so an existing branch is resumed, not reset.
	if _, err := git.Output(clonePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if out, err := git.Combined(clonePath, "checkout", branch); err != nil {
			return fmt.Errorf("sandbox: checkout %s: %w (%s)", branch, err, out)
		}
		return nil
	}
	if out, err := git.Combined(clonePath, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("sandbox: checkout -b %s: %w (%s)", branch, err, out)
	}
	return nil
}

// Remove tears down the worktree registered at the run's sandbox path.
// Idempotent: missing canonical, missing worktree registration, and
// missing directory are all no-ops.
//
// `git worktree remove --force` is the right call: by the time Remove
// runs, the run's terminal status has been written, so any uncommitted
// state in the worktree is intentionally being discarded. The matching
// invariant is the one inherited from the worktree-bug fix on the
// session-close path. A trailing `git worktree prune` cleans up any
// stale registration left under the canonical's .git/worktrees/.
func Remove(root, projectID, runID string) error {
	dst := Path(root, projectID, runID)
	src := filepath.Join(root, "projects", projectID, "src")

	if _, err := os.Stat(src); err == nil {
		// Tolerate "not a working tree" — the worktree may never have
		// been registered (run closed before code stage).
		_, _ = git.Combined(src, "worktree", "remove", "--force", dst)
		_, _ = git.Combined(src, "worktree", "prune")
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("sandbox: remove %s: %w", dst, err)
	}
	return nil
}

// Exists reports whether a sandbox directory currently exists for this
// run. A truthy return means Ensure would be a no-op.
func Exists(root, projectID, runID string) bool {
	_, err := os.Stat(Path(root, projectID, runID))
	return err == nil
}

// autoInitSubmodule materialises the project's submodule when src is
// the empty mount-point left by an uncloned `git submodule add`.
// Returns nil when nothing needs doing (src missing, src non-empty,
// submodule not declared). On failure returns *SubmoduleInitError so
// callers can format the actionable retry message via errors.As.
func autoInitSubmodule(root, projectID, src string) error {
	info, err := os.Stat(src)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("sandbox: read %s: %w", src, err)
	}
	if len(entries) > 0 {
		return nil
	}

	declared, err := gitmodulesDeclares(filepath.Join(root, ".gitmodules"), "projects/"+projectID+"/src")
	if err != nil {
		return err
	}
	if !declared {
		return nil
	}

	fmt.Fprintf(os.Stderr, "sandbox: initialising submodule projects/%s/src ...\n", projectID)
	out, err := git.Combined(root, "submodule", "update", "--init", "projects/"+projectID+"/src")
	if err != nil {
		return &SubmoduleInitError{
			Root:      root,
			ProjectID: projectID,
			Output:    out,
			Err:       err,
		}
	}
	if strings.TrimSpace(out) != "" {
		fmt.Fprintln(os.Stderr, out)
	}
	return nil
}

// gitmodulesDeclares parses .gitmodules looking for `path = want`.
// Returns false (no error) when .gitmodules is missing — that's the
// "not a bureaucracy / no submodules declared" shape and the auto-init
// pre-flight should silently skip in that case.
func gitmodulesDeclares(path, want string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sandbox: read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "path") {
			continue
		}
		_, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(val) == want {
			return true, nil
		}
	}
	return false, nil
}

// worktreeRegisteredAt returns true when canonicalSrc has a worktree
// registered at absDst. Used by EnsureAt to make registration
// idempotent (the equivalent of the old os.Stat short-circuit).
func worktreeRegisteredAt(canonicalSrc, absDst string) (bool, error) {
	out, err := git.Output(canonicalSrc, "worktree", "list", "--porcelain")
	if err != nil {
		// Canonical isn't a git repo (e.g. test fixture for the
		// missing-source path) or git isn't reachable; let the caller's
		// next step produce the diagnostic.
		return false, nil
	}
	target := canonical(absDst)
	for _, line := range strings.Split(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if canonical(path) == target {
			return true, nil
		}
	}
	return false, nil
}

// canonical resolves p as far as it can — symlinks first, abs as a
// fallback — so the equality check in worktreeRegisteredAt isn't
// thrown by /tmp -> /private/tmp on macOS or other symlink prefixes.
func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// ensureGitignore drops a `*` .gitignore under .moe/ so worktrees,
// locks, and other transient artifacts never leak into the
// bureaucracy's git history. Writing it lazily (on first Ensure) keeps
// bureaucracy.Init uncoupled from sandbox internals.
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
