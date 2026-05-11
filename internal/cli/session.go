package cli

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/session"
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
	err = withRepoLock(root, repolock.Options{Purpose: "session-abandon"}, func() error {
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
	err = withRepoLock(root, repolock.Options{Purpose: "session-resolve"}, func() error {
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
