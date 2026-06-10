package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/sync"
)

// `moe session` manages the throwaway branches and worktrees created
// by stage sessions. Normal operation auto-cleans them on Claude exit,
// but a crash, killed process, or rebase conflict can leave state
// behind — these subcommands make it discoverable and actionable.

func init() {
	g := NewCommandGroup("session", "list or clean up leftover stage-session worktrees and branches")
	g.Register(&Command{
		Name:    "list",
		Summary: "list open stage-session worktrees and branches",
		Run:     runSessionList,
	})
	g.Register(&Command{
		Name:    "abandon",
		Summary: "drop a session's worktree and branch without landing its commits",
		Run:     runSessionAbandon,
	})
	g.Register(&Command{
		Name:    "resolve",
		Summary: "retry rebase + ff-merge for a session whose close failed",
		Run:     runSessionResolve,
	})
	g.Register(&Command{
		Name:    "gc",
		Summary: "remove orphan session worktrees and branches",
		Run:     runSessionGC,
	})
	RegisterGroup(g)
}

func runSessionList(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe session list")
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	sessions, err := session.List(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		moePrintln(stdout, "no open sessions")
		return 0
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Branch < sessions[j].Branch
	})
	for _, s := range sessions {
		moePrintf(stdout, "%s\t%s\n", s.Branch, s.WorktreePath)
	}
	return 0
}

func runSessionAbandon(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		moePrintln(stderr, "usage: moe session abandon <branch>")
		moePrintln(stderr, "       (branch is of the form session/<project>/<run>/<doc>; see `moe session list`)")
		return 2
	}
	branch := args[0]
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	err = repolock.With(root, repolock.Options{Purpose: "session-abandon"}, func() error {
		s, err := session.FindByBranch(root, branch)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("no session found for branch %q", branch)
		}
		return session.Abandon(s)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "abandoned %s\n", branch)
	return 0
}

func runSessionResolve(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		moePrintln(stderr, "usage: moe session resolve <branch>")
		moePrintln(stderr, "       retry rebase + ff-merge for <branch>; run this after fixing conflicts")
		moePrintln(stderr, "       inside the session worktree (see `moe session list` for the path).")
		return 2
	}
	branch := args[0]
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	err = sync.WithJournalPush(root, repolock.Options{Purpose: "session-resolve"}, stdout, stderr, func() error {
		s, err := session.FindByBranch(root, branch)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("no session found for branch %q", branch)
		}
		if s.WorktreePath == "" {
			return fmt.Errorf("branch %q has no worktree; use `moe session abandon %s` to drop the branch", branch, branch)
		}
		return session.Close(s)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "resolved %s\n", branch)
	return 0
}

// runSessionGC reaps every stray stage-session object under
// `.moe/worktrees/` and its companion `refs/heads/session/...` branches.
// Five orphan rules — three scoped to registered worktrees, two
// independent passes for residue the registered set can't see:
//
//  1. Registered worktree whose run reached a terminal status
//     (merged, closed, promoted).
//  2. Registered worktree whose run.json is missing on disk.
//  3. Registered worktree whose project directory is missing on disk.
//  4. `refs/heads/session/<p>/<r>/<d>` ref with no worktree (botched
//     abandon residue — the exact case `session.Open` refuses with
//     "abandoned close" today).
//  5. `.moe/worktrees/<uuid>/` directory git doesn't know about
//     (the mirror image of rule 4).
//
// Rules 1–3 reap via `session.Abandon`. Rule 4 also runs through
// `session.Abandon` (FindByBranch returns a synthetic session whose
// Abandon tolerates the missing worktree). Rule 5 removes the directory
// with `os.RemoveAll`. All three passes run under a single repo lock so
// the reap is consistent against parallel `moe session open` /
// `session abandon` calls.
//
// Partial-failure shape mirrors `moe clone gc`: per-orphan errors go to
// stderr, surviving orphans are still listed for the operator, exit 1
// if any reap failed.
func runSessionGC(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		moePrintln(stderr, "usage: moe session gc")
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	var reaped []string
	var reapErrs []string
	err = repolock.With(root, repolock.Options{Purpose: "session-gc"}, func() error {
		regs, err := findOrphanSessions(root)
		if err != nil {
			return err
		}
		for _, s := range regs {
			if err := session.Abandon(s); err != nil {
				reapErrs = append(reapErrs, fmt.Sprintf("%s: %v", s.Branch, err))
				continue
			}
			reaped = append(reaped, s.Branch)
		}
		branches, err := findOrphanSessionBranches(root)
		if err != nil {
			return err
		}
		for _, b := range branches {
			s, ferr := session.FindByBranch(root, b)
			if ferr != nil {
				reapErrs = append(reapErrs, fmt.Sprintf("%s: %v", b, ferr))
				continue
			}
			if s == nil {
				// Branch vanished between scan and reap — nothing to do.
				continue
			}
			if err := session.Abandon(s); err != nil {
				reapErrs = append(reapErrs, fmt.Sprintf("%s: %v", b, err))
				continue
			}
			reaped = append(reaped, b)
		}
		dirs, err := findOrphanWorktreeDirs(root)
		if err != nil {
			return err
		}
		for _, dir := range dirs {
			if err := os.RemoveAll(dir); err != nil {
				reapErrs = append(reapErrs, fmt.Sprintf("worktree dir %s: %v", dir, err))
				continue
			}
			reaped = append(reaped, "worktree dir "+filepath.Base(dir))
		}
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	sort.Strings(reaped)
	if len(reaped) == 0 && len(reapErrs) == 0 {
		moePrintln(stdout, "session gc: no orphan sessions")
		return 0
	}
	for _, r := range reaped {
		moePrintf(stdout, "removed %s\n", r)
	}
	for _, e := range reapErrs {
		moePrintf(stderr, "session gc: %s\n", e)
	}
	if len(reapErrs) > 0 {
		return 1
	}
	return 0
}

// findOrphanSessions returns the registered-worktree sessions whose run
// state says they're reapable: terminal status, missing run.json, or
// missing project directory. Pushed and in-progress runs are skipped —
// their worktrees are still load-bearing for live stage sessions.
// Result is sorted by branch so the verb's output order is stable.
func findOrphanSessions(root string) ([]*session.Session, error) {
	sessions, err := session.List(root)
	if err != nil {
		return nil, err
	}
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	status := make(map[string]string, len(mds))
	for _, md := range mds {
		status[md.Project+"/"+md.ID] = md.Status
	}
	var out []*session.Session
	for _, s := range sessions {
		// Rule 3: project directory missing on disk.
		if _, err := os.Stat(filepath.Join(root, "projects", s.Project)); errors.Is(err, os.ErrNotExist) {
			out = append(out, s)
			continue
		} else if err != nil {
			return nil, fmt.Errorf("session gc: stat project %s: %w", s.Project, err)
		}
		// Rule 1 / Rule 2: terminal run status or run.json missing
		// entirely (Scan only returns runs whose run.json parsed).
		st, ok := status[s.Project+"/"+s.Run]
		if !ok {
			out = append(out, s)
			continue
		}
		switch st {
		case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	return out, nil
}

// findOrphanSessionBranches returns `session/<p>/<r>/<d>` branches with
// no worktree currently checked out. This is the rule-4 residue
// `session.Open` refuses with "branch %s exists without a registered
// worktree — abandoned close?". Result is sorted so the verb's output
// is stable.
func findOrphanSessionBranches(root string) ([]string, error) {
	out, err := git.Output(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/session/")
	if err != nil {
		return nil, fmt.Errorf("session gc: list session branches: %w", err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	wtBranches, err := branchesWithWorktrees(root)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, b := range branches {
		if wtBranches[b] {
			continue
		}
		orphans = append(orphans, b)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// findOrphanWorktreeDirs returns directories directly under
// `.moe/worktrees/<uuid>/` that git's `worktree list` doesn't reference.
// This is the rule-5 residue — a directory left over from an aborted
// `worktree add`, a manual `rm -rf` of the canonical-side registration,
// or a botched abandon whose `worktree remove` partially completed.
// Result is sorted so the verb's output is stable.
func findOrphanWorktreeDirs(root string) ([]string, error) {
	wtRoot := filepath.Join(root, ".moe", "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session gc: read %s: %w", wtRoot, err)
	}
	known, err := worktreePaths(root)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(wtRoot, e.Name())
		if isKnownWorktreePath(full, known) {
			continue
		}
		orphans = append(orphans, full)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// branchesWithWorktrees returns every branch git's `worktree list` says
// is currently checked out, in a set keyed by short branch name. Used
// by rule 4 to subtract registered branches from the set of session/*
// refs.
func branchesWithWorktrees(root string) (map[string]bool, error) {
	out, err := git.Output(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("session gc: worktree list: %w", err)
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			set[b] = true
		}
	}
	return set, nil
}

// worktreePaths returns the set of worktree paths git knows about. Both
// the literal path and its symlink-resolved form are stored so callers
// can compare either against an on-disk directory; macOS's /tmp →
// /private/tmp resolution is the canonical example.
func worktreePaths(root string) (map[string]bool, error) {
	out, err := git.Output(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("session gc: worktree list: %w", err)
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			set[abs] = true
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			set[resolved] = true
		}
	}
	return set, nil
}

// isKnownWorktreePath reports whether dir is in the known set after
// resolving symlinks. The lookup is symmetric — see worktreePaths for
// why both shapes are stored.
func isKnownWorktreePath(dir string, known map[string]bool) bool {
	if abs, err := filepath.Abs(dir); err == nil {
		if known[abs] {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil && known[resolved] {
			return true
		}
	}
	return false
}

// findRoot is a small wrapper so the session subcommands can share the
// cwd → Find → error-print idiom without reimplementing it.
func findRoot(stderr io.Writer) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", err
	}
	return root, nil
}
