package push

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/sandbox"
)

// TestFilterSandboxBindMountsDropsCharDevices pins the bind-mount
// carve-out: a status entry whose on-disk file is a character device
// (the runtime's /dev/null stand-in for shadowed host config files)
// shouldn't gate the push. Real regular files stay in.
//
// Uses a symlink to /dev/null instead of mknod so the test doesn't
// need CAP_MKNOD — os.Stat follows the symlink and reports the device
// mode bits from the target, which is the same shape the filter sees
// in production.
func TestFilterSandboxBindMountsDropsCharDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-to-/dev/null shape not available on windows")
	}
	clone := t.TempDir()

	// Real file — an actual uncommitted agent edit.
	if err := os.WriteFile(filepath.Join(clone, "real.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bind-mount stand-in via a symlink to /dev/null. os.Stat follows
	// the symlink and reports `ModeDevice|ModeCharDevice`, matching
	// what the production runtime's bind-mounted /dev/null entries
	// look like.
	if err := os.Symlink("/dev/null", filepath.Join(clone, ".bashrc")); err != nil {
		t.Skipf("symlink /dev/null: %v", err)
	}

	entries := []git.StatusEntry{
		{XY: "??", Path: "real.go"},
		{XY: "??", Path: ".bashrc"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 1 || got[0].Path != "real.go" {
		t.Fatalf("filterSandboxBindMounts kept %v; want only real.go", got)
	}
}

// TestFilterSandboxBindMountsKeepsMissingEntries: a status entry whose
// file is missing (stat error) stays in the slice. Better to refuse
// the push on an ambiguous record than silently let a deleted-but-
// uncommitted edit through.
func TestFilterSandboxBindMountsKeepsMissingEntries(t *testing.T) {
	clone := t.TempDir()
	entries := []git.StatusEntry{
		{XY: " D", Path: "gone.go"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 1 || got[0].Path != "gone.go" {
		t.Fatalf("filterSandboxBindMounts dropped a missing entry: %v", got)
	}
}

// TestCheckCleanWorkTreeIgnoresMoeDir locks in the loop the original
// incident exposed: harness-private artifacts dropped into
// `<clone>/.moe/` (the dev-env cache being the first one) must not
// gate the push pre-flight. EnsureAt adds `.moe/` to the clone's
// `.git/info/exclude`, so `git status` doesn't even report the file,
// and CheckCleanWorkTree sees a clean tree.
func TestCheckCleanWorkTreeIgnoresMoeDir(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "projects", "thing", "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, src, "init", "-b", "main")
	gittest.Run(t, src, "commit", "--allow-empty", "-m", "init")

	clone, err := sandbox.Ensure(root, "thing", "req-a")
	if err != nil {
		t.Fatalf("sandbox.Ensure: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(clone, ".moe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".moe", "dev-env.env"), []byte("FOO=bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CheckCleanWorkTree(clone, "thing"); err != nil {
		t.Fatalf("CheckCleanWorkTree: %v", err)
	}
}

// TestRebaseInProgress pins the mid-rebase probe: a `.git/rebase-merge`
// or `.git/rebase-apply` directory means a rebase stopped and was never
// finished (the codex-rebase-weirdness wedge). A clean clone, or a
// like-named regular file, must read as not-mid-rebase so the probe
// doesn't false-positive a normal push.
func TestRebaseInProgress(t *testing.T) {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		t.Run(state, func(t *testing.T) {
			clone := t.TempDir()
			gitDir := filepath.Join(clone, ".git")
			if err := os.MkdirAll(filepath.Join(gitDir, state), 0o755); err != nil {
				t.Fatal(err)
			}
			if !RebaseInProgress(clone) {
				t.Fatalf("RebaseInProgress = false with %s present", state)
			}
		})
	}

	t.Run("clean", func(t *testing.T) {
		clone := t.TempDir()
		if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if RebaseInProgress(clone) {
			t.Fatal("RebaseInProgress = true on a clean clone")
		}
	})

	t.Run("regular-file-not-dir", func(t *testing.T) {
		clone := t.TempDir()
		gitDir := filepath.Join(clone, ".git")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A regular file named rebase-merge is not a stopped rebase.
		if err := os.WriteFile(filepath.Join(gitDir, "rebase-merge"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if RebaseInProgress(clone) {
			t.Fatal("RebaseInProgress = true for a regular file named rebase-merge")
		}
	})
}

// TestDefaultAdvanced pins the ff-retry probe against a fixture pair of
// repos: a bare origin and a clone carrying a run branch one commit
// ahead of main. When origin/main moves past the branch tip (a
// concurrent merge landing during the checks window) the probe reports
// true — retryable; when origin/main is still an ancestor of the branch
// (at or behind its tip) it reports false — the ff-push rejection had
// another cause and a retry can't fix it.
func TestDefaultAdvanced(t *testing.T) {
	gittest.SetupEnv(t)

	origin := gittest.InitBare(t)
	seed := t.TempDir()
	gittest.InitAt(t, seed)
	gittest.WriteAndCommit(t, seed, "README.md", "seed\n", "seed")
	gittest.Run(t, seed, "remote", "add", "origin", origin)
	gittest.Run(t, seed, "push", "origin", "main")

	// Clone and put the run branch one commit ahead of main.
	clone := t.TempDir()
	gittest.Run(t, "", "clone", "-b", "main", origin, clone)
	branch := "moe/run"
	gittest.Run(t, clone, "checkout", "-b", branch)
	gittest.WriteAndCommit(t, clone, "feature.txt", "hello\n", "add feature")

	// origin/main is still the seed (an ancestor of the branch) → not
	// advanced.
	if advanced, err := DefaultAdvanced(clone, branch, "main"); err != nil || advanced {
		t.Fatalf("DefaultAdvanced with origin at seed: got (%v, %v), want (false, nil)", advanced, err)
	}

	// A concurrent worker advances origin/main past the branch's base.
	work := t.TempDir()
	gittest.Run(t, "", "clone", "-b", "main", origin, work)
	gittest.WriteAndCommit(t, work, "other.txt", "other\n", "divergent")
	gittest.Run(t, work, "push", "origin", "main")

	// Now origin/main carries a commit the branch doesn't → advanced.
	if advanced, err := DefaultAdvanced(clone, branch, "main"); err != nil || !advanced {
		t.Fatalf("DefaultAdvanced after origin advanced: got (%v, %v), want (true, nil)", advanced, err)
	}
}

// TestTrailerValueAnchorsAndScopesRun is the branch-deleting site's
// regression: TrailerValue must return only the value scoped to the
// exact (project, run) asked for, never a prefix-extending sibling's
// (`auth` vs `auth-2`), a same-slug run in another project's, or one a
// commit merely quotes in its body. reconcileOnePushedRun reads MoE-PR
// and MoE-Merged through here; a wrong read flips a still-open run to
// merged and deletes its remote branch.
func TestTrailerValueAnchorsAndScopesRun(t *testing.T) {
	gittest.SetupEnv(t)
	root := gittest.Init(t)
	commit := func(subject, body string) {
		gittest.Run(t, root, "commit", "--allow-empty", "-m", subject+"\n\n"+body)
	}
	// Prefix-extending sibling in the same project: auth vs auth-2.
	commit("push: auth", "MoE-Run: auth\nMoE-Project: alpha\nMoE-PR: https://example.com/pr/auth")
	commit("push: auth-2", "MoE-Run: auth-2\nMoE-Project: alpha\nMoE-PR: https://example.com/pr/auth-2")
	// Same slug in a different project.
	commit("push: auth (beta)", "MoE-Run: auth\nMoE-Project: beta\nMoE-PR: https://example.com/pr/beta-auth")
	// A commit for another run that merely quotes auth's trailer at line
	// start further down its body — the grep matches, the exact re-check
	// rejects it (first MoE-Run is `noise`, not `auth`).
	commit("push: noise quotes auth", "MoE-Run: noise\nMoE-Project: alpha\nMoE-PR: https://example.com/pr/noise\nMoE-Run: auth")

	cases := []struct {
		project, run, want string
	}{
		{"alpha", "auth", "https://example.com/pr/auth"},
		{"alpha", "auth-2", "https://example.com/pr/auth-2"},
		{"beta", "auth", "https://example.com/pr/beta-auth"},
		{"alpha", "ghost", ""}, // no such run
	}
	for _, tc := range cases {
		if got := TrailerValue(root, tc.project, tc.run, "MoE-PR"); got != tc.want {
			t.Errorf("TrailerValue(%s, %s) = %q, want %q", tc.project, tc.run, got, tc.want)
		}
	}
}

// TestFilterSandboxBindMountsKeepsRegularFiles: the steady-state case.
// Every entry is a normal file; the filter returns the slice unchanged
// so a real uncommitted edit still gates the push.
func TestFilterSandboxBindMountsKeepsRegularFiles(t *testing.T) {
	clone := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(clone, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries := []git.StatusEntry{
		{XY: " M", Path: "a.go"},
		{XY: "??", Path: "b.go"},
	}
	got := filterSandboxBindMounts(clone, entries)
	if len(got) != 2 {
		t.Fatalf("filterSandboxBindMounts dropped a regular file: %v", got)
	}
}

// TestCheckBranchHasCommits exercises the guard's four outcomes. The
// load-bearing one is unresolvable-base: AheadOf returns an error, the
// guard steps aside (nil), and the push proceeds — the fix for the
// wrong "no commits ahead" refusal in detached-submodule sandboxes.
func TestCheckBranchHasCommits(t *testing.T) {
	t.Run("missing branch is an error", func(t *testing.T) {
		dir := gittest.Init(t)
		gittest.Commit(t, dir, "base")

		if err := CheckBranchHasCommits(dir, "feat", "main", "sdlc"); err == nil {
			t.Fatal("got nil, want error for a branch that doesn't exist")
		}
	})

	t.Run("zero commits ahead of a resolvable base is an error", func(t *testing.T) {
		dir := gittest.Init(t)
		gittest.Commit(t, dir, "base")
		gittest.Run(t, dir, "checkout", "-b", "feat")
		// feat points at the same commit as main: nothing ahead.

		if err := CheckBranchHasCommits(dir, "feat", "main", "sdlc"); err == nil {
			t.Fatal("got nil, want error for zero commits ahead")
		}
	})

	t.Run("commits ahead of a resolvable base is allowed", func(t *testing.T) {
		dir := gittest.Init(t)
		gittest.Commit(t, dir, "base")
		gittest.Run(t, dir, "checkout", "-b", "feat")
		gittest.Commit(t, dir, "work")

		if err := CheckBranchHasCommits(dir, "feat", "main", "sdlc"); err != nil {
			t.Fatalf("got %v, want nil for a branch with commits ahead", err)
		}
	})

	t.Run("unresolvable base skips the guard", func(t *testing.T) {
		dir := gittest.Init(t)
		gittest.Commit(t, dir, "base")
		gittest.Run(t, dir, "checkout", "-b", "feat")
		gittest.Commit(t, dir, "work")

		// The branch exists and has real commits, but the base ref
		// doesn't resolve (the detached-submodule sandbox has no local
		// `main`). AheadOf errors, so the guard must step aside.
		if err := CheckBranchHasCommits(dir, "feat", "does-not-exist", "sdlc"); err != nil {
			t.Fatalf("got %v, want nil skip when base is unresolvable", err)
		}
	})
}
