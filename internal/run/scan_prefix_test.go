package run

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestScanPrefixPrunesReads(t *testing.T) {
	root := t.TempDir()
	for _, project := range []string{"alpha", "alphabet", "beta"} {
		writeRun(t, root, project, "match", StatusMerged)
		writeRun(t, root, project, "other", StatusClosed)
	}
	// Thousands of unrelated entries must not add metadata reads.
	for i := range 2000 {
		writeRun(t, root, "bulk", fmt.Sprintf("run-%04d", i), StatusClosed)
	}
	reads := countReads(t)
	for _, tt := range []struct {
		prefix string
		want   []string
	}{
		{"alpha/mat", []string{"alpha/match:merged"}},
		{"alpha/", []string{"alpha/match:merged", "alpha/other:closed"}},
		{"alph", []string{"alpha/match:merged", "alpha/other:closed", "alphabet/match:merged", "alphabet/other:closed"}},
		{"beta/match", []string{"beta/match:merged"}},
		{"absent/", nil},
		{"alpha/absent", nil},
		{"../", nil},
		{"alpha/../beta", nil},
		{"alpha/match/", nil},
		{"alpha/*", nil},
		{"[ab]", nil},
	} {
		t.Run(tt.prefix, func(t *testing.T) {
			reads.Store(0)
			mds, err := ScanPrefix(root, tt.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if got := scanKeys(t, mds); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if n := reads.Load(); n != int64(len(tt.want)) {
				t.Fatalf("read %d metadata files, want %d", n, len(tt.want))
			}
		})
	}
}

func TestScanPrefixEmptyAndStrayEntries(t *testing.T) {
	root := t.TempDir()
	if mds, err := ScanPrefix(root, ""); err != nil || len(mds) != 0 {
		t.Fatalf("missing projects: %v, %v", mds, err)
	}
	for _, d := range []string{"projects/empty/runs", "projects/noruns/docs", "projects/alpha/runs/no-json/documents"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"projects/README.md", "projects/alpha/runs/notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if mds, err := ScanPrefix(root, ""); err != nil || len(mds) != 0 {
		t.Fatalf("empty/stray entries: %v, %v", mds, err)
	}
	writeRun(t, root, "beta", "same", StatusMerged)
	writeRun(t, root, "alpha", "same", StatusClosed)
	got, err := ScanPrefix(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := mustScan(t, root); !slices.Equal(scanKeys(t, got), scanKeys(t, want)) {
		t.Fatalf("empty prefix = %v, Scan = %v", scanKeys(t, got), scanKeys(t, want))
	}
	// Metacharacters and dots are literal even when they can match names.
	writeRun(t, root, "alpha", "[literal]", StatusClosed)
	writeRun(t, root, "alpha", "..literal", StatusClosed)
	for _, prefix := range []string{"alpha/[", "alpha/.."} {
		mds, err := ScanPrefix(root, prefix)
		if err != nil || len(mds) != 1 {
			t.Fatalf("literal prefix %q: %v, %v", prefix, mds, err)
		}
	}
}

func TestScanPrefixErrorsAndRecovery(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "ok", StatusMerged)
	writeRun(t, root, "p", "bad", StatusClosed)
	bad := filepath.Join(root, Dir("p", "bad"), "run.json")
	for _, body := range []string{"{not json", `{"id":"bad","project":"p"}`} {
		if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := ScanPrefix(root, "p/bad"); err == nil {
				t.Fatal("selected invalid metadata must fail on every call")
			}
			if mds, err := ScanPrefix(root, "p/ok"); err != nil || len(mds) != 1 {
				t.Fatalf("unrelated invalid metadata: %v, %v", mds, err)
			}
		}
	}
	writeRun(t, root, "p", "bad", StatusClosed)
	if mds, err := ScanPrefix(root, "p/bad"); err != nil || len(mds) != 1 {
		t.Fatalf("repaired metadata: %v, %v", mds, err)
	}
	orig := readRunFile
	readRunFile = func(path string) ([]byte, error) {
		if path == bad {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		}
		return orig(path)
	}
	t.Cleanup(func() { readRunFile = orig })
	if _, err := ScanPrefix(root, "p/bad"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable selected metadata: %v", err)
	}
	if _, err := ScanPrefix(root, "p/ok"); err != nil {
		t.Fatalf("unreadable unrelated metadata: %v", err)
	}
	// Directory errors are propagated too; a file at projects/ is invalid.
	invalidRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidRoot, "projects"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanPrefix(invalidRoot, ""); err == nil {
		t.Fatal("invalid projects directory must fail")
	}
}

func TestScanPrefixFreshMetadata(t *testing.T) {
	root := t.TempDir()
	writeRun(t, root, "p", "r", StatusMerged)
	md, err := Load(root, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		// Both pairs have equal byte lengths; no sleeps or stat changes needed.
		for _, pair := range [][2]string{{"sdlc", StatusClosed}, {"idea", StatusMerged}} {
			md.Workflow, md.Status = pair[0], pair[1]
			if err := Save(root, md); err != nil {
				t.Fatal(err)
			}
			got, err := ScanPrefix(root, "p/")
			if err != nil || len(got) != 1 {
				t.Fatalf("fresh scan: %v, %v", got, err)
			}
			if got[0].Workflow != md.Workflow || got[0].Status != md.Status {
				t.Fatalf("stale metadata: %+v", got[0])
			}
			got[0].Documents["design"].Sessions["claude"] = "mutated"
			if again, err := ScanPrefix(root, "p/"); err != nil || again[0].Documents["design"].Sessions["claude"] != "s1" {
				t.Fatalf("mutation leaked: %v, %v", again, err)
			}
		}
	}
	writeRun(t, root, "p", "new", StatusInProgress)
	if err := os.RemoveAll(filepath.Join(root, Dir("p", "r"))); err != nil {
		t.Fatal(err)
	}
	got, err := ScanPrefix(root, "p/")
	if err != nil || strings.Join(scanKeys(t, got), " ") != "p/new:in_progress" {
		t.Fatalf("after add/remove: %v, %v", scanKeys(t, got), err)
	}
}
