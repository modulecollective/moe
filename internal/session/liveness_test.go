package session

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
)

// exitedPid runs a trivial child to completion and returns its pid — a
// same-host pid that is genuinely gone, which is the only way to stage
// the "provably dead" leg without inventing a number the kernel might
// hand out. The child is this test binary with a filter that matches no
// test, so it needs nothing on PATH and exits immediately.
func exitedPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run throwaway child: %v", err)
	}
	return cmd.Process.Pid
}

// deadClaim writes a record by hand for a same-host pid that has
// already exited.
func deadClaim(t *testing.T, s *Session, machine bool, beatAge time.Duration) {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this box; the same-host leg can't be staged")
	}
	c := Claim{
		Branch:      s.Branch,
		Machine:     machine,
		Owner:       fmt.Sprintf("%s/%d", host, exitedPid(t)),
		StartedAt:   time.Now().Add(-time.Hour).UTC(),
		HeartbeatAt: time.Now().Add(-beatAge).UTC(),
	}
	if err := writeClaim(ClaimPath(s.Root, s.Project, s.Run, s.Doc), c); err != nil {
		t.Fatal(err)
	}
}

// TestHoldMarksAndClose Clears: the record's whole contract is that it
// outlives its process only when the session ended badly. A clean Close
// takes it with the branch, so a surviving record is exactly an orphan.
func TestHoldMarksAndCloseClears(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatal(err)
	}
	release, err := Hold(s, true /*machine*/)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	c, ok := ReadClaim(root, "moe", "r1", "design")
	if !ok || !c.Machine {
		t.Fatalf("claim = %+v ok=%v, want a machine-marked record", c, ok)
	}
	if _, pid, parsed := repolock.ParseOwner(c.Owner); !parsed || pid != os.Getpid() {
		t.Errorf("owner = %q, want this process", c.Owner)
	}
	release()

	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md", "written", "work: update design")
	if err := Close(s); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := ReadClaim(root, "moe", "r1", "design"); ok {
		t.Error("claim survived a clean close; a leftover record reads as an orphan")
	}
}

func TestAbandonClearsTheClaim(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatal(err)
	}
	release, err := Hold(s, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := Abandon(s); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if _, ok := ReadClaim(root, "moe", "r1", "design"); ok {
		t.Error("claim survived abandon")
	}
}

// TestReapableOnlyForProvablyDeadMachineSessions is the operator's rule
// rendered as a table: a robot session that died may be abandoned and
// retried; a human-started one may not, and neither may anything
// ambiguous. Every row that isn't the first must be false — that is the
// direction where a wrong answer destroys a live session's worktree.
func TestReapableOnlyForProvablyDeadMachineSessions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Session)
		want  bool
	}{
		{
			name:  "dead machine session",
			setup: func(t *testing.T, s *Session) { deadClaim(t, s, true, 2*StaleAfter) },
			want:  true,
		},
		{
			name:  "dead operator session",
			setup: func(t *testing.T, s *Session) { deadClaim(t, s, false, 2*StaleAfter) },
			want:  false,
		},
		{
			name:  "no claim at all",
			setup: func(t *testing.T, s *Session) {},
			want:  false,
		},
		{
			name:  "machine session with a fresh heartbeat",
			setup: func(t *testing.T, s *Session) { deadClaim(t, s, true, time.Minute) },
			want:  false,
		},
		{
			name: "live machine session — an operator's own `!!!` pane",
			setup: func(t *testing.T, s *Session) {
				release, err := Hold(s, true)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
			},
			want: false,
		},
		{
			name: "claim owned by another host",
			setup: func(t *testing.T, s *Session) {
				c := Claim{
					Branch: s.Branch, Machine: true, Owner: "some-other-box/0",
					HeartbeatAt: time.Now().Add(-2 * StaleAfter).UTC(),
				}
				if err := writeClaim(ClaimPath(s.Root, s.Project, s.Run, s.Doc), c); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			name: "unparseable owner",
			setup: func(t *testing.T, s *Session) {
				c := Claim{
					Branch: s.Branch, Machine: true, Owner: "garbage",
					HeartbeatAt: time.Now().Add(-2 * StaleAfter).UTC(),
				}
				if err := writeClaim(ClaimPath(s.Root, s.Project, s.Run, s.Doc), c); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestRoot(t)
			s, err := Open(root, "moe", "r1", "design")
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, s)
			if got := Reapable(s, time.Now()); got != tc.want {
				t.Errorf("Reapable = %v, want %v", got, tc.want)
			}
		})
	}
}
