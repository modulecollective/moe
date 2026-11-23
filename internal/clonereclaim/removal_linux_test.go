//go:build linux

package clonereclaim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func disposalFixture(t *testing.T, expired bool) (*runowner.Handle, *Registry, Disposal) {
	t.Helper()
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	writeFixture(t, filepath.Join(path, "nested", "evidence"), "keep until authorized")
	writeFixture(t, filepath.Join(h.Root, "sibling"), "survives")
	if err := os.Symlink(filepath.Join(h.Root, "sibling"), filepath.Join(path, "outside-link")); err != nil {
		t.Fatal(err)
	}
	basis := DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("a", 40), Status: run.StatusClosed, At: time.Now().Add(-run.CloneRetention), Deadline: time.Now()}, Expired: expired, Preservation: "independently_preserved"}
	d, err := r.BeginDisposal(h, basis, CleanupPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AuthorizeRemoval(h, d, CleanupPlan{}); err != nil {
		t.Fatal(err)
	}
	return h, r, d
}
func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfinedRemovalAndNewGeneration(t *testing.T) {
	h, r, d := disposalFixture(t, false)
	outside := filepath.Join(h.Root, "sibling")
	// A link added before authorization is removed as a link, never traversed.
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Identity.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live tree survived: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "survives" {
		t.Fatalf("outside changed: %q %v", b, err)
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if err := CheckRunRecovery(h); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.Identity.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, r.Identity.Path)
	gittest.Commit(t, r.Identity.Path, "new source")
	next, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Identity.Generation == r.Identity.Generation {
		t.Fatal("generation reused")
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next.Identity.Path); err != nil {
		t.Fatal("old authorization touched replacement:", err)
	}
}

func TestConfinedRemovalResumeRetainsNewEarlyWork(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "early", true: "expired"}[expired], func(t *testing.T) {
			h, r, d := disposalFixture(t, expired)
			attempt, err := r.disposalDir(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer attempt.Close()
			parent, err := openAbsoluteDir(filepath.Dir(r.Identity.Path))
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			if err := renameNoReplace(parent, filepath.Base(r.Identity.Path), attempt, "tree"); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(attempt.Name(), "tree", "new-output"), "new evidence")
			// A future replacement at the original path survives every retry.
			writeFixture(t, filepath.Join(r.Identity.Path, "replacement"), "survives")
			if err := r.FinishRemoval(h, d.ID, d.Basis); expired && err != nil {
				t.Fatal(err)
			} else if !expired && err == nil {
				t.Fatal("new output deleted before expiry")
			}
			if b, err := os.ReadFile(filepath.Join(r.Identity.Path, "replacement")); err != nil || string(b) != "survives" {
				t.Fatal("replacement changed")
			}
		})
	}
}

func TestConfinedRemovalRejectsNestedReposAndReceiptFault(t *testing.T) {
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.MkdirAll(filepath.Join(path, "nested", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckTree(h, false); err == nil {
		t.Fatal("nested repo admitted")
	}
	if err := os.Remove(filepath.Join(path, "nested", ".git")); err != nil {
		t.Fatal(err)
	}
	previous := syncRecordFile
	syncRecordFile = func(f *os.File) error {
		if strings.Contains(f.Name(), ".publish-") {
			return errors.New("injected sync failure")
		}
		return previous(f)
	}
	defer func() { syncRecordFile = previous }()
	if _, err := r.BeginDisposal(h, DisposalBasis{Purpose: "rebind", Preservation: "independently_preserved"}, CleanupPlan{}); err == nil {
		t.Fatal("unsynced intent accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("tree lost before durable intent")
	}
}

func TestConfinedRemovalReconcilesMissingFinalReceipt(t *testing.T) {
	h, r, d := disposalFixture(t, true)
	attempt, err := r.disposalDir(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	// Simulate the final publication being absent after completed tree removal.
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(attempt.Name(), "removed.json")); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal(err)
	}
}
