package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestRunReservationsAreExactStrictAndIndependentOfWorktrees(t *testing.T) {
	root := newTestRoot(t)
	for _, branch := range []string{"session/moe/r1/z-retired", "session/moe/r1/code", "session/moe/r10/code", "session/other/r1/code"} {
		gittest.Run(t, root, "branch", branch)
	}
	refs, err := RunReservations(root, "moe", "r1")
	if err != nil || strings.Join(refs, ",") != "session/moe/r1/code,session/moe/r1/z-retired" {
		t.Fatalf("refs %v err %v", refs, err)
	}
	if err := RequireUnreserved(root, "moe", "r1"); err == nil {
		t.Fatal("orphan refs were ignored")
	}
	if err := RequireUnreserved(root, "moe", "r2"); err != nil {
		t.Fatal(err)
	}
	if _, err := RunReservations(t.TempDir(), "moe", "r1"); err == nil {
		t.Fatal("failed ref read became empty reservations")
	}
}

func TestReserveRequiresMatchingDeadClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Session)
		allow bool
	}{
		{"missing", func(t *testing.T, s *Session) {}, false},
		{"malformed", func(t *testing.T, s *Session) {
			p := ClaimPath(s.Root, s.Project, s.Run, s.Doc)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("oops"), 0600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"live operator", func(t *testing.T, s *Session) {
			release, err := Hold(s, false)
			if err != nil {
				t.Fatal(err)
			}
			release()
		}, false},
		{"live machine", func(t *testing.T, s *Session) {
			release, err := Hold(s, true)
			if err != nil {
				t.Fatal(err)
			}
			release()
		}, false},
		{"fresh dead", func(t *testing.T, s *Session) { deadClaim(t, s, false, time.Minute) }, false},
		{"stale dead operator", func(t *testing.T, s *Session) { deadClaim(t, s, false, 2*StaleAfter) }, true},
		{"stale dead machine", func(t *testing.T, s *Session) { deadClaim(t, s, true, 2*StaleAfter) }, true},
		{"stale live", func(t *testing.T, s *Session) {
			release, err := Hold(s, true)
			if err != nil {
				t.Fatal(err)
			}
			release()
			c, _ := ReadClaim(s.Root, s.Project, s.Run, s.Doc)
			c.HeartbeatAt = time.Now().Add(-2 * StaleAfter)
			writeClaimOrFail(t, s, c)
		}, false},
		{"mismatched", func(t *testing.T, s *Session) {
			deadClaim(t, s, false, 2*StaleAfter)
			c, _ := ReadClaim(s.Root, s.Project, s.Run, s.Doc)
			c.Branch = "session/moe/r1/other"
			writeClaimOrFail(t, s, c)
		}, false},
		{"foreign host", func(t *testing.T, s *Session) {
			deadClaim(t, s, false, 2*StaleAfter)
			c, _ := ReadClaim(s.Root, s.Project, s.Run, s.Doc)
			c.Owner = "foreign-host/1"
			writeClaimOrFail(t, s, c)
		}, false},
		{"foreign namespace", func(t *testing.T, s *Session) {
			deadClaim(t, s, false, 2*StaleAfter)
			c, _ := ReadClaim(s.Root, s.Project, s.Run, s.Doc)
			c.PidNS = "foreign-namespace"
			writeClaimOrFail(t, s, c)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "code")
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, s)
			// Refusal must occur before Open reconciles a stale worktree.
			before := gittest.Output(t, s.WorktreePath, "rev-parse", "HEAD")
			commitInWorktree(t, root, "new-main.txt", "new main", "move main")
			path := ClaimPath(root, "moe", "r1", "code")
			claimBefore, _ := os.ReadFile(path)
			got, release, err := Reserve(root, "moe", "r1", "code", false)
			if tc.allow {
				if err != nil {
					t.Fatal(err)
				}
				release()
				if got.WorktreePath != s.WorktreePath {
					t.Fatal("did not resume")
				}
				if gittest.Output(t, s.WorktreePath, "rev-parse", "HEAD") == before {
					t.Fatal("resume did not reconcile")
				}
			} else {
				var conflict *UnresolvedError
				if !errors.As(err, &conflict) {
					t.Fatalf("error %v", err)
				}
				if gittest.Output(t, s.WorktreePath, "rev-parse", "HEAD") != before {
					t.Fatal("refused admission reconciled work")
				}
				after, _ := os.ReadFile(path)
				if string(after) != string(claimBefore) {
					t.Fatal("refused admission replaced claim")
				}
			}
			// Every remaining reservation blocks mutation, including dead claims.
			if err := RequireUnreserved(root, "moe", "r1"); err == nil {
				t.Fatal("mutation admitted")
			}
		})
	}
}

func TestReserveOtherDocumentAndClaimFailure(t *testing.T) {
	root := newTestRoot(t)
	s, release, err := Reserve(root, "moe", "r1", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	deadClaim(t, s, false, 2*StaleAfter)
	if _, _, err := Reserve(root, "moe", "r1", "code", false); err == nil {
		t.Fatal("other document admitted")
	}
	// Make initial claim publication fail after fresh worktree creation.
	root2 := newTestRoot(t)
	p := filepath.Join(root2, ".moe", "claims")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("file blocks directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reserve(root2, "moe", "r1", "code", false); err == nil {
		t.Fatal("claim failure admitted")
	}
	if err := RequireUnreserved(root2, "moe", "r1"); err != nil {
		t.Fatal("fresh reservation leaked:", err)
	}
}

func TestCancelReservationRetainsResumedClaimAndDirtyWork(t *testing.T) {
	root := newTestRoot(t)
	s, stop, err := Reserve(root, "moe", "r1", "code", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	deadClaim(t, s, false, 2*StaleAfter)
	if err := os.WriteFile(filepath.Join(s.WorktreePath, "saved"), []byte("work"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed, stop, err := Reserve(root, "moe", "r1", "code", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	before, _ := os.ReadFile(ClaimPath(root, "moe", "r1", "code"))
	if err := CancelReservation(resumed); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(ClaimPath(root, "moe", "r1", "code"))
	if len(after) == 0 || string(before) != string(after) {
		t.Fatal("lost resumed claim")
	}
	body, err := os.ReadFile(filepath.Join(s.WorktreePath, "saved"))
	if err != nil || string(body) != "work" {
		t.Fatal("lost resumed work")
	}
}
