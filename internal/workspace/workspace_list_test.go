package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestListEmpty covers the no-workspaces case: List returns no rows
// and no error, both when .moe/named is missing and when it exists
// but is empty.
func TestListEmpty(t *testing.T) {
	root := t.TempDir()
	got, err := List(root, "")
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if err := os.MkdirAll(filepath.Join(root, ".moe", "named"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = List(root, "")
	if err != nil {
		t.Fatalf("List on empty named/: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

// TestListReportsClaimAndDirty seeds two workspaces — one with a
// claim and a dirty file, one clean — and confirms the Info fields
// populate as expected.
func TestListReportsClaimAndDirty(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	// dev: claimed by tele/run-a, with an untracked file.
	wpDev, err := Acquire(root, "tele", "dev", "tele/run-a")
	if err != nil {
		t.Fatalf("Acquire dev: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wpDev, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// clean: created via Ensure, no claim, no edits.
	if _, err := Ensure(root, "tele", "clean"); err != nil {
		t.Fatalf("Ensure clean: %v", err)
	}

	rows, err := List(root, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2: %+v", len(rows), rows)
	}
	// Sorted by (Project, Name): clean before dev.
	if rows[0].Name != "clean" || rows[1].Name != "dev" {
		t.Fatalf("unexpected order: %+v", rows)
	}
	if rows[0].Claim != "" || rows[0].Dirty {
		t.Fatalf("clean row should be unclaimed and clean: %+v", rows[0])
	}
	if rows[1].Claim != "tele/run-a" {
		t.Fatalf("dev claim = %q, want tele/run-a", rows[1].Claim)
	}
	if !rows[1].Dirty {
		t.Fatalf("dev should be dirty: %+v", rows[1])
	}

	// Filtering by project returns the same rows for the single project.
	filtered, err := List(root, "tele")
	if err != nil {
		t.Fatalf("List(tele): %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered rows=%d want 2", len(filtered))
	}
}

// TestListClaimedCleanIsNotDirty covers the `moe workspace list`
// regression: before the claim moved under `.moe/`, every claimed
// workspace surfaced as DIRTY because the root-level `claim.json`
// showed up in `git.Status`. A claim alone — no scratch files, no
// edits — must leave Dirty=false.
func TestListClaimedCleanIsNotDirty(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")

	if _, err := Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	rows, err := List(root, "tele")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1: %+v", len(rows), rows)
	}
	if rows[0].Claim != "tele/run-a" {
		t.Fatalf("claim = %q, want tele/run-a", rows[0].Claim)
	}
	if rows[0].Dirty {
		t.Fatalf("a claimed-but-otherwise-clean workspace should not be Dirty: %+v", rows[0])
	}
}

// TestRemoveRefusesClaimed exercises the load-bearing safety
// invariant: a claimed workspace cannot be removed.
func TestRemoveRefusesClaimed(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	if _, err := Acquire(root, "tele", "dev", "tele/run-a"); err != nil {
		t.Fatal(err)
	}
	err := Remove(root, "tele", "dev")
	if err == nil {
		t.Fatal("Remove should refuse a claimed workspace")
	}
	var ace *AlreadyClaimedError
	if !errors.As(err, &ace) {
		t.Fatalf("expected *AlreadyClaimedError, got %T: %v", err, err)
	}
	// And the directory survives.
	if !Exists(root, "tele", "dev") {
		t.Fatal("Remove should not delete a claimed workspace")
	}
}

// TestRemoveDeletesAndIsIdempotent removes an unclaimed workspace
// and confirms a second Remove call is a silent no-op.
func TestRemoveDeletesAndIsIdempotent(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	seedSrc(t, root, "tele")
	if _, err := Ensure(root, "tele", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, "tele", "dev"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Exists(root, "tele", "dev") {
		t.Fatal("workspace dir should be gone after Remove")
	}
	if err := Remove(root, "tele", "dev"); err != nil {
		t.Fatalf("Remove (idempotent): %v", err)
	}
}
