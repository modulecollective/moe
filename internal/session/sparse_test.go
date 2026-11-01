package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func TestSparseSessionPreservesArchive(t *testing.T) {
	root := newTestRoot(t)
	active := run.ThreadPathFor("claude", "alpha", "active", "design")
	other := run.ThreadPathFor("codex", "beta", "other", "design")
	diagnostic := "projects/beta/runs/other/diagnostics/design/attempt/thread-claude.jsonl"
	included := []string{
		active, run.PromptPathFor("codex", "alpha", "active", "code"),
		"projects/alpha/runs/active/diagnostics/design/attempt/thread-codex.jsonl",
		"projects/alpha/runs/active/diagnostics/design/attempt/prompt-codex.md",
		run.ContentPath("beta", "other", "design"),
		"projects/beta/knowledge/topics/prompt-example.md",
		"projects/beta/hooks/thread-example.jsonl",
		"projects/beta/runs/other/followups.md", "soul.md",
	}
	omitted := []string{other, diagnostic,
		run.PromptPathFor("claude", "beta", "other", "code"),
		"projects/beta/runs/other/diagnostics/design/attempt/prompt-claude.md",
	}
	for _, path := range append(included, omitted...) {
		gittest.WriteAndCommit(t, root, path, "original\n", "seed artifact")
	}
	s, err := Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range included {
		if _, err := os.Stat(filepath.Join(s.WorktreePath, path)); err != nil {
			t.Errorf("included %s: %v", path, err)
		}
	}
	for _, path := range omitted {
		if _, err := os.Stat(filepath.Join(s.WorktreePath, path)); !os.IsNotExist(err) {
			t.Errorf("omitted %s: %v", path, err)
		}
		if out := gittest.Output(t, s.WorktreePath, "ls-files", "-t", "--", path); !strings.HasPrefix(out, "S ") {
			t.Errorf("not skip-worktree: %q", out)
		}
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("canonical lost %s: %v", path, err)
		}
	}
	if out := gittest.Output(t, s.WorktreePath, "status", "--porcelain"); out != "" {
		t.Fatalf("dirty checkout: %s", out)
	}
	if got, want := gittest.Output(t, s.WorktreePath, "write-tree"), gittest.Output(t, root, "rev-parse", "HEAD^{tree}"); got != want {
		t.Fatalf("tree changed: %s != %s", got, want)
	}
	// Sibling sessions retain their own active run and cannot change each
	// other's sparse settings, or canonical main's full checkout.
	sibling, err := Open(root, "beta", "other", "code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sibling.WorktreePath, other)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.WorktreePath, other)); !os.IsNotExist(err) {
		t.Fatal("sibling hydrated other session")
	}
	if git.Probe(root, "config", "--local", "--get", "core.sparseCheckout") {
		t.Fatal("canonical sparse config changed")
	}
	if git.Probe(root, "config", "--worktree", "--get", "core.sparseCheckout") {
		t.Fatal("canonical worktree made sparse")
	}
	if err := Abandon(sibling); err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, s.WorktreePath, run.ContentPath("alpha", "active", "design"), "canvas\n", "canvas")
	commitInWorktree(t, s.WorktreePath, active, "active updated\n", "thread")
	gittest.WriteAndCommit(t, root, other, "incoming\n", "other thread changed")
	resumed, err := Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.WorktreePath != s.WorktreePath {
		t.Fatal("did not resume")
	}
	if _, err := os.Stat(filepath.Join(s.WorktreePath, other)); !os.IsNotExist(err) {
		t.Fatal("rebase hydrated archive")
	}
	if err := Close(s); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{other: "incoming\n", active: "active updated\n", diagnostic: "original\n"} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", path, got, err)
		}
	}
}

func TestSparseSetupFailureCleansNewSession(t *testing.T) {
	root := newTestRoot(t)
	previous := git.Hook
	defer func() { git.Hook = previous }()
	injected := false
	git.Hook = func(dir string, args []string, _ time.Duration, err error) {
		if !injected && err == nil && len(args) > 2 && args[0] == "worktree" && args[1] == "add" {
			injected = true
			path := args[len(args)-1]
			b, e := os.ReadFile(filepath.Join(path, ".git"))
			if e != nil {
				t.Fatal(e)
			}
			gitdir := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir: "))
			// An unreadable worktree config makes sparse setup fail after add.
			if e := os.Mkdir(filepath.Join(gitdir, "config.worktree"), 0755); e != nil {
				t.Fatal(e)
			}
		}
	}
	if _, err := Open(root, "alpha", "active", "design"); err == nil {
		t.Fatal("setup unexpectedly succeeded")
	}
	git.Hook = previous
	if !injected {
		t.Fatal("failure was not injected")
	}
	if branchExists(root, BranchName("alpha", "active", "design")) {
		t.Fatal("partial branch survived")
	}
	entries, err := os.ReadDir(worktreesDir(root))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial worktree survived: %v, %v", entries, err)
	}
	if _, err := Open(root, "alpha", "active", "design"); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestFullSessionResumeKeepsCheckout(t *testing.T) {
	root := newTestRoot(t)
	path := run.ThreadPathFor("claude", "beta", "other", "design")
	gittest.WriteAndCommit(t, root, path, "archive", "seed")
	full := filepath.Join(worktreesDir(root), "old-full")
	gittest.Run(t, root, "worktree", "add", "-b", BranchName("alpha", "active", "design"), full)
	s, err := Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.WorktreePath, path)); err != nil {
		t.Fatal(err)
	}
}

func TestSparseCheckoutFailureCleanup(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup-fails=%t", failCleanup), func(t *testing.T) {
			root := newTestRoot(t)
			canvas := run.ContentPath("alpha", "active", "design")
			gittest.WriteAndCommit(t, root, canvas, "required context\n", "seed canvas")
			previous := git.Hook
			defer func() { git.Hook = previous }()
			var worktree, gitdir string
			checkoutFailed := false
			git.Hook = func(dir string, args []string, _ time.Duration, err error) {
				if args[0] == "checkout" && err != nil {
					checkoutFailed = true
				}
				if args[0] != "sparse-checkout" || err != nil {
					return
				}
				worktree = dir
				b, e := os.ReadFile(filepath.Join(dir, ".git"))
				if e != nil {
					t.Fatal(e)
				}
				gitdir = strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir: "))
				// Prevent checkout after sparse setup has succeeded.
				if e := os.WriteFile(filepath.Join(gitdir, "index.lock"), nil, 0644); e != nil {
					t.Fatal(e)
				}
				if failCleanup {
					// One --force cannot remove a locked worktree.
					if e := os.WriteFile(filepath.Join(gitdir, "locked"), []byte("probe"), 0644); e != nil {
						t.Fatal(e)
					}
				}
			}
			_, err := Open(root, "alpha", "active", "design")
			git.Hook = previous
			if err == nil || !checkoutFailed {
				t.Fatalf("checkout failure not reached: %v", err)
			}
			branch := BranchName("alpha", "active", "design")
			if failCleanup {
				for _, want := range []string{"surviving branch", branch, worktree} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("cleanup failure missing %q: %v", want, err)
					}
				}
				if !branchExists(root, branch) {
					t.Fatal("surviving branch was not retained")
				}
				if err := os.Remove(filepath.Join(gitdir, "locked")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(gitdir, "index.lock")); err != nil {
					t.Fatal(err)
				}
				before := gittest.Output(t, worktree, "status", "--porcelain")
				if s, err := Open(root, "alpha", "active", "design"); s != nil || err == nil {
					t.Fatalf("direct retry accepted incomplete checkout: session=%v, err=%v", s, err)
				} else {
					for _, want := range []string{"incomplete setup", worktree, "moe session abandon " + branch} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("retry refusal missing %q: %v", want, err)
						}
					}
				}
				if after := gittest.Output(t, worktree, "status", "--porcelain"); after != before {
					t.Fatalf("retry modified incomplete tree: before=%q, after=%q", before, after)
				}
				if err := Abandon(&Session{Root: root, Branch: branch, WorktreePath: worktree}); err != nil {
					t.Fatal(err)
				}
			}
			if branchExists(root, branch) {
				t.Fatal("partial branch survived cleanup")
			}
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatalf("partial worktree survived cleanup: %v", err)
			}
			s, err := Open(root, "alpha", "active", "design")
			if err != nil {
				t.Fatalf("retry after cleanup: %v", err)
			}
			if body, err := os.ReadFile(filepath.Join(s.WorktreePath, canvas)); err != nil || string(body) != "required context\n" {
				t.Fatalf("retry canvas = %q, %v", body, err)
			}
			if status := gittest.Output(t, s.WorktreePath, "status", "--porcelain"); status != "" {
				t.Fatalf("retry checkout is dirty: %s", status)
			}
		})
	}
}

func TestSparseInterruptedSetupRefusesResume(t *testing.T) {
	if root := os.Getenv("MOE_TEST_SETUP_ROOT"); root != "" {
		stop := os.Getenv("MOE_TEST_SETUP_STOP")
		git.Hook = func(_ string, args []string, _ time.Duration, err error) {
			if err == nil && ((stop == "registered" && args[0] == "worktree" && args[1] == "add") ||
				(stop == "sparse" && args[0] == "sparse-checkout") ||
				(stop == "populated" && args[0] == "checkout")) {
				// Exit without cleanup, just as a killed opener would.
				os.Exit(42)
			}
		}
		_, err := Open(root, "alpha", "active", "design")
		t.Fatalf("interruption not reached: %v", err)
	}
	for _, stop := range []string{"registered", "sparse", "populated"} {
		t.Run(stop, func(t *testing.T) {
			root := newTestRoot(t)
			canvas := run.ContentPath("alpha", "active", "design")
			gittest.WriteAndCommit(t, root, canvas, "required context\n", "seed canvas")
			cmd := exec.Command(os.Args[0], "-test.run=^TestSparseInterruptedSetupRefusesResume$")
			cmd.Env = append(os.Environ(), "MOE_TEST_SETUP_ROOT="+root, "MOE_TEST_SETUP_STOP="+stop)
			out, err := cmd.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 42 {
				t.Fatalf("interrupted opener: %v\n%s", err, out)
			}
			branch := BranchName("alpha", "active", "design")
			worktree, err := findWorktreeForBranch(root, branch)
			if err != nil || worktree == "" {
				t.Fatalf("interruption left no registered worktree: %q, %v", worktree, err)
			}
			if s, err := Open(root, "alpha", "active", "design"); s != nil || err == nil {
				t.Fatalf("accepted interrupted setup: session=%v, err=%v", s, err)
			} else if !strings.Contains(err.Error(), "moe session abandon "+branch) {
				t.Fatalf("missing recovery guidance: %v", err)
			}
			if err := Abandon(&Session{Root: root, Branch: branch, WorktreePath: worktree}); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(worktreesDir(root))
			if err != nil || len(entries) != 0 {
				t.Fatalf("abandon left setup state: %v, %v", entries, err)
			}
			s, err := Open(root, "alpha", "active", "design")
			if err != nil {
				t.Fatalf("open after abandon: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.WorktreePath, canvas)); err != nil {
				t.Fatalf("fresh checkout missing context: %v", err)
			}
		})
	}
}

func TestSparseResumeKeepsStagedDeletion(t *testing.T) {
	root := newTestRoot(t)
	canvas := run.ContentPath("alpha", "active", "design")
	gittest.WriteAndCommit(t, root, canvas, "context\n", "seed canvas")
	s, err := Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	// A valid interrupted turn can look just like an unpopulated checkout.
	gittest.Run(t, s.WorktreePath, "rm", "--", canvas)
	before := gittest.Output(t, s.WorktreePath, "status", "--porcelain")
	resumed, err := Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatalf("resume with staged deletion: %v", err)
	}
	if resumed.WorktreePath != s.WorktreePath {
		t.Fatal("did not resume existing checkout")
	}
	if after := gittest.Output(t, s.WorktreePath, "status", "--porcelain"); after != before {
		t.Fatalf("resume changed staged deletion: before=%q, after=%q", before, after)
	}
}

func TestOpenRejectsPatternIDs(t *testing.T) {
	root := newTestRoot(t)
	for _, id := range []string{"*", "../other", "foo/bar", "[a]"} {
		if _, err := Open(root, "alpha", id, "design"); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}
