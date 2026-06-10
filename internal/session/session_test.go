package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// newTestRoot seeds a git repo with one commit on main so that worktree
// creation has something to branch from. Mirrors the pattern in
// internal/run/run_test.go.
func newTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	gittest.Commit(t, root, "seed")
	return root
}

func commitInWorktree(t *testing.T, worktreePath, relPath, body, msg string) {
	t.Helper()
	gittest.WriteAndCommit(t, worktreePath, relPath, body, msg)
}

func TestOpenCloseRoundtrip(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Branch != "session/moe/r1/design" {
		t.Errorf("Branch = %q, want %q", s.Branch, "session/moe/r1/design")
	}
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}

	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\nhello\n", "work: update design")

	if err := Close(s); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Worktree and branch should be gone.
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Close: err=%v", err)
	}
	if branchExists(root, s.Branch) {
		t.Errorf("branch %s still present after Close", s.Branch)
	}

	// Main should now carry the content.
	mainHead, err := git.RevParse(root, "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	out := gittest.Output(t, root, "cat-file", "-p",
		mainHead+":projects/moe/runs/r1/documents/design/content.md")
	if !strings.Contains(out, "hello") {
		t.Errorf("landed content missing expected body: %q", out)
	}

	// The working tree at the canonical root must also reflect the
	// landed content — `update-ref` updates the ref but not the index
	// or worktree, so downstream commands that read files via os.Stat
	// would see stale state. Guard against regressing to that shape.
	diskPath := filepath.Join(root, "projects/moe/runs/r1/documents/design/content.md")
	body, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read landed file from working tree: %v", err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("working tree content missing expected body: %q", body)
	}
}

// TestCloseWithSubmodule guards against regressing to plain `git worktree
// remove`, which refuses with "working trees containing submodules
// cannot be moved or removed" whenever the superproject has a submodule
// checked out (as the bureaucracy root does for projects/*/src).
func TestCloseWithSubmodule(t *testing.T) {
	root := newTestRoot(t)

	// Donor repo to serve as the submodule source.
	donor := gittest.Init(t)
	gittest.Commit(t, donor, "donor seed")

	// Add the donor as a submodule. `protocol.file.allow=always` is
	// needed since Git 2.38 (CVE-2022-39253) disabled file:// by default.
	gittest.Run(t, root, "-c", "protocol.file.allow=always",
		"submodule", "add", donor, "sub")
	gittest.Run(t, root, "commit", "-m", "add submodule")

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Init the submodule inside the new worktree. `git worktree add`
	// does not populate submodules by default, but in real bureaucracy
	// use the submodule ends up checked out in the session worktree.
	// Plain `git worktree remove` only refuses when the submodule is
	// actually present on disk, so this step is what makes the test
	// actually exercise the regression.
	gittest.Run(t, s.WorktreePath, "-c", "protocol.file.allow=always",
		"submodule", "update", "--init")

	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\n", "work: update design")

	if err := Close(s); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Close: err=%v", err)
	}
}

func TestOpenResumesExistingSession(t *testing.T) {
	root := newTestRoot(t)
	first, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	second, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if second.WorktreePath != first.WorktreePath {
		t.Errorf("resume landed on a different worktree: first=%s second=%s",
			first.WorktreePath, second.WorktreePath)
	}
	if second.Branch != first.Branch {
		t.Errorf("resume landed on a different branch: first=%s second=%s",
			first.Branch, second.Branch)
	}
	if err := Abandon(first); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
}

func TestOpenOrphanBranchErrors(t *testing.T) {
	root := newTestRoot(t)
	// Create a session branch without a worktree to simulate a busted
	// close state.
	gittest.Run(t, root, "branch", "session/moe/r1/design")
	if _, err := Open(root, "moe", "r1", "design"); err == nil {
		t.Fatal("expected error on orphan branch, got nil")
	} else if !strings.Contains(err.Error(), "without a registered worktree") {
		t.Fatalf("error does not point at the orphan state: %v", err)
	}
}

func TestCloseRebaseConflictLeavesSessionIntact(t *testing.T) {
	root := newTestRoot(t)
	// Seed main with an initial file.
	gittest.WriteAndCommit(t, root, "shared.txt", "v1\n", "seed shared")

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Canvas commit so Close gets past the empty-canvas gate and into
	// the rebase path the test actually exercises.
	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\n", "work: update design")
	// Session branch edits shared.txt.
	commitInWorktree(t, s.WorktreePath, "shared.txt", "session-edit\n", "session: touch shared")

	// Main moves ahead with a conflicting edit.
	if err := os.WriteFile(filepath.Join(root, "shared.txt"), []byte("main-edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "shared.txt")
	gittest.Run(t, root, "commit", "-m", "main: touch shared")

	err = Close(s)
	if err == nil {
		t.Fatal("expected rebase conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Errorf("error does not mention rebase: %v", err)
	}
	// Typed error so the CLI chain-back can errors.As it and launch
	// a one-shot agent inside the worktree to resolve.
	var rfe *RebaseFailureError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected *RebaseFailureError, got %T", err)
	}
	if rfe.Branch != s.Branch {
		t.Errorf("rfe.Branch = %q, want %q", rfe.Branch, s.Branch)
	}
	if rfe.WorktreePath != s.WorktreePath {
		t.Errorf("rfe.WorktreePath = %q, want %q", rfe.WorktreePath, s.WorktreePath)
	}
	if rfe.Dirty {
		t.Errorf("rfe.Dirty = true, want false (real conflict, not a refusal)")
	}
	// Conflict files are read before --abort discards the rebase
	// state, so the chain-back kickoff can name them.
	foundShared := false
	for _, p := range rfe.Conflicts {
		if p == "shared.txt" {
			foundShared = true
			break
		}
	}
	if !foundShared {
		t.Errorf("expected shared.txt in rfe.Conflicts, got %v", rfe.Conflicts)
	}
	if rfe.GitOutput == "" {
		t.Errorf("rfe.GitOutput should carry the verbatim rebase output")
	}
	// Worktree and branch remain, so the operator (or the chain-back
	// agent) can fix it.
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Errorf("worktree missing after rebase failure: %v", err)
	}
	if !branchExists(root, s.Branch) {
		t.Errorf("branch missing after rebase failure")
	}
}

// TestCloseRebaseRefusalIsDirty: when the rebase refuses to start
// because the worktree is dirty (the seed transcript's failure mode —
// --abort left residue, a follow-up Close ran rebase against the
// dirty tree, git refused), Close surfaces it as Dirty=true so the
// CLI can pick the dirty-shape kickoff prompt rather than the
// conflict-shape one.
func TestCloseRebaseRefusalIsDirty(t *testing.T) {
	root := newTestRoot(t)

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	commitInWorktree(t, s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md",
		"# Design\n", "work: update design")

	// Move main forward so the rebase actually has work to do.
	gittest.WriteAndCommit(t, root, "main-only.txt", "main\n", "main: add")

	// Drop an unstaged edit in the worktree before close — git rebase
	// refuses with "cannot rebase: You have unstaged changes" before
	// it starts touching the branch.
	dirty := filepath.Join(s.WorktreePath, "projects/moe/runs/r1/documents/design/content.md")
	if err := os.WriteFile(dirty, []byte("# Design\nunstaged edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = Close(s)
	if err == nil {
		t.Fatal("expected rebase refusal, got nil")
	}
	var rfe *RebaseFailureError
	if !errors.As(err, &rfe) {
		t.Fatalf("expected *RebaseFailureError, got %T: %v", err, err)
	}
	if !rfe.Dirty {
		t.Errorf("rfe.Dirty = false, want true for unstaged-changes refusal; output=%q", rfe.GitOutput)
	}
}

func TestAbandonRemovesWorktreeAndBranch(t *testing.T) {
	root := newTestRoot(t)
	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Abandon(s); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if _, err := os.Stat(s.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still present after Abandon")
	}
	if branchExists(root, s.Branch) {
		t.Errorf("branch still present after Abandon")
	}
}

func TestListIncludesOpenSessions(t *testing.T) {
	root := newTestRoot(t)
	a, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	b, err := Open(root, "moe", "r2", "code")
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer Abandon(a)
	defer Abandon(b)

	got, err := List(root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2\n%+v", len(got), got)
	}
	branches := map[string]bool{}
	for _, s := range got {
		branches[s.Branch] = true
	}
	if !branches["session/moe/r1/design"] || !branches["session/moe/r2/code"] {
		t.Errorf("missing expected branches: %v", branches)
	}
}

// TestSessionCloseRefusesUnchangedCanvasFromCommittedSession: a
// session that committed non-canvas paths but left the canvas blob
// equal to main's (the kickoff stub) must refuse Close with a typed
// *CanvasUnchangedError. Without this gate, the "agent had a
// conversation but never wrote the canvas" case fast-forwards
// silently and a downstream `!!` cascade dispatches the next stage
// against the unchanged stub — the exact incident this run targets.
func TestSessionCloseRefusesUnchangedCanvasFromCommittedSession(t *testing.T) {
	root := newTestRoot(t)
	// Seed the kickoff stub on main so the branch starts with a
	// non-empty canvas blob that matches main's blob — exactly the
	// shape `Open run` produces.
	canvasRel := "projects/moe/runs/r1/documents/design/content.md"
	gittest.WriteAndCommit(t, root, canvasRel, "# Design\n", "Open run moe/r1")

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Commit something other than the canvas so the branch has at
	// least one commit but the canvas blob is still the stub.
	commitInWorktree(t, s.WorktreePath, "scratch.txt", "scratch\n", "session: scratch")

	err = Close(s)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	var cue *CanvasUnchangedError
	if !errors.As(err, &cue) {
		t.Fatalf("expected *CanvasUnchangedError, got %T: %v", err, err)
	}
	if cue.CanvasPath != canvasRel {
		t.Errorf("CanvasPath = %q, want %q", cue.CanvasPath, canvasRel)
	}
	if cue.Branch != s.Branch {
		t.Errorf("Branch = %q, want %q", cue.Branch, s.Branch)
	}
	if cue.WorktreePath != s.WorktreePath {
		t.Errorf("WorktreePath = %q, want %q", cue.WorktreePath, s.WorktreePath)
	}
	if cue.Project != "moe" || cue.Run != "r1" || cue.Doc != "design" {
		t.Errorf("identity wrong: %+v", cue)
	}
	if !strings.Contains(err.Error(), "unchanged from main") {
		t.Errorf("error should describe the canvas as unchanged from main: %v", err)
	}
	if !strings.Contains(err.Error(), "moe session abandon") {
		t.Errorf("error should point at abandon: %v", err)
	}
	// Worktree and branch must remain so the operator can recover.
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Errorf("worktree missing after refusal: %v", err)
	}
	if !branchExists(root, s.Branch) {
		t.Errorf("branch missing after refusal")
	}
}

// TestSessionCloseRefusesZeroCommitSession: a session that hit a
// bootstrap failure (or any pre-first-turn bail) has no commits
// past main, so the branch tip canvas trivially matches main's. The
// silent-Abandon path used to swallow this case — the chain prompt
// would then fire and cascade. With the new gate it surfaces as a
// typed *CanvasUnchangedError so the operator sees what happened
// and can either re-open or `moe session abandon` explicitly.
func TestSessionCloseRefusesZeroCommitSession(t *testing.T) {
	root := newTestRoot(t)
	canvasRel := "projects/moe/runs/r1/documents/design/content.md"
	gittest.WriteAndCommit(t, root, canvasRel, "# Design\n", "Open run moe/r1")

	s, err := Open(root, "moe", "r1", "design")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = Close(s)
	if err == nil {
		t.Fatal("expected zero-commit close to refuse loud, got nil")
	}
	var cue *CanvasUnchangedError
	if !errors.As(err, &cue) {
		t.Fatalf("expected *CanvasUnchangedError, got %T: %v", err, err)
	}
	// Branch and worktree must remain so the operator can recover.
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Errorf("worktree missing after refusal: %v", err)
	}
	if !branchExists(root, s.Branch) {
		t.Errorf("branch missing after refusal")
	}
}

func TestFindByBranchOnOrphanReturnsSynthetic(t *testing.T) {
	root := newTestRoot(t)
	gittest.Run(t, root, "branch", "session/moe/r1/design")
	s, err := FindByBranch(root, "session/moe/r1/design")
	if err != nil {
		t.Fatalf("FindByBranch: %v", err)
	}
	if s == nil {
		t.Fatal("FindByBranch returned nil for an existing branch")
	}
	if s.WorktreePath != "" {
		t.Errorf("synthetic session should have empty WorktreePath, got %q", s.WorktreePath)
	}
	if err := Abandon(s); err != nil {
		t.Fatalf("Abandon synthetic: %v", err)
	}
	if branchExists(root, "session/moe/r1/design") {
		t.Error("branch still present after Abandon")
	}
}
