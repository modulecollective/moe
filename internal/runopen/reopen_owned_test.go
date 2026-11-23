package runopen

import (
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func ownedClosedRun(t *testing.T) (string, *run.Metadata, *runowner.Handle) {
	t.Helper()
	root := newIdeaBureaucracy(t, "alpha", "my-idea", "# idea\n")
	setRunFields(t, root, "alpha", "my-idea", "chat", run.StatusClosed)
	gittest.Commit(t, root, "closed fixture")
	md, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	h, err := runowner.Acquire(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release() })
	return root, md, h
}

func TestReopenReloadsFreshMetadataUnderHeldOwnership(t *testing.T) {
	root, stale, h := ownedClosedRun(t)
	fresh, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Agent = "codex"
	if err := run.Save(root, fresh); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, root, "bind agent after earlier read")
	if err := Reopen(root, stale, h); err != nil {
		t.Fatal(err)
	}
	if stale.Agent != "codex" || stale.Status != run.StatusInProgress {
		t.Fatalf("fresh fields overwritten: %+v", stale)
	}
	if busy, err := runowner.Busy(root, "alpha", "my-idea"); err != nil || !busy {
		t.Fatal("reopen released caller ownership")
	}
}

func TestReopenRejectsStaleStateAndMissingOwner(t *testing.T) {
	for _, kind := range []string{"workflow", "status", "owner"} {
		t.Run(kind, func(t *testing.T) {
			root, stale, h := ownedClosedRun(t)
			fresh, err := run.Load(root, "alpha", "my-idea")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "workflow" {
				fresh.Workflow = "sdlc"
			}
			if kind == "status" {
				fresh.Status = run.StatusMerged
			}
			if kind == "owner" {
				h = nil
			}
			if err := run.Save(root, fresh); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, root, "intervening state")
			before := gittest.HeadSHA(t, root)
			if err := Reopen(root, stale, h); err == nil {
				t.Fatal("stale/unowned reopen accepted")
			}
			if gittest.HeadSHA(t, root) != before {
				t.Fatal("refused reopen committed")
			}
		})
	}
}

func TestReopenCommitFailureRestoresMetadataAndIndex(t *testing.T) {
	root, md, h := ownedClosedRun(t)
	failCommits(t, root)
	if err := Reopen(root, md, h); err == nil {
		t.Fatal("failed commit succeeded")
	}
	fresh, err := run.Load(root, "alpha", "my-idea")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != run.StatusClosed || md.Status != run.StatusClosed {
		t.Fatal("failed reopen changed status")
	}
	if dirty := gittest.Output(t, root, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		t.Fatalf("failed reopen left tracked changes: %s", dirty)
	}
}
