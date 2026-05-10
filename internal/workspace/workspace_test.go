package workspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGit skips when git isn't available and isolates git config so
// fixture commits don't pick up the host's identity / hooks. Mirrors
// the helper in sandbox_test.go.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[user]\n\temail = t@example.com\n\tname = T\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s %v: %v", dir, args, err)
	}
	return strings.TrimSpace(string(out))
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
}

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
	runGit(t, src, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", "README.md")
	runGit(t, src, "commit", "-m", "seed")
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
	requireGit(t)
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
	requireGit(t)
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
	requireGit(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	// Create the run-a branch and commit on it, then leave a stray edit.
	runGit(t, wp, "checkout", "-b", "moe/run-a")
	if err := os.WriteFile(filepath.Join(wp, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wp, "add", "code.txt")
	runGit(t, wp, "commit", "-m", "v1")
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
	requireGit(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	wp, err := Ensure(root, "tele", "dev")
	if err != nil {
		t.Fatal(err)
	}
	mainSHA := runGitOut(t, wp, "rev-parse", "HEAD")

	// Run A: branches off main, lands a commit, leaves a clean tree.
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach run-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wp, "code.txt"), []byte("from-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wp, "add", "code.txt")
	runGit(t, wp, "commit", "-m", "a-only")
	aTipSHA := runGitOut(t, wp, "rev-parse", "HEAD")
	if aTipSHA == mainSHA {
		t.Fatal("setup: run-a tip should differ from main")
	}

	// Run B: attaches; must anchor moe/run-b to main, NOT to a-only.
	if err := Attach(wp, "moe/run-b", "main"); err != nil {
		t.Fatalf("Attach run-b: %v", err)
	}
	bTipSHA := runGitOut(t, wp, "rev-parse", "HEAD")
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
	requireGit(t)
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
	runGit(t, wp, "add", "x")
	runGit(t, wp, "commit", "-m", "a")
	want := runGitOut(t, wp, "rev-parse", "HEAD")
	// Switch off the run-a branch, then re-attach on turn 2. We
	// detach rather than `checkout main` because main is checked out
	// in the canonical submodule and worktrees can't share a branch.
	runGit(t, wp, "checkout", "--detach", "main")
	if err := Attach(wp, "moe/run-a", "main"); err != nil {
		t.Fatalf("Attach run-a (turn 2): %v", err)
	}
	got := runGitOut(t, wp, "rev-parse", "HEAD")
	if got != want {
		t.Fatalf("Attach reuse drifted: want %s got %s", want, got)
	}
}
