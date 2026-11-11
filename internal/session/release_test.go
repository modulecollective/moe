package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestReleaseAllowsOnlySameDocumentResume(t *testing.T) {
	root := newTestRoot(t)
	s, stop, err := Reserve(root, "moe", "r1", "design", true)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	c, _ := ReadClaim(root, "moe", "r1", "design")
	if !c.ReleasedAt.IsZero() {
		t.Fatal("stopping heartbeat released a still-mutating owner")
	}
	if _, _, err := Reserve(root, "moe", "r1", "design", true); err == nil {
		t.Fatal("unreleased owner replaced")
	}
	if err := ReleaseOwnership(s); err != nil {
		t.Fatal(err)
	}
	if Reapable(s, time.Now()) {
		t.Fatal("release broadened machine reaping")
	}
	if _, _, err := Reserve(root, "moe", "r1", "code", true); err == nil {
		t.Fatal("release admitted another document")
	}
	resumed, stop2, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stop2()
	if resumed.WorktreePath != s.WorktreePath {
		t.Fatal("did not preserve worktree")
	}
	before, err := os.ReadFile(ClaimPath(root, "moe", "r1", "design"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwnership(s); err == nil {
		t.Fatal("old owner released replacement")
	}
	after, err := os.ReadFile(ClaimPath(root, "moe", "r1", "design"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("late release changed generation")
	}
	if err := Abandon(resumed); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwnership(resumed); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadClaim(root, "moe", "r1", "design"); ok {
		t.Fatal("release recreated removed claim")
	}
}

func TestReserveSettlesLandedReceiptBeforeNewTurn(t *testing.T) {
	root := newTestRoot(t)
	s, stop, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	main := gittest.Output(t, root, "rev-parse", "main")
	commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
	tip := gittest.Output(t, root, "rev-parse", s.Branch)
	if err := writeClose(s, tip, main); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "merge", "--ff-only", s.Branch)
	if err := os.Remove(filepath.Join(s.WorktreePath, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseOwnership(s); err != nil {
		t.Fatal(err)
	}
	next, stopNext, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stopNext()
	if next.WorktreePath == s.WorktreePath {
		t.Fatal("landed tree reused as unfinished work")
	}
	if len(next.RecoveryPaths) != 1 {
		t.Fatal("preservation path not reported")
	}
	if got := gittest.Output(t, root, "rev-parse", "main"); got != tip {
		t.Fatal("cleanup changed history")
	}
	if err := Close(next); err == nil {
		t.Fatal("fresh no-op bypassed canvas gate")
	}
}

func TestReleasedClaimsStillRefuseForeignAndMalformedOwners(t *testing.T) {
	for _, shape := range []string{"host", "namespace", "owner", "generation"} {
		t.Run(shape, func(t *testing.T) {
			root := newTestRoot(t)
			s, stop, err := Reserve(root, "moe", "r1", "design", false)
			if err != nil {
				t.Fatal(err)
			}
			stop()
			if err := ReleaseOwnership(s); err != nil {
				t.Fatal(err)
			}
			c, _ := ReadClaim(root, "moe", "r1", "design")
			switch shape {
			case "host":
				c.Owner = "another-host/1234"
			case "namespace":
				c.PidNS = "another-namespace"
			case "owner":
				c.Owner = "unknown"
			case "generation":
				c.StartedAt = time.Time{}
			}
			writeClaimOrFail(t, s, c)
			if _, _, err := Reserve(root, "moe", "r1", "design", false); err == nil {
				t.Fatal("ambiguous released claim accepted")
			}
		})
	}
}

func TestRecoveryRefusesUnsafePathsAndFailedPreservation(t *testing.T) {
	for _, shape := range []string{"unmanaged", "symlink", "wrong-branch", "preservation", "preservation-symlink"} {
		t.Run(shape, func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			commitInWorktree(t, s.WorktreePath, canvasRel, "saved canvas", "work: update design")
			gittest.Run(t, root, "merge", "--ff-only", s.Branch)
			original := s.WorktreePath
			switch shape {
			case "unmanaged":
				s.WorktreePath = t.TempDir()
			case "symlink":
				moved := original + "-moved"
				if err := os.Rename(original, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, original); err != nil {
					t.Fatal(err)
				}
			case "wrong-branch":
				gittest.Run(t, s.WorktreePath, "checkout", "-b", "other")
			case "preservation", "preservation-symlink":
				if err := os.Remove(filepath.Join(original, ".git")); err != nil {
					t.Fatal(err)
				}
				if shape == "preservation-symlink" {
					if err := os.Symlink(filepath.Join(root, ".moe", "worktrees"), filepath.Join(root, ".moe", "session-recovery")); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(root, ".moe", "session-recovery"), []byte("block preservation"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Resolve(s); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if !branchExists(root, s.Branch) {
				t.Fatal("branch lost")
			}
			if shape == "preservation" || shape == "preservation-symlink" {
				if b, err := os.ReadFile(filepath.Join(original, canvasRel)); err != nil || string(b) != "saved canvas" {
					t.Fatalf("failed preservation lost original: %s %v", b, err)
				}
			}
		})
	}
}

func TestHeartbeatCannotRecreateOrReplaceClaim(t *testing.T) {
	root := newTestRoot(t)
	s, stop, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	owner, _ := ReadClaim(root, "moe", "r1", "design")
	if err := beatClaim(s, owner); err != nil {
		t.Fatal(err)
	}
	after, _ := ReadClaim(root, "moe", "r1", "design")
	if !after.HeartbeatAt.After(owner.HeartbeatAt) {
		t.Fatal("positive heartbeat control did not write")
	}
	if err := clearClaim(s); err != nil {
		t.Fatal(err)
	}
	if err := beatClaim(s, owner); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadClaim(root, "moe", "r1", "design"); ok {
		t.Fatal("heartbeat recreated deleted claim")
	}
	replacement := owner
	replacement.StartedAt = owner.StartedAt.Add(time.Second)
	writeClaimOrFail(t, s, replacement)
	if err := beatClaim(s, owner); err != nil {
		t.Fatal(err)
	}
	after, _ = ReadClaim(root, "moe", "r1", "design")
	if after != replacement {
		t.Fatal("heartbeat clobbered replacement generation")
	}
}

func TestFailedReleaseKeepsCrashAdmissionRules(t *testing.T) {
	root := newTestRoot(t)
	s, stop, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	dir := filepath.Dir(ClaimPath(root, "moe", "r1", "design"))
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	err = ReleaseOwnership(s)
	if restoreErr := os.Chmod(dir, 0700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("release write failure ignored")
	}
	c, ok := ReadClaim(root, "moe", "r1", "design")
	if !ok || !c.ReleasedAt.IsZero() {
		t.Fatal("failed release published relinquishment")
	}
	if _, _, err := Reserve(root, "moe", "r1", "design", false); err == nil {
		t.Fatal("failed release allowed takeover")
	}
}
