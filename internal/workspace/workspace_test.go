package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// seedSrc builds a minimal `projects/<id>/src/` submodule-like repo so
// Ensure has something to clone. A single committed file plus a main
// branch is enough; the gitfile-style-vs-plain split is exercised in
// sandbox_test.go.
func seedSrc(t *testing.T, root, projectID string) string {
	t.Helper()
	src := filepath.Join(root, "projects", projectID, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, src)
	gittest.WriteAndCommit(t, src, "README.md", "seed\n", "seed")
	return src
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		in  string
		bad bool
	}{
		{"", true},
		{"dev", false},
		{"DEV", true},
		{"my-workspace", false},
		{"my_workspace", true},
		{"./escape", true},
		{"-leading-dash", true},
		{"trailing-dash-", false}, // pattern allows; not a problem in practice
	}
	for _, c := range cases {
		err := ValidateName(c.in)
		if c.bad && err == nil {
			t.Errorf("ValidateName(%q) accepted; want error", c.in)
		}
		if !c.bad && err != nil {
			t.Errorf("ValidateName(%q) rejected: %v", c.in, err)
		}
	}
}

// TestEnsureCreatesAndIsIdempotent confirms first-call clones the
// source submodule into .moe/named/<project>/<name>/, and the second
// call is a stat that returns the same path without re-cloning.
func TestEnsureCreatesAndIsIdempotent(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !filepath.IsAbs(wp) {
		t.Fatalf("Ensure returned non-absolute path: %s", wp)
	}
	want := filepath.Join(root, ".moe", "named", "tele", "dev")
	if got, err := filepath.EvalSymlinks(wp); err == nil {
		// EvalSymlinks resolves any temp-dir-on-tmp aliasing on macOS so
		// the comparison doesn't false-positive on /private/var vs /var.
		if wantResolved, _ := filepath.EvalSymlinks(want); wantResolved != "" && got != wantResolved {
			t.Fatalf("Ensure path = %s, want %s", got, wantResolved)
		}
	}

	// Second call returns the same path without re-cloning.
	wp2, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatalf("Ensure (reuse): %v", err)
	}
	if wp2 != wp {
		t.Fatalf("Ensure path drifted: %s vs %s", wp, wp2)
	}

	// The clone is a working git repo with the seed file.
	if got, err := os.ReadFile(filepath.Join(wp, "README.md")); err != nil || string(got) != "seed\n" {
		t.Fatalf("README.md: got=%q err=%v", got, err)
	}
}

// TestAcquireAndRelease covers the happy path: a workspace claimed by
// run A, re-acquired by run A (idempotent), released, then re-claimed
// by run B.
func TestAcquireAndRelease(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	wp, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire run-a: %v", err)
	}
	c, err := ReadClaim(root, "tele", "dev")
	if err != nil || c == nil || c.Run != "tele/run-a" {
		t.Fatalf("ReadClaim after Acquire run-a: %+v err=%v", c, err)
	}

	// Re-acquire by the same run is a no-op.
	wp2, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("re-Acquire run-a: %v", err)
	}
	if wp2 != wp {
		t.Fatalf("Acquire path drifted: %s vs %s", wp, wp2)
	}

	// A different run is refused while the claim is held.
	if _, err := Acquire(root, "tele", "dev", "tele/run-b"); err == nil {
		t.Fatal("Acquire run-b should refuse while run-a holds the claim")
	} else {
		var ace *AlreadyClaimedError
		if !errors.As(err, &ace) {
			t.Fatalf("expected *AlreadyClaimedError, got %T: %v", err, err)
		}
		if ace.Holder.Run != "tele/run-a" {
			t.Fatalf("AlreadyClaimedError.Holder.Run = %q, want tele/run-a", ace.Holder.Run)
		}
		if !errors.Is(err, ErrAlreadyClaimed) {
			t.Fatalf("errors.Is(err, ErrAlreadyClaimed) = false")
		}
	}

	if err := Release(root, "tele", "dev"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	c, err = ReadClaim(root, "tele", "dev")
	if err != nil {
		t.Fatalf("ReadClaim after Release: %v", err)
	}
	if c != nil {
		t.Fatalf("ReadClaim after Release should be nil, got %+v", c)
	}

	// Release on no-claim is idempotent.
	if err := Release(root, "tele", "dev"); err != nil {
		t.Fatalf("Release (idempotent): %v", err)
	}

	// A fresh run can now claim it.
	if _, err := Acquire(root, "tele", "dev", "tele/run-b"); err != nil {
		t.Fatalf("Acquire run-b after release: %v", err)
	}
}

// TestReleaseOnMissingWorkspaceIsNoOp covers the case where a run was
// opened with --workspace dev but never reached `sdlc code` (so the
// workspace was never created). Close should still succeed.
func TestReleaseOnMissingWorkspaceIsNoOp(t *testing.T) {
	root := t.TempDir()
	if err := Release(root, "tele", "ghost"); err != nil {
		t.Fatalf("Release on missing workspace: %v", err)
	}
}

// TestAttachRefusesDirty is the load-bearing handoff guarantee: a
// previous run that left an uncommitted edit must surface as an error,
// not be silently overwritten by the next run's checkout.
func TestAttachRefusesDirty(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	// Create the run-a branch and commit on it, then leave a stray edit.
	gittest.Run(t, wp, "checkout", "-b", "moe/run-a")
	if err := os.WriteFile(filepath.Join(wp, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "code.txt")
	gittest.Run(t, wp, "commit", "-m", "v1")
	// Stray uncommitted edit.
	if err := os.WriteFile(filepath.Join(wp, "code.txt"), []byte("v2-uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = Attach(wp, "moe/run-b", "main")
	if err == nil {
		t.Fatal("Attach should refuse a dirty workspace")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error should mention uncommitted: %v", err)
	}
}

// TestAttachCreatesNewBranchOffBase guards against the silent-inherit
// failure mode: when a new run attaches to a workspace previously on
// run-a's branch, the new branch must be anchored to the project
// default, not to run-a's tip.
func TestAttachCreatesNewBranchOffBase(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	mainSHA := gittest.Output(t, wp, "rev-parse", "HEAD")

	// Run A: branches off main, lands a commit, leaves a clean tree.
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach run-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wp, "code.txt"), []byte("from-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "code.txt")
	gittest.Run(t, wp, "commit", "-m", "a-only")
	aTipSHA := gittest.Output(t, wp, "rev-parse", "HEAD")
	if aTipSHA == mainSHA {
		t.Fatal("setup: run-a tip should differ from main")
	}

	// Run B: attaches; must anchor moe/run-b to main, NOT to a-only.
	if err := Attach(wp, "moe/run-b", "main"); err != nil {
		t.Fatalf("Attach run-b: %v", err)
	}
	bTipSHA := gittest.Output(t, wp, "rev-parse", "HEAD")
	if bTipSHA != mainSHA {
		t.Fatalf("moe/run-b should branch off main (%s), got %s (= run-a tip %s)",
			mainSHA, bTipSHA, aTipSHA)
	}
	// And run-a's commit is not visible from run-b.
	if _, err := os.Stat(filepath.Join(wp, "code.txt")); err == nil {
		t.Fatal("run-a's code.txt leaked into run-b's working tree")
	}
}

// TestAttachReusesExistingBranch covers the `sdlc code` second-turn
// path: the run's branch already exists from turn 1, so Attach should
// just check it out without a base-branch detour.
func TestAttachReusesExistingBranch(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach run-a (turn 1): %v", err)
	}
	if err := os.WriteFile(filepath.Join(wp, "x"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "x")
	gittest.Run(t, wp, "commit", "-m", "a")
	want := gittest.Output(t, wp, "rev-parse", "HEAD")
	// Switch off the run-a branch, then re-attach on turn 2. We
	// detach rather than `checkout main` because main is checked out
	// in the canonical submodule and worktrees can't share a branch.
	gittest.Run(t, wp, "checkout", "--detach", "main")
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach run-a (turn 2): %v", err)
	}
	got := gittest.Output(t, wp, "rev-parse", "HEAD")
	if got != want {
		t.Fatalf("Attach reuse drifted: want %s got %s", want, got)
	}
}

// TestAcquireThenAttachOnFreshWorkspace is the load-bearing regression:
// re-using a named workspace for a fresh run did `Acquire` (writes the
// claim) then `Attach` (calls `refuseDirty` → `git.Status`) and Attach
// errored because the claim file at the workspace root surfaced as `??`.
// With the claim folded under `.moe/` (covered by the clone's
// `.git/info/exclude`), Attach must succeed and `git.Status` must
// return no entries.
func TestAcquireThenAttachOnFreshWorkspace(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	wp, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach after Acquire: %v", err)
	}
	entries, err := git.Status(wp)
	if err != nil {
		t.Fatalf("git.Status: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("git.Status should be empty after Acquire+Attach, got %+v", entries)
	}
}

// TestAcquireWritesClaimUnderDotMoe nails the new on-disk layout: the
// claim must land at `<wp>/.moe/claim.json`, and `<wp>/claim.json` must
// not exist. This is what stops the file from being flagged by
// `git.Status` (and therefore by `refuseDirty` and the `Dirty` Info
// field).
func TestAcquireWritesClaimUnderDotMoe(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	wp, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wp, ".moe", "claim.json")); err != nil {
		t.Fatalf("claim should live under .moe/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wp, "claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy <wp>/claim.json must not exist, got err=%v", err)
	}
}

// TestReadClaimMigratesLegacyLayout covers the in-the-wild workspaces
// that already carry `<wp>/claim.json` from before this fix. The next
// read must (a) return the holder, (b) leave the claim at the new
// path, (c) remove the legacy file.
func TestReadClaimMigratesLegacyLayout(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	// Materialize the workspace dir but skip Acquire — we want a clean
	// slate to seed the legacy claim directly.
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	seeded := Claim{Project: "tele", Name: "dev", Run: "tele/legacy-run"}
	b, err := json.MarshalIndent(seeded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(wp, "claim.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadClaim(root, "tele", "dev")
	if err != nil {
		t.Fatalf("ReadClaim: %v", err)
	}
	if got == nil || *got != seeded {
		t.Fatalf("ReadClaim returned %+v, want %+v", got, seeded)
	}
	// Migrated to the new path.
	migrated, err := os.ReadFile(filepath.Join(wp, ".moe", "claim.json"))
	if err != nil {
		t.Fatalf("new-layout claim missing after migration: %v", err)
	}
	if string(migrated) != string(b) {
		t.Fatalf("migrated claim contents drifted:\n got=%q\nwant=%q", migrated, b)
	}
	// Legacy file is gone.
	if _, err := os.Stat(filepath.Join(wp, "claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy <wp>/claim.json should be removed, got err=%v", err)
	}
}

// TestReleaseCleansBothLocations seeds a stale legacy claim alongside
// the current-layout claim (the half-migrated state that could exist
// if a run wrote the legacy file and an Acquire-by-the-same-run later
// wrote the new one without an intervening read). Release must wipe
// both so a fresh Acquire doesn't resurrect the legacy file.
func TestReleaseCleansBothLocations(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	wp, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Seed a stale legacy claim alongside the new one.
	if err := os.WriteFile(filepath.Join(wp, "claim.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Release(root, "tele", "dev"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wp, ".moe", "claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new-layout claim should be gone after Release, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(wp, "claim.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy claim should be gone after Release, got err=%v", err)
	}
}
