package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/git"
)

// A receipt is transaction intent, never satisfaction. Only ancestry of its
// validated tip proves that the merge completed. Keep it until cleanup ends.
type pendingClose struct {
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
	Tip      string `json:"tip"`
	Main     string `json:"main"`
}

func closePath(s *Session) string {
	return filepath.Join(s.Root, ".moe", "session-closes", s.Project, s.Run, s.Doc+".json")
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func readClose(s *Session) (*pendingClose, error) {
	if !sessionIDPattern.MatchString(s.Project) || !sessionIDPattern.MatchString(s.Run) || !filepath.IsLocal(s.Doc) || s.Branch != BranchName(s.Project, s.Run, s.Doc) {
		return nil, fmt.Errorf("session close: invalid session identity %q", s.Branch)
	}
	body, err := os.ReadFile(closePath(s))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session close: read receipt: %w", err)
	}
	var p pendingClose
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("session close: malformed receipt: %w", err)
	}
	if p.Branch != s.Branch || !objectID.MatchString(p.Tip) || !objectID.MatchString(p.Main) || p.Worktree == "" {
		return nil, fmt.Errorf("session close: invalid receipt for %s", s.Branch)
	}
	if err := managedWorktree(s.Root, p.Worktree); err != nil {
		return nil, err
	}
	if s.WorktreePath != "" && s.WorktreePath != p.Worktree {
		return nil, fmt.Errorf("session close: receipt worktree mismatch for %s", s.Branch)
	}
	return &p, nil
}

func writeClose(s *Session, tip, main string) error {
	p := pendingClose{Branch: s.Branch, Worktree: s.WorktreePath, Tip: tip, Main: main}
	path := closePath(s)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".close-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(append(body, '\n'))
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func removeIfPresent(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Unlike Probe, a false ancestry answer must not hide an unreadable repo.
func ancestor(root, older, newer string) (bool, error) {
	_, err := git.Output(root, "merge-base", "--is-ancestor", older, newer)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func sessionTip(s *Session) (string, error) {
	out, err := git.Output(s.Root, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+s.Branch)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		ref, tip, ok := strings.Cut(line, " ")
		if ok && ref == "refs/heads/"+s.Branch {
			return tip, nil
		}
	}
	return "", nil
}

// finishPending runs before accessing the worktree; removal may already have
// destroyed its Git link. An absent branch is valid only after proven landing.
func finishPending(s *Session) (bool, error) {
	p, err := readClose(s)
	if err != nil || p == nil {
		return false, err
	}
	tip, err := sessionTip(s)
	if err != nil {
		return false, err
	}
	if tip != "" && tip != p.Tip {
		return false, fmt.Errorf("session close: receipt tip differs from %s; preserved for explicit recovery", s.Branch)
	}
	valid, err := ancestor(s.Root, p.Main, p.Tip)
	if err != nil {
		return false, err
	}
	if !valid {
		return false, fmt.Errorf("session close: receipt tip does not descend from recorded main")
	}
	landed, err := ancestor(s.Root, p.Tip, "main")
	if err != nil {
		return false, err
	}
	if !landed {
		if tip == "" {
			return false, fmt.Errorf("session close: unlanded receipt has no branch; preserve %s for recovery", p.Tip)
		}
		return false, nil
	}
	s.WorktreePath = p.Worktree
	return true, teardown(s, false, p.Tip)
}

// Resolve is the explicit recovery verb. Only here may legacy sessions without
// a receipt use whole-tip ancestry as proof of landing; normal Close still
// rejects a newly opened no-op session.
func Resolve(s *Session) error {
	if done, err := finishPending(s); done || err != nil {
		return err
	}
	tip, err := sessionTip(s)
	if err != nil {
		return err
	}
	if tip == "" {
		return fmt.Errorf("session resolve: branch %s is missing", s.Branch)
	}
	landed, err := ancestor(s.Root, tip, "main")
	if err != nil {
		return err
	}
	if landed {
		return teardown(s, false, tip)
	}
	if s.WorktreePath == "" {
		return fmt.Errorf("session resolve: unlanded branch %s has no worktree; retain its commits and restore a worktree before retrying", s.Branch)
	}
	return Close(s)
}

// Only direct children of the managed directory can be removed. Canonicalize
// the parent (e.g. /tmp on macOS), but never follow a session-directory symlink.
func managedWorktree(root, path string) error {
	base, err := canonPath(worktreesDir(root))
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) != base {
		return fmt.Errorf("session cleanup: unmanaged worktree path %q", path)
	}
	info, err := os.Lstat(path)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("session cleanup: worktree path is not a real directory: %s", path)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func preserveWorktree(s *Session) error {
	base := filepath.Join(s.Root, ".moe", "session-recovery")
	if err := os.MkdirAll(base, 0755); err != nil {
		return fmt.Errorf("session cleanup: preserve files: %w", err)
	}
	info, err := os.Lstat(base)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session cleanup: recovery directory is not a real directory: %s", base)
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	path := filepath.Join(base, id)
	if err := os.Rename(s.WorktreePath, path); err != nil {
		return fmt.Errorf("session cleanup: preserve files at %s: %w", s.WorktreePath, err)
	}
	s.RecoveryPaths = append(s.RecoveryPaths, path)
	return nil
}

func recoveryError(s *Session, err error) error {
	if err != nil && len(s.RecoveryPaths) != 0 {
		return fmt.Errorf("%w\n  residual files preserved at %s", err, strings.Join(s.RecoveryPaths, ", "))
	}
	return err
}

func readableWorktree(s *Session, registered bool) (bool, error) {
	link := filepath.Join(s.WorktreePath, ".git")
	info, err := os.Lstat(link)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("session cleanup: invalid Git link at %s", link)
	}
	body, err := os.ReadFile(link)
	if err != nil {
		return false, err
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir: ")
	if !ok {
		return false, nil
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(s.WorktreePath, gitdir)
	}
	common, err := git.Output(s.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, err
	}
	if filepath.Dir(gitdir) != filepath.Join(strings.TrimSpace(common), "worktrees") {
		return false, fmt.Errorf("session cleanup: Git link points outside this repository: %s", link)
	}
	meta, err := os.Lstat(gitdir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !meta.IsDir() || meta.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("session cleanup: invalid Git directory %s", gitdir)
	}
	back, err := os.ReadFile(filepath.Join(gitdir, "gitdir"))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(back)) != link || !registered {
		return false, fmt.Errorf("session cleanup: Git worktree identity mismatch at %s", s.WorktreePath)
	}
	head, err := git.Output(s.WorktreePath, "symbolic-ref", "HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(head) != "refs/heads/"+s.Branch {
		return false, fmt.Errorf("session cleanup: checked out branch mismatch")
	}
	return true, nil
}

// teardown shares the cleanup steps between landed recovery and explicit
// discard. Its caller owns repolock and has stopped the session heartbeat.
func teardown(s *Session, discard bool, landedTip string) (result error) {
	defer func() {
		result = recoveryError(s, result)
	}()
	if s.WorktreePath != "" {
		if err := managedWorktree(s.Root, s.WorktreePath); err != nil {
			return err
		}
	}
	out, err := git.Output(s.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	registered := false
	for _, w := range parseWorktreeList(out) {
		if w.branch == s.Branch && w.path != s.WorktreePath {
			return fmt.Errorf("session cleanup: branch registered at unexpected path %s", w.path)
		}
		if s.WorktreePath != "" && w.path == s.WorktreePath {
			if w.branch != s.Branch {
				return fmt.Errorf("session cleanup: worktree belongs to %s", w.branch)
			}
			registered = true
		}
	}
	if s.WorktreePath != "" {
		_, statErr := os.Lstat(s.WorktreePath)
		exists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		readable := false
		if exists {
			readable, err = readableWorktree(s, registered)
			if err != nil {
				return err
			}
		}
		if readable && !discard {
			// Compare both index and files to the proven landed commit. HEAD
			// may be an unborn symbolic ref if branch deletion already happened.
			for _, args := range [][]string{{"diff", "--name-only", landedTip, "--"}, {"diff", "--cached", "--name-only", landedTip, "--"}} {
				names, err := git.Output(s.WorktreePath, args...)
				if err != nil {
					return err
				}
				if strings.TrimSpace(names) != "" {
					return fmt.Errorf("session cleanup: additional tracked/staged edits at %s; commit or preserve them before resolving", s.WorktreePath)
				}
			}
		}
		if exists && !readable && !discard {
			if err := preserveWorktree(s); err != nil {
				return err
			}
			exists = false
		}
		if exists && !readable && discard {
			// This explicit act authorizes discarding the named residual tree.
			// Git removes its registration once the missing directory is confirmed.
			if err := os.RemoveAll(s.WorktreePath); err != nil {
				return err
			}
			exists = false
		}
		if registered {
			if err := git.Run(s.Root, "worktree", "remove", "--force", s.WorktreePath); err != nil {
				return err
			}
		} else if discard && exists {
			if err := os.RemoveAll(s.WorktreePath); err != nil {
				return err
			}
		}
		if err := removeIfPresent(s.WorktreePath + ".setup-pending"); err != nil {
			return err
		}
	}
	tip, err := sessionTip(s)
	if err != nil {
		return err
	}
	if tip != "" {
		if err := git.Run(s.Root, "branch", "-D", s.Branch); err != nil {
			return err
		}
	}
	if err := clearClaim(s); err != nil {
		return fmt.Errorf("session cleanup: clear claim: %w", err)
	}
	if err := removeIfPresent(closePath(s)); err != nil {
		return fmt.Errorf("session cleanup: clear receipt: %w", err)
	}
	return nil
}
