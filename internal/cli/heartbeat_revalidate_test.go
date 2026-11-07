package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func reapCandidate(t *testing.T) *session.Session {
	t.Helper()
	root := seedCloseFixture(t, "tele", "work", "sdlc", run.StatusInProgress)
	s, err := session.Open(root, "tele", "work", "design")
	if err != nil {
		t.Fatal(err)
	}
	// A PID above Linux's limit cannot be recycled by the git subprocesses
	// in this test. On other systems, use a completed child and check death.
	pid := 0
	if body, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		limit, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		pid = limit + 1
	} else {
		child := exec.Command(os.Args[0], "-test.run=^$")
		if err := child.Run(); err != nil {
			t.Fatal(err)
		}
		pid = child.Process.Pid
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	writeReapClaim(t, s, session.Claim{
		Branch: s.Branch, Machine: true, Owner: fmt.Sprintf("%s/%d", host, pid),
		PidNS: repolock.PidNamespace(), StartedAt: time.Now().Add(-time.Hour),
		HeartbeatAt: time.Now().Add(-2 * session.StaleAfter),
	})
	if !session.Reapable(s, time.Now()) {
		t.Fatal("fixture must be reapable before selection")
	}
	return s
}

func writeReapClaim(t *testing.T, s *session.Session, c session.Claim) {
	t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, session.ClaimPath(s.Root, s.Project, s.Run, s.Doc), string(body))
}

// pauseSelectedReap joins the reaper on every exit, including failed admission.
// The barrier is outside repolock; admission uses its real locked writer.
func pauseSelectedReap(t *testing.T, g *heartbeatGate, log io.Writer) func() {
	t.Helper()
	selected, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	g.beforeReapLock = func() {
		close(selected)
		<-resume
	}
	var once sync.Once
	finish := func() {
		t.Helper()
		once.Do(func() { close(resume) })
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("reaper did not finish")
		}
	}
	t.Cleanup(finish)
	go func() {
		defer close(done)
		g.reap(log)
	}()
	select {
	case <-selected:
	case <-time.After(15 * time.Second):
		t.Fatal("reaper did not select fixture")
	}
	return finish
}

func TestHeartbeatReapPreservesResumedOwner(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(fmt.Sprintf("machine=%v", machine), func(t *testing.T) {
			s := reapCandidate(t)
			g := newHeartbeatGate(s.Root)
			g.surveyed[s.Project] = "already surveyed"
			var log bytes.Buffer
			finish := pauseSelectedReap(t, g, &log)
			previousWalk := rideWalkActive
			rideWalkActive = machine
			t.Cleanup(func() { rideWalkActive = previousWalk })
			resumed, _, cancel, err := openStageSession(s.Root, stageTurnInputs{
				Project: s.Project, RunSlug: s.Run, DocID: s.Doc,
				LockPurpose: "test-resume", ValidateEntry: sdlcSessionEntry,
			}, io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cancel(); err != nil {
					t.Error(err)
				}
			})
			if resumed.WorktreePath != s.WorktreePath {
				t.Fatal("admission did not resume the selected worktree")
			}
			sentinel := filepath.Join(resumed.WorktreePath, "unsaved-work.txt")
			writeFile(t, sentinel, "keep this uncommitted work\n")
			claim, ok := session.ReadClaim(s.Root, s.Project, s.Run, s.Doc)
			_, pid, parsed := repolock.ParseOwner(claim.Owner)
			if !ok || !parsed || pid != os.Getpid() || claim.Machine != machine || claim.Branch != s.Branch {
				t.Fatalf("admission claim = %+v, ok=%v", claim, ok)
			}
			head := gittest.HeadSHA(t, s.Root)
			ref := gittest.Output(t, s.Root, "rev-parse", s.Branch)
			metadata := mustReadReapFile(t, filepath.Join(s.Root, run.Dir(s.Project, s.Run), "run.json"))
			finish()

			if got := string(mustReadReapFile(t, sentinel)); got != "keep this uncommitted work\n" {
				t.Errorf("unsaved work = %q", got)
			}
			if got := gittest.Output(t, s.Root, "rev-parse", s.Branch); got != ref {
				t.Error("reaper changed the resumed branch")
			}
			if got, ok := session.ReadClaim(s.Root, s.Project, s.Run, s.Doc); !ok || got != claim {
				t.Errorf("reaper changed claim: %+v, ok=%v", got, ok)
			}
			registered, err := session.List(s.Root)
			if err != nil || len(registered) != 1 || registered[0].WorktreePath != resumed.WorktreePath {
				t.Fatalf("registered sessions = %+v, %v", registered, err)
			}
			refs, err := session.RunReservations(s.Root, s.Project, s.Run)
			if err != nil || len(refs) != 1 || refs[0] != s.Branch {
				t.Fatalf("reservations = %+v, %v", refs, err)
			}
			if session.RequireUnreserved(s.Root, s.Project, s.Run) == nil {
				t.Error("run lost its reservation barrier")
			}
			assertSkippedReap(t, g, &log, head, metadata)
		})
	}
}

func mustReadReapFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertSkippedReap(t *testing.T, g *heartbeatGate, log *bytes.Buffer, head string, metadata []byte) {
	t.Helper()
	if got := gittest.HeadSHA(t, g.root); got != head {
		t.Error("skipped reaper committed to main")
	}
	if got := mustReadReapFile(t, filepath.Join(g.root, run.Dir("tele", "work"), "run.json")); !bytes.Equal(got, metadata) {
		t.Error("skipped reaper changed run metadata or wrote a tombstone")
	}
	if g.surveyed["tele"] != "already surveyed" {
		t.Error("skipped reaper cleared surveyed cursor")
	}
	if strings.Contains(log.String(), "landed dead machine session") || strings.Contains(log.String(), "reaped dead machine session") {
		t.Errorf("skipped reaper logged success: %s", log)
	}
}

func TestHeartbeatReapRechecksCandidate(t *testing.T) {
	for _, change := range []string{
		"removed session", "orphan ref", "replacement worktree", "missing claim", "malformed claim",
		"operator claim", "fresh claim", "live claim", "foreign claim", "foreign namespace", "mismatched branch",
	} {
		t.Run(change, func(t *testing.T) {
			s := reapCandidate(t)
			g := newHeartbeatGate(s.Root)
			g.surveyed[s.Project] = "already surveyed"
			head := gittest.HeadSHA(t, s.Root)
			metadata := mustReadReapFile(t, filepath.Join(s.Root, run.Dir(s.Project, s.Run), "run.json"))
			claimPath := session.ClaimPath(s.Root, s.Project, s.Run, s.Doc)
			var claimAfterSelection []byte
			g.beforeReapLock = func() {
				err := repolock.With(s.Root, repolock.Options{Purpose: "test-change-candidate"}, func() error {
					c, _ := session.ReadClaim(s.Root, s.Project, s.Run, s.Doc)
					switch change {
					case "removed session":
						return session.Abandon(s)
					case "orphan ref":
						return git.Run(s.Root, "worktree", "remove", "--force", s.WorktreePath)
					case "replacement worktree":
						replacement := s.WorktreePath + "-replacement"
						if err := git.Run(s.Root, "worktree", "move", s.WorktreePath, replacement); err != nil {
							return err
						}
						s.WorktreePath = replacement
						if !session.Reapable(s, time.Now()) {
							t.Fatal("replacement must also be a dead machine candidate")
						}
					case "missing claim":
						return os.Remove(claimPath)
					case "malformed claim":
						return os.WriteFile(claimPath, []byte("{broken"), 0o644)
					case "operator claim":
						c.Machine = false
					case "fresh claim":
						c.HeartbeatAt = time.Now()
					case "live claim":
						host, err := os.Hostname()
						if err != nil {
							return err
						}
						c.Owner = repolock.OwnerString(host)
					case "foreign claim":
						c.Owner = "another-host/400000"
					case "foreign namespace":
						c.PidNS = "pid:[foreign]"
					case "mismatched branch":
						c.Branch += "-other"
					}
					writeReapClaim(t, s, c)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				claimAfterSelection, _ = os.ReadFile(claimPath)
			}
			var log bytes.Buffer
			g.reap(&log)
			assertSkippedReap(t, g, &log, head, metadata)
			if log.Len() != 0 {
				t.Errorf("skip should be silent: %s", &log)
			}
			if got, _ := os.ReadFile(claimPath); !bytes.Equal(got, claimAfterSelection) {
				t.Error("reaper changed current claim")
			}
			wantRef := change != "removed session"
			if git.HasRef(s.Root, "refs/heads/"+s.Branch) != wantRef {
				t.Error("reaper changed reservation presence")
			}
			if wantRef && change != "orphan ref" {
				registered, err := session.List(s.Root)
				if err != nil || len(registered) != 1 || registered[0].WorktreePath != s.WorktreePath {
					t.Fatalf("current worktree lost: %+v, %v", registered, err)
				}
			}
		})
	}
}

func TestHeartbeatReapLockedListingFailure(t *testing.T) {
	s := reapCandidate(t)
	g := newHeartbeatGate(s.Root)
	g.surveyed[s.Project] = "already surveyed"
	head := gittest.HeadSHA(t, s.Root)
	metadata := mustReadReapFile(t, filepath.Join(s.Root, run.Dir(s.Project, s.Run), "run.json"))
	claimPath := session.ClaimPath(s.Root, s.Project, s.Run, s.Doc)
	claim := mustReadReapFile(t, claimPath)
	ref := gittest.Output(t, s.Root, "rev-parse", s.Branch)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	shim := "#!/bin/sh\nif [ \"$1\" = worktree ] && [ \"$2\" = list ]; then\n echo locked-list-failure >&2\n exit 1\nfi\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	g.beforeReapLock = func() { t.Setenv("PATH", shimDir+string(os.PathListSeparator)+oldPath) }
	var log bytes.Buffer
	g.reap(&log)
	t.Setenv("PATH", oldPath)
	assertSkippedReap(t, g, &log, head, metadata)
	if !strings.Contains(log.String(), "locked-list-failure") || !strings.Contains(log.String(), "left "+s.Branch+" in place") {
		t.Errorf("missing warn-only listing failure: %s", &log)
	}
	if got := mustReadReapFile(t, claimPath); !bytes.Equal(got, claim) {
		t.Error("failed listing changed claim")
	}
	if got := gittest.Output(t, s.Root, "rev-parse", s.Branch); got != ref {
		t.Error("failed listing changed ref")
	}
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Fatal("failed listing removed worktree", err)
	}
}
