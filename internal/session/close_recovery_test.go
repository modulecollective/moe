package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// Interrupt worktree removal after Git has already unlinked .git. Retrying
// must use the landed transaction, rather than compare the canvas to main.
func TestCloseRetriesLandedMissingLink(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/thread-codex.jsonl", "saved transcript", "save transcript")
	tip := gittest.Output(t, root, "rev-parse", s.Branch)
	// The failed removal leaves files whose provenance Git cannot establish.
	git.Hook = func(dir string, args []string, _ time.Duration, err error) {
		if dir == root && strings.Join(args, " ") == "merge --ff-only "+s.Branch && err == nil {
			if err := os.Remove(filepath.Join(s.WorktreePath, ".git")); err != nil {
				t.Error(err)
			}
		}
	}
	err = Close(s)
	git.Hook = nil
	if err != nil { // Both immediate repair and a later retry are valid.
		t.Logf("first close: %v", err)
		if err := Close(s); err != nil {
			t.Fatal(err)
		}
	}
	if got := gittest.Output(t, root, "rev-parse", "main"); got != tip {
		t.Fatalf("duplicate work commit: %s != %s", got, tip)
	}
	if branchExists(root, s.Branch) {
		t.Fatal("branch remains")
	}
	if got := gittest.Output(t, root, "show", "main:"+canvasRel); got != "saved canvas" {
		t.Fatal(got)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".moe", "session-recovery", "*", "projects/moe/runs/r1/documents/design/thread-codex.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("residual files not preserved: %v %v", matches, err)
	}
}

func TestCloseRetryBoundaries(t *testing.T) {
	for _, boundary := range []string{"merge", "worktree", "branch", "claim"} {
		t.Run(boundary, func(t *testing.T) {
			root := newTestRoot(t)
			s, stop, err := Reserve(root, "moe", "r1", "design", false)
			if err != nil {
				t.Fatal(err)
			}
			stop()
			commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
			tip := gittest.Output(t, root, "rev-parse", s.Branch)
			claimPath := ClaimPath(root, "moe", "r1", "design")
			lock := filepath.Join(root, ".git", "refs", "heads", s.Branch+".lock")
			var undo func()
			git.Hook = func(dir string, args []string, _ time.Duration, err error) {
				if dir != root || err != nil || undo != nil {
					return
				}
				command := strings.Join(args, " ")
				switch {
				case boundary == "merge" && command == "merge --ff-only "+s.Branch:
					p := filepath.Join(s.WorktreePath, canvasRel)
					if err := os.WriteFile(p, []byte("additional edit"), 0600); err != nil {
						t.Fatal(err)
					}
					undo = func() {
						if err := os.WriteFile(p, []byte("saved canvas"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				case boundary == "worktree" && command == "worktree remove --force "+s.WorktreePath:
					if err := os.WriteFile(lock, nil, 0600); err != nil {
						t.Fatal(err)
					}
					undo = func() {
						if err := os.Remove(lock); err != nil {
							t.Fatal(err)
						}
					}
				case (boundary == "branch" || boundary == "claim") && command == "branch -D "+s.Branch:
					p := filepath.Dir(claimPath)
					if boundary == "claim" {
						p = filepath.Dir(closePath(s))
					}
					if err := os.Chmod(p, 0500); err != nil {
						t.Fatal(err)
					}
					undo = func() {
						if err := os.Chmod(p, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			err = Close(s)
			git.Hook = nil
			if undo == nil {
				t.Fatal("boundary not driven")
			}
			undo()
			if err == nil {
				t.Fatal("injected cleanup failure ignored")
			}
			if _, err := os.Stat(closePath(s)); err != nil {
				t.Fatalf("receipt lost: %v", err)
			}
			commitInWorktree(t, root, "later.txt", "later work", "advance main")
			main := gittest.Output(t, root, "rev-parse", "main")
			recovered, err := FindByBranch(root, s.Branch)
			if err != nil || recovered == nil {
				t.Fatalf("find: %v %v", recovered, err)
			}
			if err := Close(recovered); err != nil {
				t.Fatal(err)
			}
			if got := gittest.Output(t, root, "rev-parse", "main"); got != main {
				t.Fatal("retry minted another commit")
			}
			if !git.Probe(root, "merge-base", "--is-ancestor", tip, "main") {
				t.Fatal("saved turn lost")
			}
			for _, p := range []string{s.WorktreePath, closePath(s), claimPath} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("residue at %s: %v", p, err)
				}
			}
			if branchExists(root, s.Branch) {
				t.Fatal("branch remains")
			}
		})
	}
}

func TestResolveLegacyPartialWorktrees(t *testing.T) {
	for _, shape := range []string{"readable", "missing-link", "missing-directory", "missing-registration", "orphan", "unlanded"} {
		t.Run(shape, func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
			other, err := Open(root, "moe", "other", "design")
			if err != nil {
				t.Fatal(err)
			}
			otherLink, err := os.ReadFile(filepath.Join(other.WorktreePath, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if shape != "unlanded" {
				gittest.Run(t, root, "merge", "--ff-only", s.Branch)
			}
			switch shape {
			case "missing-link":
				if err := os.Remove(filepath.Join(s.WorktreePath, ".git")); err != nil {
					t.Fatal(err)
				}
			case "missing-directory":
				if err := os.RemoveAll(s.WorktreePath); err != nil {
					t.Fatal(err)
				}
			case "missing-registration", "orphan":
				link, err := os.ReadFile(filepath.Join(s.WorktreePath, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				dir := strings.TrimSpace(strings.TrimPrefix(string(link), "gitdir: "))
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if shape == "orphan" {
					if err := os.RemoveAll(s.WorktreePath); err != nil {
						t.Fatal(err)
					}
					s.WorktreePath = ""
				} else {
					if err := os.Remove(filepath.Join(s.WorktreePath, ".git")); err != nil {
						t.Fatal(err)
					}
				}
			case "unlanded":
				gittest.Run(t, root, "worktree", "remove", "--force", s.WorktreePath)
				s.WorktreePath = ""
			}
			err = Resolve(s)
			if shape == "unlanded" {
				if err == nil || !branchExists(root, s.Branch) {
					t.Fatalf("unlanded work discarded: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !branchExists(root, other.Branch) {
				t.Fatal("other branch affected")
			}
			after, err := os.ReadFile(filepath.Join(other.WorktreePath, ".git"))
			if err != nil || string(after) != string(otherLink) {
				t.Fatalf("other registration affected: %v", err)
			}
			if shape == "missing-link" || shape == "missing-registration" {
				if len(s.RecoveryPaths) != 1 {
					t.Fatal("no recovery path")
				}
				if b, err := os.ReadFile(filepath.Join(s.RecoveryPaths[0], canvasRel)); err != nil || string(b) != "saved canvas" {
					t.Fatalf("backup lost: %s %v", b, err)
				}
			}
		})
	}
}

func TestPendingCloseRefusesUnprovenLanding(t *testing.T) {
	for _, shape := range []string{"corrupt", "tip-mismatch", "missing-object", "unlanded"} {
		t.Run(shape, func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			main := gittest.Output(t, root, "rev-parse", "main")
			commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
			tip := gittest.Output(t, root, "rev-parse", s.Branch)
			if err := writeClose(s, tip, main); err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "corrupt":
				if err := os.WriteFile(closePath(s), []byte("bad JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			case "tip-mismatch":
				commitInWorktree(t, s.WorktreePath, "more", "new", "more work")
			case "missing-object":
				if err := writeClose(s, strings.Repeat("f", 40), main); err != nil {
					t.Fatal(err)
				}
			}
			done, err := finishPending(s)
			if done || (err == nil && shape != "unlanded") {
				t.Fatalf("unproven receipt accepted: %v %v", done, err)
			}
			if !branchExists(root, s.Branch) {
				t.Fatal("branch deleted")
			}
			if _, err := os.Stat(s.WorktreePath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAbandonDiscardsNamedBrokenWorktree(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.WorktreePath, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.WorktreePath, "unfinished"), []byte("explicitly discarded"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Abandon(s); err != nil {
		t.Fatal(err)
	}
	if branchExists(root, s.Branch) {
		t.Fatal("branch remains")
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
}

func TestFailedReceiptWritePreventsLanding(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatal(err)
	}
	main := gittest.Output(t, root, "rev-parse", "main")
	commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
	if err := os.WriteFile(filepath.Join(root, ".moe", "session-closes"), []byte("block receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Close(s); err == nil {
		t.Fatal("close ignored receipt write failure")
	}
	if got := gittest.Output(t, root, "rev-parse", "main"); got != main {
		t.Fatal("merge ran without receipt")
	}
	if !branchExists(root, s.Branch) {
		t.Fatal("session evidence lost")
	}
}

func TestPendingCloseWithMissingBranchChecksIndexAndFiles(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprint(dirty), func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			main := gittest.Output(t, root, "rev-parse", "main")
			commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
			tip := gittest.Output(t, root, "rev-parse", s.Branch)
			if err := writeClose(s, tip, main); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, root, "merge", "--ff-only", s.Branch)
			gittest.Run(t, root, "update-ref", "-d", "refs/heads/"+s.Branch)
			if dirty {
				p := filepath.Join(s.WorktreePath, canvasRel)
				if err := os.WriteFile(p, []byte("staged edit"), 0600); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, s.WorktreePath, "add", canvasRel)
				if err := os.WriteFile(p, []byte("saved canvas"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = Close(s)
			if dirty {
				if err == nil {
					t.Fatal("staged-only edit was discarded")
				}
				if _, err := os.Stat(closePath(s)); err != nil {
					t.Fatal("receipt lost", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
