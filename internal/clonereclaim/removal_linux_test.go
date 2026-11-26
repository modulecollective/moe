//go:build linux

package clonereclaim

import (
	"encoding/json"
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
	return disposalWithBasis(t, DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("a", 40), Status: run.StatusClosed, At: time.Now().Add(-run.CloneRetention), Deadline: time.Now()}, Expired: expired, Preservation: "independently_preserved"})
}

func disposalWithBasis(t *testing.T, basis DisposalBasis) (*runowner.Handle, *Registry, Disposal) {
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

func TestConfinedRemovalRejectsSubstitutionAtRename(t *testing.T) {
	h, r, d := disposalWithBasis(t, DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("a", 40), Status: run.StatusMerged}})
	rename := renameCloneTree
	defer func() { renameCloneTree = rename }()
	renameCloneTree = func(parent *os.File, old string, attempt *os.File, name string) error {
		if err := os.Rename(r.Identity.Path, r.Identity.Path+"-admitted"); err != nil {
			return err
		}
		writeFixture(t, filepath.Join(r.Identity.Path, "replacement"), "survives")
		return renameNoReplace(parent, old, attempt, name)
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err == nil {
		t.Fatal("substituted tree deleted")
	}
	for _, path := range []string{filepath.Join(r.Identity.Path, "replacement"), filepath.Join(r.Identity.Path+"-admitted", "nested", "evidence")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("substitution lost contents:", err)
		}
	}
}

func TestConfinedRemovalResumesParentSyncFailure(t *testing.T) {
	h, r, d := disposalFixture(t, false)
	sync := syncRecordFile
	injected := false
	syncRecordFile = func(file *os.File) error {
		if file.Name() == filepath.Dir(r.Identity.Path) && !injected {
			injected = true
			return errors.New("quarantine parent sync fault")
		}
		return sync(file)
	}
	err := r.FinishRemoval(h, d.ID, d.Basis)
	syncRecordFile = sync
	if err == nil || !injected {
		t.Fatal("parent sync fault did not stop removal")
	}
	pending, yes, err := r.PendingRemoval()
	if err != nil || !yes || pending.ID != d.ID {
		t.Fatalf("pending authorization lost: %v %v", yes, err)
	}
	if err := CheckRunRecovery(h); err == nil {
		t.Fatal("pending removal admitted recreation")
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal("private-tree retry:", err)
	}
	if err := CheckRunRecovery(h); err != nil {
		t.Fatal("completed retry did not permit recreation:", err)
	}
}

func TestConfinedRemovalRequiresExactAuthorization(t *testing.T) {
	h, r, d := disposalWithBasis(t, DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("a", 40), Status: run.StatusMerged}})
	attempt, err := r.disposalDir(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	auth, err := readRecord[removalAuthorization](attempt, "removal-authorized.json")
	if err != nil {
		t.Fatal(err)
	}
	auth.Identity.Inode++
	b, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attempt.Name(), "removal-authorized.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRemoval(h, d.ID, d.Basis); err == nil {
		t.Fatal("foreign authorization deleted tree")
	}
	if _, err := os.Stat(filepath.Join(r.Identity.Path, "nested", "evidence")); err != nil {
		t.Fatal("authorization failure lost contents")
	}
}

func TestConfinedRemovalResumesPartialInventoryWithoutTouchingReplacement(t *testing.T) {
	h, r, d := disposalFixture(t, false)
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
	if err := os.Remove(filepath.Join(attempt.Name(), "tree", "nested", "evidence")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Identity.Path, "replacement"), "survives")
	if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
		t.Fatal("partial inventory could not resume:", err)
	}
	if b, err := os.ReadFile(filepath.Join(r.Identity.Path, "replacement")); err != nil || string(b) != "survives" {
		t.Fatal("retry touched original-path replacement")
	}
	if _, err := os.Stat(filepath.Join(attempt.Name(), "tree")); !os.IsNotExist(err) {
		t.Fatal("authorized partial tree survived")
	}
}

func TestMergedRemovalPolicyAndReceipts(t *testing.T) {
	for _, kind := range []string{"merged", "old-merged", "closed", "rebind"} {
		t.Run(kind, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Second)
			basis := DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("b", 40), Status: run.StatusMerged, At: at, Deadline: at.Add(run.CloneRetention)}}
			merged := strings.Contains(kind, "merged")
			if kind == "closed" || kind == "old-merged" {
				basis.Terminal.Status = run.StatusClosed
				basis.Preservation = "independently_preserved"
			}
			if kind == "rebind" {
				basis.Purpose = "rebind"
				basis.Preservation = "independently_preserved"
			}
			h, r, d := disposalWithBasis(t, basis)
			attempt, err := r.disposalDir(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer attempt.Close()
			auth, err := readRecord[removalAuthorization](attempt, "removal-authorized.json")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "old-merged" {
				// An old binary saved fingerprints and preservation text for merged runs.
				// Retain that exact receipt shape; fresh matching authority changes policy.
				d.Basis.Terminal.Status = run.StatusMerged
				auth.Disposal = d
				for name, record := range map[string]any{"intent.json": d, "removal-authorized.json": auth} {
					b, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(attempt.Name(), name), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else if kind == "merged" {
				if d.Basis.Expired || d.Basis.Preservation != "" || d.Basis.Terminal != basis.Terminal {
					t.Fatalf("fabricated basis: %+v", d.Basis)
				}
				for _, e := range auth.Inventory {
					if e.Digest != "" {
						t.Fatal("merged authorization hashed contents")
					}
				}
			}
			if err := r.InspectRemoval(h, d.ID, d.Basis); err != nil {
				t.Fatal("initial receipt:", err)
			}
			parent, err := openAbsoluteDir(filepath.Dir(r.Identity.Path))
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			if err := renameNoReplace(parent, filepath.Base(r.Identity.Path), attempt, "tree"); err != nil {
				t.Fatal(err)
			}
			// Partial deletion, changed regular-file bytes, and new output are all
			// disposable only for merged authority (or existing expiry policy).
			if err := os.Remove(filepath.Join(attempt.Name(), "tree", "outside-link")); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(attempt.Name(), "tree", "nested", "evidence"), "changed evidence")
			writeFixture(t, filepath.Join(attempt.Name(), "tree", "new-output"), "new evidence")
			writeFixture(t, filepath.Join(r.Identity.Path, "replacement"), "survives")
			for _, operation := range []func() error{
				func() error { return r.InspectRemoval(h, d.ID, d.Basis) },
				func() error { return r.FinishRemoval(h, d.ID, d.Basis) },
			} {
				err := operation()
				if merged && err != nil {
					t.Fatal("merged resume:", err)
				}
				if !merged && (err == nil || !strings.Contains(err.Error(), "changed since")) {
					t.Fatal("preservation-required retry admitted", err)
				}
			}
			if merged {
				if _, err := os.Stat(filepath.Join(attempt.Name(), "tree")); !os.IsNotExist(err) {
					t.Fatal("partial tree survives", err)
				}
				if err := os.Remove(filepath.Join(attempt.Name(), "removed.json")); err != nil {
					t.Fatal(err)
				}
				if err := r.InspectRemoval(h, d.ID, d.Basis); err != nil {
					t.Fatal("inspect missing final receipt:", err)
				}
				if err := r.FinishRemoval(h, d.ID, d.Basis); err != nil {
					t.Fatal("recover missing final receipt:", err)
				}
			}
			if b, err := os.ReadFile(filepath.Join(r.Identity.Path, "replacement")); err != nil || string(b) != "survives" {
				t.Fatal("replacement lost", err)
			}
		})
	}
}

func TestMergedRemovalRequiresExactTerminalAuthority(t *testing.T) {
	for _, field := range []string{"commit", "status", "time", "deadline", "purpose", "expiry"} {
		t.Run(field, func(t *testing.T) {
			basis := DisposalBasis{Purpose: "terminal", Terminal: run.TerminalEvidence{Commit: strings.Repeat("c", 40), Status: run.StatusMerged}, Expired: field == "expiry"}
			h, r, d := disposalWithBasis(t, basis)
			fresh := d.Basis
			switch field {
			case "commit":
				fresh.Terminal.Commit = strings.Repeat("d", 40)
			case "status":
				fresh.Terminal.Status = run.StatusInProgress
			case "time":
				fresh.Terminal.At = time.Now()
			case "deadline":
				fresh.Terminal.Deadline = time.Now()
			case "purpose":
				fresh.Purpose = "rebind"
			case "expiry":
				fresh.Expired = false
			}
			if err := r.InspectRemoval(h, d.ID, fresh); err == nil {
				t.Fatal("changed authority inspected")
			}
			if err := r.FinishRemoval(h, d.ID, fresh); err == nil {
				t.Fatal("changed authority removed")
			}
			if _, err := os.Stat(r.Identity.Path); err != nil {
				t.Fatal("refusal lost tree", err)
			}
		})
	}
}

func TestDisposalRequiresPreservation(t *testing.T) {
	for _, purpose := range []string{"terminal", "rebind"} {
		for _, status := range []string{run.StatusMerged, run.StatusClosed, run.StatusPromoted} {
			for _, expired := range []bool{false, true} {
				basis := DisposalBasis{Purpose: purpose, Terminal: run.TerminalEvidence{Status: status, Commit: strings.Repeat("a", 40)}, Expired: expired}
				want := purpose == "rebind" || (!expired && status != run.StatusMerged)
				if got := basis.RequiresPreservation(); got != want {
					t.Fatalf("%+v: got %v want %v", basis, got, want)
				}
			}
		}
	}
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, basis := range []DisposalBasis{
		{Purpose: "terminal", Terminal: run.TerminalEvidence{Status: run.StatusMerged}},
		{Purpose: "terminal", Terminal: run.TerminalEvidence{Status: run.StatusClosed, Commit: strings.Repeat("a", 40)}},
		{Purpose: "rebind", Terminal: run.TerminalEvidence{Status: run.StatusMerged}},
		{Purpose: "rebind", Expired: true, Preservation: "independently_preserved"},
	} {
		if _, err := r.BeginDisposal(h, basis, CleanupPlan{}); err == nil {
			t.Fatalf("invalid basis admitted: %+v", basis)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
