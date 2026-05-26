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

// allowFFPushIntoCanonical configures the canonical src to accept a
// push into its currently-checked-out main. The fixture canonical is
// a working repo (not bare), so git's default `denyCurrentBranch=refuse`
// would block the ff-push that simulates a real merge. The canonical's
// worktree becoming inconsistent after the push is harmless — the
// fixture never reads it again — and the alternative (advancing main
// via WriteAndCommit then back-fetching) would diverge from what the
// production push path actually does.
func allowFFPushIntoCanonical(t *testing.T, root, projectID string) {
	t.Helper()
	src := filepath.Join(root, "projects", projectID, "src")
	gittest.Run(t, src, "config", "receive.denyCurrentBranch", "ignore")
}

// TestResetToDefaultParksOnDefaultAfterMerge is the load-bearing case:
// after a push-merge fast-forward (simulated here by pushing the run
// branch into the canonical's default), ResetToDefault must leave the
// workspace on the default branch at the merged tip with no local run
// branch left behind.
func TestResetToDefaultParksOnDefaultAfterMerge(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	allowFFPushIntoCanonical(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// One feature commit so the merge has something to advance with.
	if err := os.WriteFile(filepath.Join(wp, "feature.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "feature.txt")
	gittest.Run(t, wp, "commit", "-m", "feature")
	mergedTip := gittest.Output(t, wp, "rev-parse", "HEAD")

	// Simulate the push-merge: ff-push the run branch into origin/main.
	// This is the same operation push.FastForwardToDefault performs from
	// the workspace's origin (the canonical submodule).
	gittest.Run(t, wp, "push", "origin", "moe/run-a:main")

	if err := ResetToDefault(wp, "main", "moe/run-a"); err != nil {
		t.Fatalf("ResetToDefault: %v", err)
	}

	// HEAD on main at the merged tip.
	head := gittest.Output(t, wp, "rev-parse", "HEAD")
	if head != mergedTip {
		t.Errorf("HEAD = %s, want merged tip %s", head, mergedTip)
	}
	branch, err := git.Output(wp, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(branch) != "main" {
		t.Errorf("current branch = %q, want main", strings.TrimSpace(branch))
	}
	// Local main matches the merged tip.
	localMain := gittest.Output(t, wp, "rev-parse", "refs/heads/main")
	if localMain != mergedTip {
		t.Errorf("local main = %s, want %s", localMain, mergedTip)
	}
	// Run branch is gone.
	if err := git.Run(wp, "rev-parse", "--verify", "--quiet", "refs/heads/moe/run-a"); err == nil {
		t.Error("local moe/run-a should be deleted after ResetToDefault")
	}
	// Feature file is present in the worktree (carried by the merge).
	if _, err := os.Stat(filepath.Join(wp, "feature.txt")); err != nil {
		t.Errorf("feature.txt missing after ResetToDefault: %v", err)
	}
}

// TestResetToDefaultIsIdempotent re-runs the verb against an already-
// parked workspace and asserts every step is a no-op chain — same
// guarantee Rebase carried.
func TestResetToDefaultIsIdempotent(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	allowFFPushIntoCanonical(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wp, "feature.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "feature.txt")
	gittest.Run(t, wp, "commit", "-m", "feature")
	gittest.Run(t, wp, "push", "origin", "moe/run-a:main")

	if err := ResetToDefault(wp, "main", "moe/run-a"); err != nil {
		t.Fatalf("ResetToDefault (first): %v", err)
	}
	headAfterFirst := gittest.Output(t, wp, "rev-parse", "HEAD")

	if err := ResetToDefault(wp, "main", "moe/run-a"); err != nil {
		t.Fatalf("ResetToDefault (second): %v", err)
	}
	headAfterSecond := gittest.Output(t, wp, "rev-parse", "HEAD")
	if headAfterFirst != headAfterSecond {
		t.Errorf("HEAD drifted on idempotent re-run: %s -> %s", headAfterFirst, headAfterSecond)
	}
}

// TestResetToDefaultRefusesDirty pins fail-loud at the post-merge call
// site: the merge has already landed, so an uncommitted edit here is a
// real bug; the caller surfaces it as a warning rather than papering
// over the inconsistent state.
func TestResetToDefaultRefusesDirty(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wp, "stray.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = ResetToDefault(wp, "main", "moe/run-a")
	if err == nil {
		t.Fatal("expected ResetToDefault to refuse a dirty workspace")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should name the uncommitted state: %v", err)
	}
}

// TestResetToDefaultMissingBranchIsSkipped covers the partial-retry
// path: the local run branch was already cleaned by a prior attempt,
// so the delete step is a silent skip rather than an error.
func TestResetToDefaultMissingBranchIsSkipped(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	// Workspace already parked on main; never opened a run branch.
	gittest.Run(t, wp, "checkout", "main")

	if err := ResetToDefault(wp, "main", "moe/never-existed"); err != nil {
		t.Errorf("ResetToDefault should tolerate a missing run branch: %v", err)
	}
}

// TestResetToDefaultMissingWorkspaceIsNoop matches the no-op shape of
// Release on a workspace that never materialised — the caller is
// best-effort and shouldn't fail if `sdlc code` never ran.
func TestResetToDefaultMissingWorkspaceIsNoop(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	if err := ResetToDefault(filepath.Join(root, "ghost"), "main", "moe/run-a"); err != nil {
		t.Errorf("ResetToDefault on missing dir should be a no-op: %v", err)
	}
}

// TestResetToDefaultRequiresDefaultBranch fails early on a missing
// default — same shape Rebase carried. The caller (releaseRunWorkspace)
// shouldn't be able to elide it by accident.
func TestResetToDefaultRequiresDefaultBranch(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	err = ResetToDefault(wp, "", "moe/run-a")
	if err == nil {
		t.Fatal("expected ResetToDefault to refuse an empty default branch")
	}
	if !strings.Contains(err.Error(), "default branch is required") {
		t.Errorf("error should name the empty default branch: %v", err)
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
