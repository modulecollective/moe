package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func uncertainGCClaim(t *testing.T, s *session.Session, kind string) {
	t.Helper()
	path := session.ClaimPath(s.Root, s.Project, s.Run, s.Doc)
	switch kind {
	case "malformed":
		writeFile(t, path, `{"branch":`)
	case "unreadable":
		if os.Geteuid() == 0 {
			t.Skip("requires non-root permission checks")
		}
		writeDeadClaim(t, s, false)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		if _, err := os.ReadFile(path); !os.IsPermission(err) {
			t.Fatalf("fixture must fail to read with permission denied, got %v", err)
		}
	case "mismatched":
		writeDeadClaim(t, s, false)
		c, ok := session.ReadClaim(s.Root, s.Project, s.Run, s.Doc)
		if !ok {
			t.Fatal("missing dead claim")
		}
		_, pid, ok := repolock.ParseOwner(c.Owner)
		if !ok || repolock.ProcessAlive(pid) || !session.Dead(s.Root, s.Project, s.Run, s.Doc, time.Now()) {
			t.Fatal("matching fixture must prove death before changing branch")
		}
		c.PidNS = repolock.PidNamespace()
		c.Branch += "-other"
		writeReapClaim(t, s, c)
	case "live":
		host, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		writeReapClaim(t, s, session.Claim{
			Branch: s.Branch, Owner: fmt.Sprintf("%s/%d", host, os.Getpid()),
			PidNS: repolock.PidNamespace(), HeartbeatAt: time.Now().Add(-2 * session.StaleAfter),
		})
	default:
		t.Fatalf("unknown fixture %q", kind)
	}
}

// Snapshot both Git's registration and the operator's uncommitted bytes.
func assertGCSessionRetained(t *testing.T, s *session.Session) func() {
	t.Helper()
	tip := gittest.Output(t, s.Root, "rev-parse", s.Branch)
	path := session.ClaimPath(s.Root, s.Project, s.Run, s.Doc)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Temporarily restore permission only to snapshot the unreadable fixture.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	claim, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, info.Mode()); err != nil {
		t.Fatal(err)
	}
	if s.WorktreePath != "" {
		writeFile(t, filepath.Join(s.WorktreePath, "uncommitted-sentinel"), "keep this work\n")
	}
	return func() {
		t.Helper()
		if got := gittest.Output(t, s.Root, "rev-parse", s.Branch); got != tip {
			t.Errorf("branch tip changed: %s -> %s", tip, got)
		}
		after, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("claim lost: %v", err)
		}
		if after.Mode() != info.Mode() {
			t.Errorf("claim mode changed: %v -> %v", info.Mode(), after.Mode())
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(body, claim) {
			t.Errorf("claim bytes changed: %q, %v", body, err)
		}
		if err := os.Chmod(path, info.Mode()); err != nil {
			t.Fatal(err)
		}
		found, err := session.FindByBranch(s.Root, s.Branch)
		if err != nil || found == nil || found.WorktreePath != s.WorktreePath {
			t.Fatalf("worktree registration changed: %+v, %v", found, err)
		}
		if s.WorktreePath != "" {
			body, err := os.ReadFile(filepath.Join(s.WorktreePath, "uncommitted-sentinel"))
			if err != nil || string(body) != "keep this work\n" {
				t.Errorf("uncommitted work lost: %q, %v", body, err)
			}
		}
	}
}

func TestSessionGCHoldsUncertainClaims(t *testing.T) {
	for rule := 1; rule <= 4; rule++ {
		for _, kind := range []string{"malformed", "unreadable", "mismatched"} {
			t.Run(fmt.Sprintf("rule%d/%s", rule, kind), func(t *testing.T) {
				root := newSessionTestRoot(t)
				trailerstest.SeedRun(t, root, "alpha", "held", "sdlc", run.StatusClosed)
				s := &session.Session{Root: root, Project: "alpha", Run: "held", Doc: "design", Branch: session.BranchName("alpha", "held", "design")}
				if rule == 4 {
					gittest.Run(t, root, "branch", s.Branch)
				} else {
					var err error
					s, err = session.Open(root, "alpha", "held", "design")
					if err != nil {
						t.Fatal(err)
					}
				}
				if rule == 2 {
					if err := os.Remove(filepath.Join(root, "projects/alpha/runs/held/run.json")); err != nil {
						t.Fatal(err)
					}
				}
				if rule == 3 {
					if err := os.RemoveAll(filepath.Join(root, "projects/alpha")); err != nil {
						t.Fatal(err)
					}
				}
				uncertainGCClaim(t, s, kind)
				retained := assertGCSessionRetained(t, s)
				// A held session must not suppress unrelated claimless cleanup.
				eligible := "session/beta/absent/design"
				gittest.Run(t, root, "branch", eligible)
				code, stdout, stderr := runSessionGCInRoot(t, root)
				if code != 0 || stderr != "" || strings.TrimSpace(stdout) != "removed "+eligible {
					t.Fatalf("gc exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if git.HasRef(root, "refs/heads/"+eligible) {
					t.Fatal("eligible branch survived")
				}
				retained()
				if rule != 4 {
					var out, errs bytes.Buffer
					if code := Run([]string{"session", "list"}, &out, &errs); code != 0 || !strings.Contains(out.String(), s.Branch) {
						t.Fatalf("held session not reachable in list: exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
					}
				}
				// The fixtures have no running owner. Explicit discard remains
				// reachable after inspecting held work, without deleting the claim.
				var out, errs bytes.Buffer
				if code := Run([]string{"session", "abandon", s.Branch}, &out, &errs); code != 0 || !strings.Contains(out.String(), "abandoned") {
					t.Fatalf("explicit abandon: exit=%d stdout=%q stderr=%q", code, out.String(), errs.String())
				}
				if git.HasRef(root, "refs/heads/"+s.Branch) {
					t.Error("explicit abandon retained branch")
				}
				if _, err := os.Lstat(session.ClaimPath(root, s.Project, s.Run, s.Doc)); !os.IsNotExist(err) {
					t.Errorf("explicit abandon retained claim: %v", err)
				}
				if s.WorktreePath != "" {
					if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
						t.Errorf("explicit abandon retained worktree: %v", err)
					}
				}
			})
		}
	}
}

func TestSessionGCRevalidatesClaims(t *testing.T) {
	for _, initial := range []string{"absent", "dead"} {
		for _, replacement := range []string{"malformed", "live"} {
			t.Run(initial+"/"+replacement, func(t *testing.T) {
				root := newSessionTestRoot(t)
				s, err := session.Open(root, "alpha", "missing-run", "design")
				if err != nil {
					t.Fatal(err)
				}
				if initial == "dead" {
					writeDeadClaim(t, s, false)
				}
				candidates, err := collectSessionOrphans(root, time.Now())
				if err != nil || len(candidates.sessions) != 1 {
					t.Fatalf("pre-scan: %+v, %v", candidates, err)
				}
				uncertainGCClaim(t, s, replacement)
				retained := assertGCSessionRetained(t, s)
				removed, errs, err := sessionGCPass(root, repolock.Options{Purpose: "test-gc-revalidate"})
				if err != nil || len(errs) != 0 || len(removed) != 0 {
					t.Fatalf("locked pass: removed=%v errors=%v err=%v", removed, errs, err)
				}
				retained()
			})
		}
	}
}

func TestHeartbeatGCHoldsUncertainClaims(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "mismatched", "live"} {
		for _, registered := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/registered=%v", kind, registered), func(t *testing.T) {
				root := parallelQuietFixture(t)
				groomFixture(t, root, "fix-a")
				g := sweptOnceOverParkedWork(t, root, true)
				cursor := g.surveyed["moe"]
				if cursor == "" {
					t.Fatal("fixture needs a surveyed cursor")
				}
				seedRun(t, root, "moe", "finished", "sdlc", run.StatusClosed, time.Now(), nil)
				s := &session.Session{Root: root, Project: "moe", Run: "finished", Doc: "design", Branch: session.BranchName("moe", "finished", "design")}
				if registered {
					var err error
					s, err = session.Open(root, "moe", "finished", "design")
					if err != nil {
						t.Fatal(err)
					}
				} else {
					gittest.Run(t, root, "branch", s.Branch)
				}
				uncertainGCClaim(t, s, kind)
				retained := assertGCSessionRetained(t, s)
				var log bytes.Buffer
				if ids := sweepIDs(g.Due(testTick, &log)); len(ids) != 0 {
					t.Errorf("occupied project offered: %v", ids)
				}
				if log.Len() != 0 {
					t.Errorf("held session reported as removed/failed: %s", log.String())
				}
				if g.surveyed["moe"] != cursor {
					t.Errorf("held session cleared surveyed cursor")
				}
				if !openSessionProjects(root, time.Now())["moe"] {
					t.Error("surviving ref did not hold occupancy")
				}
				retained()
			})
		}
	}
}
