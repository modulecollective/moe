//go:build linux

package clonereclaim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/runowner"
)

func registryFixture(t *testing.T) (*runowner.Handle, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, ".moe", "clones", "p", "r")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitAt(t, path)
	gittest.Commit(t, path, "seed")
	h, err := runowner.Acquire(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(); err != nil {
			t.Error(err)
		}
	})
	return h, path
}

func TestGenerationEnrollmentAndReplacement(t *testing.T) {
	h, path := registryFixture(t)
	if r, err := LoadRegistry(h); err == nil {
		r.Close()
		t.Fatal("legacy auto-enrolled")
	}
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Identity.Path != path || r.Identity.Namespace == "" || r.Identity.Mount == "" || !validRandomID(r.Identity.Generation) {
		t.Fatalf("incomplete identity: %+v", r.Identity)
	}
	loaded, err := LoadRegistry(h)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Identity != r.Identity {
		t.Fatal("enrollment not repeatably readable")
	}
	loaded.Close()
	if again, err := Enroll(h); err == nil {
		again.Close()
		t.Fatal("enrollment overwritten")
	}
	if err := r.validateOwner(h); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(path, ".moe", identityMarker), filepath.Join(registryPath(h.Root, "p", "r"), r.Identity.Generation, "identity.json")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("record permissions %s: %v", p, info.Mode())
		}
	}
	// A replacement even carrying copied identity bytes is a different tree.
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".moe"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(path+"-old", ".moe", identityMarker))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".moe", identityMarker), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadRegistry(h); err == nil {
		got.Close()
		t.Fatal("replacement accepted")
	}
}

func TestGenerationRefusesUnknownEvidence(t *testing.T) {
	for _, kind := range []string{"foreign-namespace", "marker-mismatch", "external-mismatch", "unknown-field", "trailing", "permissions", "released-owner"} {
		t.Run(kind, func(t *testing.T) {
			h, path := registryFixture(t)
			r, err := Enroll(h)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			marker := filepath.Join(path, ".moe", identityMarker)
			external := filepath.Join(registryPath(h.Root, "p", "r"), r.Identity.Generation, "identity.json")
			b, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "foreign-namespace":
				b = []byte(strings.ReplaceAll(string(b), r.Identity.Namespace, "mnt:[foreign]"))
			case "marker-mismatch", "external-mismatch":
				b = []byte(strings.ReplaceAll(string(b), `"version":1`, `"version":2`))
			case "unknown-field":
				b = []byte(strings.Replace(string(b), "{", `{"unknown":true,`, 1))
			case "trailing":
				b = append(b, []byte("{}")...)
			case "permissions":
				if err := os.Chmod(marker, 0o644); err != nil {
					t.Fatal(err)
				}
			case "released-owner":
				h.Release()
			}
			if kind == "external-mismatch" {
				marker = external
			}
			if err := os.WriteFile(marker, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := LoadRegistry(h); err == nil {
				got.Close()
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestMissingMarkerDoesNotReplacePartialEnrollment(t *testing.T) {
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	original := r.Identity.Generation
	r.Close()
	if err := os.Remove(filepath.Join(path, ".moe", identityMarker)); err != nil {
		t.Fatal(err)
	}
	if r, err := Enroll(h); err == nil {
		r.Close()
		t.Fatal("missing marker silently replaced")
	}
	if _, err := os.Stat(filepath.Join(registryPath(h.Root, "p", "r"), original, "identity.json")); err != nil {
		t.Fatal("original enrollment evidence lost:", err)
	}
}

func TestEnrollmentRejectsSymlinkedPathsAndGitfiles(t *testing.T) {
	for _, kind := range []string{"root", "project-parent", "clone", "gitdir", "gitfile", "local-marker-parent", "registry-parent"} {
		t.Run(kind, func(t *testing.T) {
			h, path := registryFixture(t)
			target := ""
			switch kind {
			case "root":
				target = h.Root
			case "project-parent":
				target = filepath.Dir(path)
			case "clone":
				target = path
			case "gitdir", "gitfile":
				target = filepath.Join(path, ".git")
			case "local-marker-parent":
				target = filepath.Join(path, ".moe")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "registry-parent":
				target = filepath.Join(h.Root, ".moe", "clone-reclamation")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(target, target+"-original"); err != nil {
				t.Fatal(err)
			}
			if kind == "gitfile" {
				if err := os.WriteFile(target, []byte("gitdir: "+target+"-original\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(target+"-original", target); err != nil {
				t.Fatal(err)
			}
			// Restore the root before TempDir cleanup: never leave its real
			// fixture data outside the managed cleanup pathname.
			t.Cleanup(func() { os.Remove(target); os.Rename(target+"-original", target) })
			if r, err := Enroll(h); err == nil {
				r.Close()
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestDurablePublicationCreateOnceAndSyncFailures(t *testing.T) {
	for _, stage := range []string{"file", "parent"} {
		t.Run(stage, func(t *testing.T) {
			parent, err := openAbsoluteDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			original := syncRecordFile
			t.Cleanup(func() { syncRecordFile = original })
			injected := errors.New("injected sync failure")
			syncRecordFile = func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() == (stage == "parent") {
					return injected
				}
				return f.Sync()
			}
			if err := publishRecord(parent, "intent.json", Identity{Version: 1}); !errors.Is(err, injected) {
				t.Fatalf("sync failure hidden: %v", err)
			}
			if stage == "file" {
				if _, err := os.Stat(filepath.Join(parent.Name(), "intent.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("intent published before file sync: %v", err)
				}
			}
		})
	}
	parent, err := openAbsoluteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := publishRecord(parent, "intent.json", Identity{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := publishRecord(parent, "intent.json", Identity{Version: 2}); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("overwrote immutable intent: %v", err)
	}
	got, err := readRecord[Identity](parent, "intent.json")
	if err != nil || got.Version != 1 {
		t.Fatalf("original evidence changed: %+v %v", got, err)
	}
	entries, err := parent.ReadDir(-1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("publication temp leaked: %v %v", entries, err)
	}
}
