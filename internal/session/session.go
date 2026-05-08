// Package session manages throwaway git worktrees for long-lived
// stage sessions.
//
// A stage session (moe sdlc design, moe sdlc code) runs Claude against
// the bureaucracy repo for minutes to hours. Holding the repo lock
// across that window would block every other moe command, so we run
// each session on a dedicated branch in a git worktree under
// <root>/.moe/worktrees/<uuid>/. Mutations during the session land on
// the session branch — no lock needed because the branch has a single
// writer. At session close, we rebase the session branch onto main,
// fast-forward main to the rebased tip, push (best effort), and tear
// down the worktree and branch.
//
// Caller responsibility: Open and Close must run with the repo lock
// held (see internal/repolock). Session turns in between run without
// the lock.
//
// Conflict policy: if rebase fails, Close aborts the rebase and
// returns an error that names the worktree and branch so the operator
// can resolve by hand or via `moe session abandon`.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// Session identifies one active stage session.
type Session struct {
	// Root is the bureaucracy root.
	Root string
	// Project / Run / Doc key the session to the document being edited.
	Project string
	Run     string
	Doc     string
	// Branch is the session branch (refs/heads/session/<p>/<r>/<d>).
	Branch string
	// WorktreePath is the absolute path to the worktree directory under
	// <root>/.moe/worktrees/<uuid>/.
	WorktreePath string
}

// BranchName returns the branch name for a (project, run, doc) tuple.
// Kept separate from Open so callers can grep for existing sessions
// without opening one.
func BranchName(projectID, runID, docID string) string {
	return "session/" + projectID + "/" + runID + "/" + docID
}

// worktreesDir is where session worktrees live under the bureaucracy root.
func worktreesDir(root string) string {
	return filepath.Join(root, ".moe", "worktrees")
}

// Open creates (or resumes) a session worktree for (projectID, runID,
// docID). The caller must hold the repo lock.
//
// Resume logic: if the session branch exists and its worktree is
// registered, return the existing Session — the operator is picking up
// where they left off. If the branch exists without a registered
// worktree (orphaned state from a botched close or manual tampering),
// return an error pointing the operator at `moe session abandon`.
func Open(root, projectID, runID, docID string) (*Session, error) {
	branch := BranchName(projectID, runID, docID)

	existing, err := findWorktreeForBranch(root, branch)
	if err != nil {
		return nil, err
	}
	if existing != "" {
		abs, absErr := canonPath(existing)
		if absErr != nil {
			return nil, fmt.Errorf("session: resolve worktree path %q: %w", existing, absErr)
		}
		return &Session{
			Root: root, Project: projectID, Run: runID, Doc: docID,
			Branch: branch, WorktreePath: abs,
		}, nil
	}
	if branchExists(root, branch) {
		return nil, fmt.Errorf(
			"session: branch %s exists without a registered worktree — abandoned close?\n"+
				"  run `moe session abandon %s` to drop it, or rebase and merge manually",
			branch, branch)
	}

	uuid, err := newUUID()
	if err != nil {
		return nil, err
	}
	worktreePath := filepath.Join(worktreesDir(root), uuid)
	if err := os.MkdirAll(worktreesDir(root), 0o755); err != nil {
		return nil, fmt.Errorf("session: mkdir %s: %w", worktreesDir(root), err)
	}

	// -b <branch> creates the branch off HEAD (main). We stage the
	// worktree at HEAD, not origin/main — the caller takes the repo
	// lock before Open, so a moved origin is the next caller's
	// concern (their sync will handle it).
	if out, err := git.Combined(root, "worktree", "add", "-b", branch, worktreePath); err != nil {
		_ = os.RemoveAll(worktreePath)
		return nil, fmt.Errorf("session: git worktree add: %w (%s)", err, strings.TrimSpace(out))
	}

	abs, err := canonPath(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("session: resolve worktree path: %w", err)
	}
	return &Session{
		Root: root, Project: projectID, Run: runID, Doc: docID,
		Branch: branch, WorktreePath: abs,
	}, nil
}

// Close lands the session branch on local main and cleans up the
// worktree. Caller must hold the repo lock.
//
// Pushing to origin is intentionally NOT part of close — the lock is
// scoped to same-machine concurrency, and `moe sync` is the explicit
// origin-push point. Making every session exit block on the network
// would add no correctness and a lot of latency.
//
// On rebase failure, the rebase is aborted, the worktree and branch
// are left intact, and the error names both so the operator can
// resolve by hand or run `moe session abandon`.
func Close(s *Session) error {
	// A session that never produced a commit (open + early bail before
	// the first turn — bootstrap error, executor refused, etc.) has
	// nothing to land. Tear it down silently rather than running the
	// canvas gate against a branch tip that's literally still at main.
	// The gate is for the silent-empty-fast-forward case where commits
	// exist but the canvas isn't among them; a zero-commit branch is
	// the no-work case, and Abandon is the right semantics.
	count, err := newCommitsPastMain(s.Root, s.Branch)
	if err != nil {
		return fmt.Errorf("session close: count commits on %s: %w", s.Branch, err)
	}
	if count == 0 {
		return Abandon(s)
	}

	// Mirror commitTurn's per-turn predicate at the seal point: refuse
	// to land a session whose canvas at the branch tip is empty (or
	// absent from the tree). commitTurn is the only producer of
	// session-branch commits and refuses to commit an empty canvas, so
	// a non-empty blob here means at least one work turn landed. The
	// branch-tip read (rather than the worktree) is the direct mirror
	// of "what would actually merge" — the worktree can disagree
	// (post-commit edits, a seed canvas the session never touched),
	// and only what's committed can fast-forward main.
	canvasRel := run.ContentPath(s.Project, s.Run, s.Doc)
	switch out, err := git.Combined(s.WorktreePath, "show", s.Branch+":"+canvasRel); {
	case err != nil, len(strings.TrimRight(out, "\n")) == 0:
		return fmt.Errorf(
			"session close: canvas %s at %s is empty — no commitTurn landed this session\n"+
				"  worktree: %s\n"+
				"  branch:   %s\n"+
				"  resolve by writing to the canvas and re-closing,\n"+
				"  or drop it: moe session abandon %s",
			canvasRel, s.Branch, s.WorktreePath, s.Branch, s.Branch)
	}

	// Rebase inside the worktree. We don't fetch origin first —
	// bureaucracy pushes happen through `moe sync`, which holds the
	// same repo lock. Under the lock, local main is the source of truth.
	if out, err := git.Combined(s.WorktreePath, "rebase", "main"); err != nil {
		_, _ = git.Combined(s.WorktreePath, "rebase", "--abort")
		return fmt.Errorf(
			"session close: rebase %s onto main failed: %w\n"+
				"  worktree: %s\n"+
				"  branch:   %s\n"+
				"  resolve by hand (cd into the worktree, rebase, re-run moe session resolve)\n"+
				"  or drop it: moe session abandon %s\n"+
				"  git output:\n%s",
			s.Branch, err, s.WorktreePath, s.Branch, s.Branch, strings.TrimSpace(out))
	}

	// Fast-forward main from the canonical root, not via `update-ref`.
	// `update-ref` would move the ref but leave the canonical root's
	// working tree and index at the old main, so downstream commands
	// (e.g. `moe sdlc push`) would read stale files from disk. `merge
	// --ff-only` updates ref, index, and working tree in one step and
	// refuses non-fast-forward, giving us the same safety check the
	// old-value CAS did.
	if out, err := git.Combined(s.Root, "merge", "--ff-only", s.Branch); err != nil {
		return fmt.Errorf("session close: fast-forward main to %s failed: %w (%s)",
			s.Branch, err, strings.TrimSpace(out))
	}

	// Remove worktree and delete branch. Order matters: `git branch -d`
	// refuses while a worktree has the branch checked out.
	//
	// --force is required when the superproject has submodules: plain
	// `git worktree remove` refuses with "working trees containing
	// submodules cannot be moved or removed". By this point the rebase
	// and fast-forward have succeeded, so there's no unsaved state the
	// safety check would protect.
	if out, err := git.Combined(s.Root, "worktree", "remove", "--force", s.WorktreePath); err != nil {
		return fmt.Errorf("session close: remove worktree %s: %w (%s)",
			s.WorktreePath, err, strings.TrimSpace(out))
	}
	if out, err := git.Combined(s.Root, "branch", "-D", s.Branch); err != nil {
		return fmt.Errorf("session close: delete branch %s: %w (%s)",
			s.Branch, err, strings.TrimSpace(out))
	}
	return nil
}

// Abandon drops the worktree and the session branch without landing
// any of its commits. Used by `moe session abandon` to clean up a
// session the operator no longer wants. Caller must hold the repo lock.
func Abandon(s *Session) error {
	// -f so a mid-rebase or mid-edit worktree still goes.
	if out, err := git.Combined(s.Root, "worktree", "remove", "--force", s.WorktreePath); err != nil {
		// Tolerate "not a working tree" — someone may have manually
		// deleted the directory. Proceed to branch deletion regardless.
		if !strings.Contains(strings.ToLower(out), "not a working tree") {
			return fmt.Errorf("session abandon: remove worktree: %w (%s)", err, strings.TrimSpace(out))
		}
		// Best-effort directory cleanup.
		_ = os.RemoveAll(s.WorktreePath)
	}
	if branchExists(s.Root, s.Branch) {
		if out, err := git.Combined(s.Root, "branch", "-D", s.Branch); err != nil {
			return fmt.Errorf("session abandon: delete branch: %w (%s)", err, strings.TrimSpace(out))
		}
	}
	return nil
}

// List returns every currently-registered session worktree under
// <root>/.moe/worktrees/. Caller may read without holding the repo
// lock — `git worktree list` is read-only.
func List(root string) ([]*Session, error) {
	out, err := git.Output(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("session list: git worktree list: %w", err)
	}
	wts := parseWorktreeList(out)
	// git resolves symlinks in worktree paths (e.g. /tmp → /private/tmp
	// on macOS), so canonicalize our reference dir the same way before
	// comparing.
	wtsDir, err := canonPath(worktreesDir(root))
	if err != nil {
		return nil, err
	}
	sessions := make([]*Session, 0, len(wts))
	for _, w := range wts {
		absPath, err := canonPath(w.path)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(wtsDir, absPath)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if !strings.HasPrefix(w.branch, "session/") {
			continue
		}
		project, run, doc, ok := parseBranch(w.branch)
		if !ok {
			continue
		}
		sessions = append(sessions, &Session{
			Root: root, Project: project, Run: run, Doc: doc,
			Branch: w.branch, WorktreePath: absPath,
		})
	}
	return sessions, nil
}

// FindByBranch returns the session registered for a given branch, or
// nil if none exists. Useful for `moe session abandon <branch>`.
func FindByBranch(root, branch string) (*Session, error) {
	sessions, err := List(root)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Branch == branch {
			return s, nil
		}
	}
	// Branch may exist as an orphan (no worktree). Surface as a
	// synthetic session so Abandon can still delete the branch.
	if branchExists(root, branch) {
		project, run, doc, ok := parseBranch(branch)
		if !ok {
			return nil, fmt.Errorf("session: unparseable branch name %q", branch)
		}
		return &Session{
			Root: root, Project: project, Run: run, Doc: doc,
			Branch: branch,
		}, nil
	}
	return nil, nil
}

// parseBranch splits "session/<project>/<run>/<doc>" into its parts.
func parseBranch(branch string) (project, run, doc string, ok bool) {
	const prefix = "session/"
	if !strings.HasPrefix(branch, prefix) {
		return "", "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(branch, prefix), "/", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// worktreeEntry is one parsed record from `git worktree list --porcelain`.
type worktreeEntry struct {
	path   string
	head   string
	branch string // refs/heads/… trimmed to short form
}

func parseWorktreeList(out string) []worktreeEntry {
	var entries []worktreeEntry
	var cur worktreeEntry
	flush := func() {
		if cur.path != "" {
			entries = append(entries, cur)
		}
		cur = worktreeEntry{}
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "worktree":
			cur.path = val
		case "HEAD":
			cur.head = val
		case "branch":
			cur.branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.branch = ""
		}
	}
	flush()
	return entries
}

// findWorktreeForBranch returns the absolute path of the worktree that
// has branch checked out, or "" if none.
func findWorktreeForBranch(root, branch string) (string, error) {
	out, err := git.Output(root, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("session: git worktree list: %w", err)
	}
	for _, w := range parseWorktreeList(out) {
		if w.branch == branch {
			return w.path, nil
		}
	}
	return "", nil
}

func branchExists(root, branch string) bool {
	_, err := git.Output(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// newCommitsPastMain returns how many commits branch carries beyond
// main. Used by Close to distinguish "session never produced anything,
// silently tear down" (zero) from "session has commits, run the canvas
// gate against them" (non-zero).
func newCommitsPastMain(root, branch string) (int, error) {
	out, err := git.Output(root, "rev-list", "--count", "main.."+branch)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

// canonPath returns an absolute, symlink-resolved version of p. On
// macOS git resolves /tmp to /private/tmp in its worktree listings, so
// comparing paths without this extra step mismatches test fixtures.
// Falls back to filepath.Abs when EvalSymlinks errors on a
// not-yet-existing path.
func canonPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}
