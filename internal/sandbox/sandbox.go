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
// First-touch on a fresh machine: project.EnsureMaterialized owns the
// auto-init pre-flight — if `<root>/projects/<project>/src/` is an
// empty submodule mountpoint and the bureaucracy's .gitmodules
// declares its url, it runs `git submodule update --init --recursive`
// for that one submodule. A failed init surfaces a
// project.SubmoduleInitError carrying the verbatim retry command, so
// the operator can copy it into a shell once the underlying issue
// (auth, network, URL) is resolved.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
)

// SubmoduleInitError is the typed error EnsureAt forwards when the
// project's submodule could not be initialised. Aliased to
// project.SubmoduleInitError so existing callers / tests that do
// `errors.As(err, &sandbox.SubmoduleInitError{})` keep working after
// the materialisation gate moved to internal/project.
type SubmoduleInitError = project.SubmoduleInitError

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

	if err := project.EnsureMaterialized(root, projectID); err != nil {
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
	// `.git/` directory, treat it as already cloned and skip the
	// fresh-clone work. The exclude reconciliation below still runs,
	// so pre-existing clones get the `.moe/` ignore line backfilled
	// on their next stage open without a separate migration step.
	if !cloneAlreadyAt(absDst) {
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
	}

	if err := ensureCloneExclude(absDst); err != nil {
		return "", err
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
//
// A permission denial inside the clone typically means a container
// running as root wrote files there (rootless docker maps container-
// root → host `nobody:nogroup`, which the moe process can't unlink).
// We surface that likely cause and point at `moe clone gc`, the verb
// that knows how to fall back to a container-driven rm. The wrapped
// error keeps the path / syscall detail intact for anyone matching on
// it with errors.Is.
func Remove(root, projectID, runID string) error {
	dst := Path(root, projectID, runID)
	if err := os.RemoveAll(dst); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf(
				"sandbox: remove %s: %w (likely a container-written file; "+
					"the project's dev-env-teardown.d should remove it, "+
					"or run `moe clone gc`)", dst, err)
		}
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

// cloneAlreadyAt reports whether absDst is a usable existing clone —
// the destination has a `.git/` directory. EnsureAt's idempotency
// check: a fresh `git clone` would refuse to run against a non-empty
// destination, so we short-circuit on a previously-materialised clone.
// A `.git` gitfile (left over from a legacy worktree-shaped sandbox)
// is rejected by the caller so the operator notices instead of the
// new code silently reusing the wrong shape.
func cloneAlreadyAt(absDst string) bool {
	info, err := os.Stat(filepath.Join(absDst, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ensureCloneExclude appends `.moe/` to the clone's
// `.git/info/exclude` if not already present. The clone's own `.moe/`
// is harness-private (the dev-env cache today, anything the harness
// drops here in the future) and must not gate the push pre-flight or
// contaminate the operator's local `git status`. The clone's git
// layer is the right boundary for these artifacts — putting the
// pattern in the project's tracked `.gitignore` would bake a harness
// implementation detail into every project that ever runs through
// MoE. Whole-directory pattern, not just `dev-env.env`, so future
// clone-local harness artifacts inherit the right treatment.
func ensureCloneExclude(clonePath string) error {
	p := filepath.Join(clonePath, ".git", "info", "exclude")
	existing, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: read %s: %w", p, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == ".moe/" {
			return nil
		}
	}
	sep := ""
	if n := len(existing); n > 0 && existing[n-1] != '\n' {
		sep = "\n"
	}
	out := append([]byte(nil), existing...)
	out = append(out, sep...)
	out = append(out, ".moe/\n"...)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	return nil
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
	// Atomic tmp+rename, not a bare WriteFile: O_TRUNC leaves a window
	// where the ignore file exists but is empty, and concurrent
	// first-touch of a fresh `.moe/` would race it into momentarily
	// un-ignoring the tree. Racers write identical content, so
	// last-writer-wins rename is idempotent. (Mirrors repolock's copy.)
	tmp, err := os.CreateTemp(dir, ".gitignore.tmp.*")
	if err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write([]byte("*\n")); err != nil {
		tmp.Close()
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	return nil
}
