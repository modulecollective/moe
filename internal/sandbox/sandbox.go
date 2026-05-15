// Package sandbox gives every run a private working tree of its
// project's submodule, implemented as an object-shared `git clone`
// against the canonical submodule.
//
// On first `moe sdlc code` against a run, Ensure runs `git clone
// --local --shared --no-checkout` from `projects/<project>/src/` into
// `.moe/clones/<project>/<run>/` and checks out HEAD. The resulting
// clone has its own `.git/` directory (not a worktree gitfile pointing
// into `<bureaucracy>/.git/modules/...`); objects are hardlinked or
// referenced via `objects/info/alternates` so there is no network
// fetch and disk cost is near-zero. Two runs against the same project
// get two independent clones, so concurrent activities can't step on
// each other's working tree, index, or refs. Branch isolation comes
// from the per-run `moe/<run-id>` branch the CheckoutBranch step
// creates in the clone's own ref-db.
//
// The previous primitive — `git worktree add` linked off the canonical
// submodule — was switched out because codex's `apply_patch` enforces
// a project-scope check on top of `--add-dir`: writes that cross the
// moe-submodule ↔ bureaucracy gitdir boundary were silently rejected,
// regardless of which side held cwd. A plain object-shared clone has
// no such boundary (its `.git` is a regular directory, not a gitfile
// into `.git/modules/...`), so the agent can write across cwd /
// add-dir freely.
//
// First-touch on a fresh machine: if `<root>/projects/<project>/src/`
// is an empty submodule mountpoint and the bureaucracy's .gitmodules
// declares its url, Ensure runs `git submodule update --init` for that
// one submodule before cloning. A failed init surfaces a
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

// Path returns the clone directory for this run, whether or not it
// currently exists.
func Path(root, projectID, runID string) string {
	return filepath.Join(root, ".moe", "clones", projectID, runID)
}

// Ensure makes sure the clone for (projectID, runID) exists and
// returns its absolute path. First call clones `projects/<projectID>/src`
// into the per-run path; subsequent calls are a no-op when the clone
// already exists at the same path.
func Ensure(root, projectID, runID string) (string, error) {
	return EnsureAt(root, projectID, Path(root, projectID, runID))
}

// EnsureAt is the path-parameterised version of Ensure: it materialises
// an object-shared clone of the project's submodule at dst. Used by
// the workspace package so named workspaces share the same primitive
// without duplicating the auto-init pre-flight or the git-clone
// invocation.
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

	// Idempotency: if the destination already exists with a usable
	// `.git/` directory, treat it as already cloned.
	if cloneAlreadyAt(absDst) {
		return absDst, nil
	}
	if _, err := os.Stat(absDst); err == nil {
		return "", fmt.Errorf(
			"sandbox: %s exists but is not a usable clone of %s; "+
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

	// `git clone --local --shared --no-checkout` against the submodule
	// path: hardlinks objects from the canonical, sets up
	// `objects/info/alternates` for shared object access, no network.
	// `--no-checkout` defers the working-tree population to the
	// explicit checkout below so we can route any post-clone branch
	// dance through CheckoutBranch's idempotent path.
	if out, err := git.Combined("", "clone", "--local", "--shared", "--no-checkout", src, absDst); err != nil {
		return "", fmt.Errorf("sandbox: git clone %s → %s: %w (%s)", src, absDst, err, out)
	}
	if out, err := git.Combined(absDst, "checkout", "HEAD"); err != nil {
		// Best-effort cleanup so a half-cloned dir doesn't poison the
		// next attempt.
		_ = os.RemoveAll(absDst)
		return "", fmt.Errorf("sandbox: git checkout HEAD in %s: %w (%s)", absDst, err, out)
	}

	return absDst, nil
}

// CheckoutBranch makes branch the current HEAD in the clone at
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

// Remove tears down the clone at the run's sandbox path. Idempotent:
// a missing directory is a no-op.
//
// The plain-clone primitive has no canonical-side registration to
// clean up (unlike the former worktree primitive), so removal is just
// `os.RemoveAll`. The clone's `objects/info/alternates` is a one-way
// reference; nuking the clone has no effect on the canonical
// submodule's object store.
func Remove(root, projectID, runID string) error {
	dst := Path(root, projectID, runID)
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

// cloneAlreadyAt reports whether absDst is a usable existing clone —
// the directory exists and contains a `.git/` directory or gitfile.
// EnsureAt's idempotency check: a fresh `git clone` would refuse to
// run against a non-empty destination, so we short-circuit on a
// previously-materialised clone.
func cloneAlreadyAt(absDst string) bool {
	if _, err := os.Stat(filepath.Join(absDst, ".git")); err == nil {
		return true
	}
	return false
}

// canonical resolves p as far as it can — symlinks first, abs as a
// fallback — so equality checks in tests aren't thrown by
// /tmp -> /private/tmp on macOS or other symlink prefixes.
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
