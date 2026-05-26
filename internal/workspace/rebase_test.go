package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// advanceCanonicalMain commits another file on the canonical's main so
// the workspace's view of origin/main lags behind. Returns the new
// canonical SHA.
func advanceCanonicalMain(t *testing.T, root, projectID, msg string) string {
	t.Helper()
	src := filepath.Join(root, "projects", projectID, "src")
	return gittest.WriteAndCommit(t, src, "advance-"+msg+".txt", "advance\n", "advance "+msg)
}

// TestRebaseFastForwardsLocalMainWhenHEADIsFeature is the load-bearing
// case: a feature branch in the workspace, canonical advanced — the
// rebase should FF the workspace's local main and rebase the feature
// branch onto it.
func TestRebaseFastForwardsLocalMainWhenHEADIsFeature(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// One commit on the feature branch so we can verify the rebase
	// actually moved it.
	if err := os.WriteFile(filepath.Join(wp, "feature.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "feature.txt")
	gittest.Run(t, wp, "commit", "-m", "feature")
	featureBefore := gittest.Output(t, wp, "rev-parse", "HEAD")

	advanced := advanceCanonicalMain(t, root, "tele", "1")

	res, err := Rebase(wp, "main")
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if res.MainAfter != advanced {
		t.Errorf("MainAfter = %s, want canonical advanced SHA %s", res.MainAfter, advanced)
	}
	if res.MainBefore == res.MainAfter {
		t.Errorf("expected main to move; before=%s after=%s", res.MainBefore, res.MainAfter)
	}
	if res.BranchBefore != featureBefore {
		t.Errorf("BranchBefore = %s, want pre-rebase HEAD %s", res.BranchBefore, featureBefore)
	}
	if res.BranchAfter == featureBefore {
		t.Errorf("expected feature branch to move; stayed at %s", featureBefore)
	}
	// Local main should now be at the canonical's advanced tip.
	localMain := gittest.Output(t, wp, "rev-parse", "refs/heads/main")
	if localMain != advanced {
		t.Errorf("workspace local main = %s, want %s", localMain, advanced)
	}
	// Feature branch should sit on top of the new main (one commit ahead).
	count := gittest.Output(t, wp, "rev-list", "--count", "main..HEAD")
	if count != "1" {
		t.Errorf("feature branch ahead-of-main = %s, want 1", count)
	}
}

// TestRebaseWhenHEADIsDefaultBranchFastForwardsOnly: HEAD on main, no
// feature branch — the verb should just FF local main and return.
func TestRebaseWhenHEADIsDefaultBranchFastForwardsOnly(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	// EnsureAt's `checkout HEAD` leaves the clone on main (detached or
	// not depending on git version); explicitly check out the local
	// main branch so HEAD is the named ref the verb expects.
	gittest.Run(t, wp, "checkout", "main")
	mainBefore := gittest.Output(t, wp, "rev-parse", "HEAD")

	advanced := advanceCanonicalMain(t, root, "tele", "1")
	if advanced == mainBefore {
		t.Fatal("setup: advanceCanonicalMain didn't move main")
	}

	res, err := Rebase(wp, "main")
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want main", res.Branch)
	}
	if res.MainAfter != advanced {
		t.Errorf("MainAfter = %s, want advanced %s", res.MainAfter, advanced)
	}
	// HEAD should be at the advanced tip.
	head := gittest.Output(t, wp, "rev-parse", "HEAD")
	if head != advanced {
		t.Errorf("HEAD after rebase = %s, want %s", head, advanced)
	}
	// Worktree should reflect the advance — the new file is checked out.
	if _, err := os.Stat(filepath.Join(wp, "advance-1.txt")); err != nil {
		t.Errorf("advance file missing from worktree after FF: %v", err)
	}
}

// TestRebaseIsIdempotentWhenAlreadyUpToDate: a second run immediately
// after a successful one should be a no-op (everything already at
// origin/main).
func TestRebaseIsIdempotentWhenAlreadyUpToDate(t *testing.T) {
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
	advanceCanonicalMain(t, root, "tele", "1")

	if _, err := Rebase(wp, "main"); err != nil {
		t.Fatalf("Rebase (first): %v", err)
	}
	branchAfterFirst := gittest.Output(t, wp, "rev-parse", "HEAD")

	res, err := Rebase(wp, "main")
	if err != nil {
		t.Fatalf("Rebase (second): %v", err)
	}
	if res.MainBefore != res.MainAfter {
		t.Errorf("idempotent rebase moved main: before=%s after=%s", res.MainBefore, res.MainAfter)
	}
	if res.BranchBefore != res.BranchAfter {
		t.Errorf("idempotent rebase moved branch: before=%s after=%s", res.BranchBefore, res.BranchAfter)
	}
	branchAfterSecond := gittest.Output(t, wp, "rev-parse", "HEAD")
	if branchAfterFirst != branchAfterSecond {
		t.Errorf("HEAD shifted on idempotent re-run: %s -> %s", branchAfterFirst, branchAfterSecond)
	}
}

// TestRebaseRefusesDirty: uncommitted edits in the workspace are not
// silently squashed by the rebase.
func TestRebaseRefusesDirty(t *testing.T) {
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
	advanceCanonicalMain(t, root, "tele", "1")

	_, err = Rebase(wp, "main")
	if err == nil {
		t.Fatal("expected Rebase to refuse a dirty workspace")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should name the uncommitted state: %v", err)
	}
	var rfe *RebaseFailureError
	if errors.As(err, &rfe) {
		t.Errorf("dirty refusal must not surface as *RebaseFailureError: %v", err)
	}
}

// TestRebaseRefusesDetachedHEAD: the verb is "rebase a workspace
// branch"; detached HEAD has no named branch to advance.
func TestRebaseRefusesDetachedHEAD(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "checkout", "--detach", "HEAD")

	_, err = Rebase(wp, "main")
	if err == nil {
		t.Fatal("expected Rebase to refuse a detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("error should name the detached state: %v", err)
	}
}

// TestRebaseRefusesDivergentLocalMain: a local main that diverges
// from origin (not a simple ancestor) is a real problem, not something
// the verb papers over.
func TestRebaseRefusesDivergentLocalMain(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	// Commit on local main, then advance canonical main on a different
	// commit so the two diverge.
	gittest.Run(t, wp, "checkout", "main")
	if err := os.WriteFile(filepath.Join(wp, "local-only.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "local-only.txt")
	gittest.Run(t, wp, "commit", "-m", "local only")
	advanceCanonicalMain(t, root, "tele", "remote")

	_, err = Rebase(wp, "main")
	if err == nil {
		t.Fatal("expected Rebase to refuse divergent local main")
	}
	if !strings.Contains(err.Error(), "diverged") {
		t.Errorf("error should name the divergence: %v", err)
	}
}

// TestRebaseReturnsTypedErrorOnConflict: when `git rebase main` hits
// real conflicts (same file edited on both sides), Rebase must abort
// and return *RebaseFailureError carrying the branch, conflict paths,
// and git output the CLI's chain-back agent needs.
func TestRebaseReturnsTypedErrorOnConflict(t *testing.T) {
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
	// Branch edits README.md.
	if err := os.WriteFile(filepath.Join(wp, "README.md"), []byte("seed\nfrom-branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, wp, "add", "README.md")
	gittest.Run(t, wp, "commit", "-m", "branch edit")

	// Canonical edits the same file differently.
	src := filepath.Join(root, "projects", "tele", "src")
	gittest.WriteAndCommit(t, src, "README.md", "seed\nfrom-canonical\n", "canonical edit")

	_, err = Rebase(wp, "main")
	if err == nil {
		t.Fatal("expected Rebase to fail on a real conflict")
	}
	var rfe *RebaseFailureError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected *RebaseFailureError, got %T: %v", err, err)
	}
	if rfe.Branch != "moe/run-a" {
		t.Errorf("RebaseFailureError.Branch = %q, want moe/run-a", rfe.Branch)
	}
	if len(rfe.Conflicts) == 0 || rfe.Conflicts[0] != "README.md" {
		t.Errorf("Conflicts = %v, want [README.md]", rfe.Conflicts)
	}
	if !strings.Contains(rfe.GitOutput, "README.md") {
		t.Errorf("GitOutput should mention README.md: %s", rfe.GitOutput)
	}
	// The rebase was aborted, so the branch tip is at its pre-rebase SHA
	// and the working tree is clean.
	if entries, err := git.Status(wp); err != nil {
		t.Fatalf("git.Status after refused rebase: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("worktree should be clean after --abort, got %+v", entries)
	}
}

// TestRebaseRefusesMissingWorkspacePath: the CLI checks Exists first,
// but the package-level guard is the right place to surface a clear
// error if a caller forgets.
func TestRebaseRefusesMissingWorkspacePath(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	missing := filepath.Join(root, ".moe", "named", "tele", "ghost")

	_, err := Rebase(missing, "main")
	if err == nil {
		t.Fatal("expected Rebase to refuse a missing workspace path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should name the missing dir: %v", err)
	}
}
