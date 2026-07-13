// Package workspace owns named per-project workspaces — reusable
// working trees of a project's submodule that successive runs check
// their branch out into. Unlike per-run sandboxes (internal/sandbox),
// a named workspace persists across runs so working-tree state (build
// cache, node_modules, a running dev server) survives the branch
// switch.
//
// Lifecycle:
//
//   - Lazily created on first use, by either `moe sdlc new
//     --workspace <name>` or `moe workspace shell <project> <name>`.
//     The working tree reuses sandbox.EnsureAt — an object-shared
//     `git clone --local --shared` of the canonical submodule, with
//     the auto-init pre-flight for fresh checkouts.
//   - Claimed by the run that's currently using it. The claim file
//     (.moe/claim.json inside the workspace dir) names the holding
//     run; a second run that names the same workspace while it's
//     claimed is refused at sdlc-new time with a pointer to the
//     holder.
//   - Branch handoff creates the new run's branch off the project's
//     default-branch tip, so it isn't anchored to the previous run's
//     tip. Refuses if the working tree is dirty, same fail-loud
//     invariant as the rest of the engine.
//   - Released at terminal status (close / merge / sync-finalize).
//     The directory stays — the next run reuses it.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/sandbox"
)

// namePattern bounds workspace names to the same lower-kebab shape
// run/project IDs use, so a workspace name can never break out of its
// parent dir or collide with shell metacharacters when echoed in
// errors. The pattern is stricter than strictly necessary (no slashes,
// no dots) on purpose: a workspace name lands in a path the operator
// will type and read repeatedly, and the cheapest way to keep it
// honest is to refuse anything that doesn't look like a slug.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Path returns the workspace directory for (root, projectID, name).
// The path is returned whether or not it currently exists.
func Path(root, projectID, name string) string {
	return filepath.Join(root, ".moe", "named", projectID, name)
}

// claimPath is where a workspace's current owner is recorded. The file
// lives under `.moe/` inside the workspace dir so it shares the
// `.git/info/exclude` umbrella with the rest of moe's per-workspace
// artifacts (dev-env.env, etc.) and doesn't show up as untracked in
// `git status`. A workspace that doesn't exist on disk is, by
// definition, unclaimed.
func claimPath(workspacePath string) string {
	return filepath.Join(workspacePath, ".moe", "claim.json")
}

// legacyClaimPath is the pre-migration claim location, at the
// workspace root. Read-only fallback used by readClaim to migrate
// existing workspaces forward without operator intervention; also
// cleaned by Release so a re-acquire doesn't resurrect it.
func legacyClaimPath(workspacePath string) string {
	return filepath.Join(workspacePath, "claim.json")
}

// ValidateName reports whether name is a usable workspace identifier.
// Exposed so callers can reject bad input before doing any work
// (writing project state, opening a run).
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("workspace: name is required")
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("workspace: name %q must match %s", name, namePattern)
	}
	return nil
}

// Exists reports whether a workspace directory currently exists for
// (projectID, name).
func Exists(root, projectID, name string) bool {
	_, err := os.Stat(Path(root, projectID, name))
	return err == nil
}

// Claim is the on-disk record of which run currently owns a workspace.
// Rendered in errors and read by ReadClaim; written by Acquire.
type Claim struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	// Run is "<projectID>/<runID>" — same shape repolock uses for its
	// Run field, so error messages can quote it directly.
	Run string `json:"run"`
}

// ErrAlreadyClaimed is returned by Acquire when the workspace is
// already claimed by a different run. Wrapped with the holding claim's
// details so callers can render a useful message and (eventually) so
// tests can errors.As on the condition.
var ErrAlreadyClaimed = errors.New("workspace: already claimed")

// AlreadyClaimedError carries the conflicting claim alongside
// ErrAlreadyClaimed. Returned by Acquire.
type AlreadyClaimedError struct {
	Holder Claim
}

func (e *AlreadyClaimedError) Error() string {
	return fmt.Sprintf("workspace %q for project %q is claimed by run %s",
		e.Holder.Name, e.Holder.Project, e.Holder.Run)
}

func (e *AlreadyClaimedError) Unwrap() error { return ErrAlreadyClaimed }

// ReadClaim returns the workspace's current claim, or (nil, nil) if no
// workspace exists or if the workspace exists but carries no claim
// (e.g. created via `moe workspace shell`).
func ReadClaim(root, projectID, name string) (*Claim, error) {
	wp := Path(root, projectID, name)
	if _, err := os.Stat(wp); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("workspace: stat %s: %w", wp, err)
	}
	return readClaim(wp)
}

// readClaim is the workspace-path-keyed sibling of ReadClaim, used
// internally where the workspace path has already been resolved.
//
// If the new-layout claim is missing but a legacy `<wp>/claim.json`
// exists, the claim is migrated forward (rewritten under `.moe/` and
// the old file removed) before returning. After one read every
// workspace is on the new layout; no compat shim survives past that.
func readClaim(workspacePath string) (*Claim, error) {
	b, err := os.ReadFile(claimPath(workspacePath))
	if errors.Is(err, os.ErrNotExist) {
		return migrateLegacyClaim(workspacePath)
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read claim: %w", err)
	}
	var c Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("workspace: parse claim: %w", err)
	}
	return &c, nil
}

// migrateLegacyClaim reads `<wp>/claim.json` (the pre-`.moe/` layout),
// rewrites it under `<wp>/.moe/claim.json`, and removes the old file.
// Returns (nil, nil) if the legacy file isn't there either.
func migrateLegacyClaim(workspacePath string) (*Claim, error) {
	legacy := legacyClaimPath(workspacePath)
	b, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read claim: %w", err)
	}
	var c Claim
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("workspace: parse claim: %w", err)
	}
	if err := writeClaim(workspacePath, c); err != nil {
		return nil, err
	}
	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("workspace: remove legacy claim: %w", err)
	}
	return &c, nil
}

// Acquire ensures the workspace exists and claims it for runRef
// ("<project>/<runID>"). A workspace already claimed by a different
// run is refused with *AlreadyClaimedError; one already claimed by
// runRef is a no-op (re-acquire is idempotent — second `sdlc code`
// turn against the same run reaches this path).
//
// Acquire does NOT switch branches or touch the working tree — that is
// Attach's job. Splitting them lets `moe workspace shell` create the
// workspace without needing a run to claim it (Acquire isn't called
// in that path) while still presenting one entry point per concern.
func Acquire(root, projectID, name, runRef string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	wp, err := Ensure(root, projectID, name)
	if err != nil {
		return "", err
	}
	existing, err := readClaim(wp)
	if err != nil {
		return "", err
	}
	if existing != nil && existing.Run != runRef {
		return "", &AlreadyClaimedError{Holder: *existing}
	}
	c := Claim{Project: projectID, Name: name, Run: runRef}
	if err := writeClaim(wp, c); err != nil {
		return "", err
	}
	return wp, nil
}

// Info is a per-workspace row, populated by List. Carries everything
// the operator-visible `moe workspace list` table needs without making
// the caller re-issue git probes per row.
type Info struct {
	Project      string
	Name         string
	Path         string
	Branch       string // current symbolic ref, or "" / "HEAD" on detached
	Claim        string // claim.Run, or "" when unclaimed
	Dirty        bool   // any uncommitted change or untracked file
	DevEnvCached bool   // <wp>/.moe/dev-env.env exists
}

// List enumerates named workspaces, optionally filtered to a single
// projectID. With projectID == "", every project's workspaces under
// .moe/named/*/ are returned. Results are sorted by (Project, Name)
// for stable output.
//
// Stale entries (a workspace dir whose parent project no longer has a
// .moe/named/<project>/ entry) and entries that aren't directories
// are silently skipped. The new precondition on `project remove`
// makes orphans unreachable from the happy path; the silent-skip is
// just defence against hand-edits or migration from before that
// guard landed.
func List(root, projectID string) ([]Info, error) {
	base := filepath.Join(root, ".moe", "named")
	var projects []string
	if projectID != "" {
		projects = []string{projectID}
	} else {
		entries, err := os.ReadDir(base)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("workspace: read %s: %w", base, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			projects = append(projects, e.Name())
		}
	}
	var out []Info
	for _, p := range projects {
		dir := filepath.Join(base, p)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("workspace: read %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if err := ValidateName(name); err != nil {
				continue
			}
			info, err := describe(root, p, name)
			if err != nil {
				return nil, err
			}
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// describe builds an Info for one workspace by probing its on-disk
// state. Errors propagate so List surfaces a real filesystem problem
// rather than silently dropping the row.
func describe(root, projectID, name string) (Info, error) {
	wp := Path(root, projectID, name)
	info := Info{Project: projectID, Name: name, Path: wp}

	branch, err := git.Output(wp, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		info.Branch = strings.TrimSpace(branch)
	}
	claim, err := readClaim(wp)
	if err != nil {
		return info, err
	}
	if claim != nil {
		info.Claim = claim.Run
	}
	entries, err := git.Status(wp)
	if err == nil && len(entries) > 0 {
		info.Dirty = true
	}
	if _, err := os.Stat(filepath.Join(wp, ".moe", "dev-env.env")); err == nil {
		info.DevEnvCached = true
	}
	return info, nil
}

// Remove deletes the workspace directory after refusing if a claim
// exists. The dev-env teardown is the CLI's responsibility — this
// primitive only owns the claim check and the dir removal, so tests
// of the workspace package don't need to grow a dev-env fixture.
//
// Missing workspace is a no-op (nil). A workspace that carries a
// claim is refused with *AlreadyClaimedError so callers can branch
// on the same error type Acquire returns.
func Remove(root, projectID, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	wp := Path(root, projectID, name)
	if _, err := os.Stat(wp); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("workspace: stat %s: %w", wp, err)
	}
	claim, err := readClaim(wp)
	if err != nil {
		return err
	}
	if claim != nil {
		return &AlreadyClaimedError{Holder: *claim}
	}
	if err := os.RemoveAll(wp); err != nil {
		return fmt.Errorf("workspace: remove %s: %w", wp, err)
	}
	return nil
}

// Release drops the claim on the workspace. Idempotent — releasing a
// workspace that doesn't exist or carries no claim is a no-op. The
// directory and its working tree are left intact: the next run reuses
// the warm clone.
func Release(root, projectID, name string) error {
	wp := Path(root, projectID, name)
	if _, err := os.Stat(wp); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("workspace: stat %s: %w", wp, err)
	}
	if err := os.Remove(claimPath(wp)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: remove claim: %w", err)
	}
	if err := os.Remove(legacyClaimPath(wp)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: remove legacy claim: %w", err)
	}
	return nil
}

// Ensure makes sure the workspace directory exists and returns its
// absolute path. First call clones the project's submodule via
// sandbox.EnsureAt; subsequent calls are a no-op. Used by Acquire and
// directly by the standalone `moe workspace shell` path.
func Ensure(root, projectID, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	wp := Path(root, projectID, name)
	return sandbox.EnsureAt(root, projectID, wp)
}

// Attach prepares the workspace for branch:
//
//   - Refuses if the working tree carries any uncommitted changes
//     (staged, unstaged, or untracked). The previous run is expected
//     to have committed before exiting; failing loud at handoff time
//     keeps a stray edit from being silently lost on the upcoming
//     branch switch.
//   - If branch already exists, checks it out (no-op if already on it).
//   - If branch doesn't exist and baseBranch is non-empty, creates
//     branch off baseBranch's tip in one step, without a separate
//     baseBranch checkout. The workspace is an object-shared clone
//     with its own ref-db, so there's no one-checkout-per-branch
//     constraint to work around.
//   - If branch doesn't exist and baseBranch is empty, creates branch
//     off whatever HEAD currently is. Useful for callers that have
//     already positioned the workspace.
//
// Returns the workspacePath unchanged for caller convenience.
func Attach(workspacePath, branch, baseBranch string) error {
	if err := refuseDirty(workspacePath); err != nil {
		return err
	}
	if branchExists(workspacePath, branch) {
		return checkout(workspacePath, branch)
	}
	if baseBranch != "" {
		return checkoutNewFrom(workspacePath, branch, baseBranch)
	}
	return checkoutNew(workspacePath, branch)
}

// refuseDirty returns a fail-loud error when the workspace has any
// uncommitted change. Same shape as the close-time clean-tree gate
// elsewhere in the engine, scoped to the workspace path so callers can
// surface the actual location in their error messages.
func refuseDirty(workspacePath string) error {
	entries, err := git.Status(workspacePath)
	if err != nil {
		return fmt.Errorf("workspace: git status in %s: %w", workspacePath, err)
	}
	if len(entries) == 0 {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		files = append(files, e.Path)
	}
	return fmt.Errorf("workspace %s has %d uncommitted file(s) — commit or revert in the workspace before re-using it: %s",
		workspacePath, len(entries), strings.Join(files, ", "))
}

func branchExists(workspacePath, branch string) bool {
	return git.Run(workspacePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

func checkout(workspacePath, branch string) error {
	if err := git.Run(workspacePath, "checkout", branch); err != nil {
		return fmt.Errorf("workspace: checkout %s: %w", branch, err)
	}
	return nil
}

func checkoutNew(workspacePath, branch string) error {
	if err := git.Run(workspacePath, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("workspace: checkout -b %s: %w", branch, err)
	}
	return nil
}

func checkoutNewFrom(workspacePath, branch, base string) error {
	if err := git.Run(workspacePath, "checkout", "-b", branch, base); err != nil {
		return fmt.Errorf("workspace: checkout -b %s %s: %w", branch, base, err)
	}
	return nil
}

// ResetToDefault parks the workspace on the project's default branch
// after a merge has landed for the holding run. The CLI's release path
// calls it on the workspace branch of the push-merge flow so the next
// claim doesn't anchor itself to a stale view:
//
//   - fetch origin <defaultBranch> with --prune (refreshes
//     origin/<defaultBranch> and drops the dead remote-tracking ref
//     for the merged run branch left behind by DeleteRemoteBranch).
//   - check out <defaultBranch>.
//   - fast-forward local <defaultBranch> to origin/<defaultBranch>.
//   - delete the local run branch (force; the ff above already advanced
//     default past it, but -D names the intent: "this branch is gone,
//     we know it's gone").
//
// Refuses on a dirty working tree. Merge has already landed at this
// point, so dirty here is a real bug — fail loud and let the operator
// recover by hand rather than silently leaving the next run with the
// same stale-default problem.
//
// Idempotent. If already on <defaultBranch>, with <defaultBranch>
// already at origin/<defaultBranch>, and the run branch already gone,
// every step is a no-op. Calling twice is safe.
//
// runBranch is the local branch to delete; if it doesn't exist (e.g.,
// already cleaned on a prior partial run) the delete step is skipped.
// Empty runBranch skips the delete entirely — useful for paths that
// just want the park-on-default behaviour without naming a branch to
// drop.
func ResetToDefault(workspacePath, defaultBranch, runBranch string) error {
	if defaultBranch == "" {
		return fmt.Errorf("workspace: ResetToDefault: default branch is required")
	}
	if _, err := os.Stat(workspacePath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("workspace: ResetToDefault: stat %s: %w", workspacePath, err)
	}
	if err := refuseDirty(workspacePath); err != nil {
		return err
	}
	if out, err := git.Combined(workspacePath, "fetch", "--prune", "origin", defaultBranch); err != nil {
		return fmt.Errorf("workspace: ResetToDefault: fetch --prune origin %s in %s: %w (%s)",
			defaultBranch, workspacePath, err, strings.TrimSpace(out))
	}
	if err := git.Run(workspacePath, "checkout", defaultBranch); err != nil {
		return fmt.Errorf("workspace: ResetToDefault: checkout %s in %s: %w",
			defaultBranch, workspacePath, err)
	}
	originRef := "refs/remotes/origin/" + defaultBranch
	if out, err := git.Combined(workspacePath, "merge", "--ff-only", originRef); err != nil {
		return fmt.Errorf("workspace: ResetToDefault: ff-merge %s in %s: %w (%s)",
			originRef, workspacePath, err, strings.TrimSpace(out))
	}
	if runBranch != "" && runBranch != defaultBranch && branchExists(workspacePath, runBranch) {
		if err := git.Run(workspacePath, "branch", "-D", runBranch); err != nil {
			return fmt.Errorf("workspace: ResetToDefault: delete branch %s: %w", runBranch, err)
		}
	}
	return nil
}

func writeClaim(workspacePath string, c Claim) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: marshal claim: %w", err)
	}
	b = append(b, '\n')
	cp := claimPath(workspacePath)
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		return fmt.Errorf("workspace: mkdir claim parent: %w", err)
	}
	if err := os.WriteFile(cp, b, 0o644); err != nil {
		return fmt.Errorf("workspace: write claim: %w", err)
	}
	return nil
}
