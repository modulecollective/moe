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
// Open and Close are symmetric about main: Open reconciles a resumed
// branch onto current main before handing it back, Close rebases onto
// main before landing. A session that fails to close — Ctrl-C, crash,
// gate refusal — leaves a resumable branch, and without the entry-side
// rebase every later resume would seat the agent on whatever main was
// when the branch was cut.
//
// Conflict policy: if rebase fails, the rebase is aborted and a
// *RebaseFailureError names the worktree, branch, and conflict
// context. On close the CLI errors.As's it to launch a one-shot agent
// in the worktree; falls back to "resolve by hand or `moe session
// abandon`" when auto-resolve doesn't take. On open there is no
// auto-resolve — the state it takes is rare enough to want an operator.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// Operation names for RebaseFailureError.Op. Unexported: nothing
// outside this package branches on the value, it only reads it back
// out of the rendered message.
const (
	opOpen  = "open"
	opClose = "close"
)

// RebaseFailureError is the typed error Open and Close return when
// their rebase-onto-main step fails. Carries the diagnostic context
// the CLI chain-back needs to launch a one-shot agent inside the
// session worktree: the branch/worktree paths it'll cd into, the raw
// git output that triggered the failure, the unmerged paths git left
// (or nil when Dirty is true and the rebase never started), and the
// Dirty flag distinguishing "rebase refused because the worktree is
// dirty" from "rebase hit real conflicts" — different kickoff shape
// per design.
type RebaseFailureError struct {
	// Op is the operation that failed, "open" or "close". Rendered
	// into the message so an operator can tell an entry-side refusal
	// (nothing landed; the turn never started) from an exit-side one
	// (a turn's commits are sitting on the branch). Empty reads as
	// "close".
	Op           string
	Branch       string
	WorktreePath string
	// Conflicts lists the unmerged paths git flagged before --abort
	// discarded the rebase state. Empty when Dirty is true (the
	// rebase never started, so there's nothing in UU state) or when
	// the git status read between rebase failure and --abort itself
	// failed.
	Conflicts []string
	// GitOutput is the verbatim stdout+stderr of the failing rebase,
	// trimmed. Surfaced to the agent so it can read what git said.
	GitOutput string
	// Dirty is true when the rebase refused because the worktree had
	// uncommitted/unstaged changes — i.e. the rebase never started.
	// Detected from the "cannot rebase" prefix in GitOutput.
	Dirty bool
}

// CanvasUnchangedError is the typed error Close returns when the
// session branch's canvas blob matches main's — i.e. this session
// never wrote to its own canvas. The branch and worktree stay
// intact so the operator can either reopen and write the canvas
// or `moe session abandon` explicitly. The previous behaviour
// (silent Abandon on zero-commit sessions, fast-forward on stub-
// equals-main) is the cascade footgun this error is here to close:
// a chain prompt firing after a no-op close lets `!!` / `!!!` carry the
// next stage forward against an unchanged canvas.
type CanvasUnchangedError struct {
	Project      string
	Run          string
	Doc          string
	Branch       string
	WorktreePath string
	// CanvasPath is the canvas's repo-relative path (the same path
	// rendered into the error message).
	CanvasPath string
}

func (e *CanvasUnchangedError) Error() string {
	return fmt.Sprintf(
		"session close: %s canvas at %s tip is unchanged from main\n"+
			"  run:      %s/%s\n"+
			"  worktree: %s\n"+
			"  the agent never wrote to the canvas. either reopen and write,\n"+
			"  or drop the session: moe session abandon %s",
		e.CanvasPath, e.Branch, e.Project, e.Run, e.WorktreePath, e.Branch)
}

func (e *RebaseFailureError) Error() string {
	op := e.Op
	if op == "" {
		op = opClose
	}
	headline := fmt.Sprintf("rebase %s onto main failed", e.Branch)
	fix := "rebase"
	if e.Dirty {
		headline = fmt.Sprintf(
			"rebase %s onto main refused: worktree has uncommitted/unstaged changes", e.Branch)
		fix = "clean up"
	}
	// The two operations leave different work pending. A failed close
	// still has a close to finish, which `moe session resolve` retries.
	// A failed open has nothing pending — re-running the stage opens
	// again, and by then the branch is based on main.
	retry := "re-run moe session resolve"
	if op == opOpen {
		retry = "re-run the stage"
	}
	return fmt.Sprintf(
		"session %s: %s\n"+
			"  worktree: %s\n"+
			"  branch:   %s\n"+
			"  resolve by hand (cd into the worktree, %s, %s)\n"+
			"  or drop it: moe session abandon %s\n"+
			"  git output:\n%s",
		op, headline, e.WorktreePath, e.Branch, fix, retry, e.Branch, e.GitOutput)
}

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
//
// Exported because the branch is the only "is this stage open right
// now" signal readable from a *linked worktree*: refs live in the
// common dir, while List's worktree paths are resolved against the
// root it is handed. Close and Abandon both delete the branch, so
// its existence is exactly "a session is open".
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
// registered, reconcile it with main (see reconcileWithMain) and
// return the existing Session — the operator is picking up where they
// left off, and must pick it up from the current contract. If the
// branch exists without a registered worktree (orphaned state from a
// botched close or manual tampering), return an error pointing the
// operator at `moe session abandon`.
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
		s := &Session{
			Root: root, Project: projectID, Run: runID, Doc: docID,
			Branch: branch, WorktreePath: abs,
		}
		if err := reconcileWithMain(s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if branchExists(root, branch) {
		return nil, fmt.Errorf(
			"session: branch %s exists without a registered worktree — abandoned close?\n"+
				"  run `moe session abandon %s` to drop it, or rebase and merge manually\n"+
				"  or run `moe session gc` to sweep every orphan in one pass",
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

// reconcileWithMain replays a resumed session branch onto current
// main, so the turn about to start reads the contract main carries
// now rather than the one it carried when the branch was cut.
//
// The window this closes: a session that never closed leaves its
// branch behind, and main keeps moving — a kick-back rewriting the
// design, another stage landing, another machine's work arriving via
// the auto-pull that runs immediately before Open. Resuming verbatim
// seats the agent on the old base, where the design doc on disk can
// be a superseded one. Close's rebase hides it after the fact: it
// replays the turn's commits onto current main, so the finished
// history is linear and the divergence leaves no trace.
//
// The ancestor guard is load-bearing, not an optimisation. `git
// rebase` refuses on a dirty worktree even when there is nothing to
// replay, and a dirty-but-current resume — the ordinary crashed-turn
// recovery — has to keep working. Only stale branches take the rebase.
func reconcileWithMain(s *Session) error {
	if git.Probe(s.Root, "merge-base", "--is-ancestor", "main", s.Branch) {
		return nil
	}
	// Covers both stale shapes: a branch with no commits of its own
	// fast-forwards to main, a diverged one replays on top.
	return rebaseOntoMain(s, opOpen)
}

// rebaseOntoMain replays the session branch onto main inside its
// worktree, normalising failure into a *RebaseFailureError tagged
// with op. The rebase is aborted on failure, leaving branch and
// worktree at their pre-rebase state for the operator (or the
// close-side auto-resolve agent) to work on.
//
// Conflict files are read before --abort discards the rebase state —
// same pattern push uses for its RebaseConflictError. "cannot rebase"
// in git's output means the rebase refused outright (dirty worktree)
// rather than hitting a mid-rebase conflict; different kickoff shape,
// so we surface it as Dirty.
func rebaseOntoMain(s *Session, op string) error {
	out, err := git.Combined(s.WorktreePath, "rebase", "main")
	if err == nil {
		return nil
	}
	trimmed := strings.TrimSpace(out)
	dirty := strings.Contains(trimmed, "cannot rebase")
	var conflicts []string
	if !dirty {
		conflicts = sessionUnmergedPaths(s.WorktreePath)
	}
	_, _ = git.Combined(s.WorktreePath, "rebase", "--abort")
	return &RebaseFailureError{
		Op:           op,
		Branch:       s.Branch,
		WorktreePath: s.WorktreePath,
		Conflicts:    conflicts,
		GitOutput:    trimmed,
		Dirty:        dirty,
	}
}

// Close lands the session branch on local main and cleans up the
// worktree. Caller must hold the repo lock.
//
// Close itself stays network-free — the rebase and fast-forward
// touch only local refs, and the repo lock is scoped to same-machine
// concurrency. Pushing to origin is the caller's edge: callers follow
// a successful Close with a push in the same lock window (closeSess
// in internal/cli/stage.go conditions it on okToPush; `moe session
// resolve` wraps sync.WithJournalPush).
//
// On rebase failure, the rebase is aborted, the worktree and branch
// are left intact, and the error names both so the operator can
// resolve by hand or run `moe session abandon`.
func Close(s *Session) error {
	// Refuse to land a session whose canvas at the branch tip is
	// identical to main's — the agent had a conversation but never
	// wrote to the canvas. This single check covers two cases:
	//
	//   1. Zero commits past main: branch is literally at main, so
	//      the blob comparison trivially matches. (Was a silent
	//      Abandon; that path is the cascade footgun this run
	//      closes.)
	//   2. Commits exist but none touched the canvas: the kickoff
	//      stub committed at `Open run` is still the blob at branch
	//      tip and at main. Close used to fast-forward on this; a
	//      downstream `!!` / `!!!` cascade would then dispatch the next
	//      stage against the stub.
	//
	// We compare blob OIDs at <branch>:<canvas> to <main>:<canvas>.
	// Equal blob OIDs mean equal content, which is exactly the
	// "this session never wrote to the canvas" predicate. The
	// branch and worktree stay intact so the operator can reopen
	// and write, or abandon explicitly.
	//
	canvasRel := run.ContentPath(s.Project, s.Run, s.Doc)
	branchBlob, branchErr := git.RevParse(s.WorktreePath, s.Branch+":"+canvasRel)
	mainBlob, mainErr := git.RevParse(s.Root, "main:"+canvasRel)
	// branchErr means the canvas doesn't exist at the branch tip at
	// all — agent never landed it. Refuse loud, same shape as
	// "blob equals main".
	//
	// mainErr means main has no canvas yet (this is the first session
	// to land one). Allow as long as the branch wrote something —
	// branchErr already gated above.
	//
	// Equal blobs is the cascade footgun: kickoff stub on both sides.
	canvasUnchanged := branchErr != nil ||
		(mainErr == nil && branchBlob == mainBlob)
	if canvasUnchanged {
		return &CanvasUnchangedError{
			Project:      s.Project,
			Run:          s.Run,
			Doc:          s.Doc,
			Branch:       s.Branch,
			WorktreePath: s.WorktreePath,
			CanvasPath:   canvasRel,
		}
	}

	// Rebase inside the worktree. We don't fetch origin first —
	// bureaucracy pushes happen through `moe sync`, which holds the
	// same repo lock. Under the lock, local main is the source of truth.
	//
	// On failure we return a typed *RebaseFailureError so the CLI
	// chain-back can launch a one-shot agent in the worktree to
	// resolve.
	if err := rebaseOntoMain(s, opClose); err != nil {
		return err
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
	// A clean ending, so the liveness record has nothing left to vouch
	// for. Dropped last: a record that outlives its branch is harmless
	// (nothing reads it without a live branch beside it), while a branch
	// that outlives its record would read as an unclaimed orphan.
	clearClaim(s)
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
	// The other clean ending — including the heartbeat's own reap, which
	// routes through here. See Close.
	clearClaim(s)
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
		project, run, doc, ok := ParseBranch(w.branch)
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
		project, run, doc, ok := ParseBranch(branch)
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

// ParseBranch splits "session/<project>/<run>/<doc>" into its parts.
//
// Exported because a branch name is the only handle some callers hold —
// the heartbeat's gc pass enumerates refs rather than worktrees, so that
// an orphan branch with no worktree is still seen — and the triple is
// what keys a claim.
func ParseBranch(branch string) (project, run, doc string, ok bool) {
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
	for line := range strings.SplitSeq(out, "\n") {
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

// sessionUnmergedPaths reports the files git left in a conflicted
// (UU/AA/...) state at the time of the failing rebase — the same
// shape push's chain-back uses to name conflicting paths in its
// kickoff. Read before --abort discards the rebase state. Returns
// nil on git.Status failure (the kickoff just lists no files; the
// agent can still run `git status` itself).
func sessionUnmergedPaths(worktreePath string) []string {
	entries, err := git.Status(worktreePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if len(e.XY) < 2 {
			continue
		}
		if e.XY[0] == 'U' || e.XY[1] == 'U' || e.XY == "AA" || e.XY == "DD" {
			out = append(out, e.Path)
		}
	}
	return out
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
