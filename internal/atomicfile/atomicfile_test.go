package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preserveOps(t *testing.T) {
	t.Helper()
	c, w, cl, ch, r := createTemp, write, closeFile, chmod, rename
	t.Cleanup(func() { createTemp, write, closeFile, chmod, rename = c, w, cl, ch, r })
}

func requireNoTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary residue = %v, error = %v", matches, err)
	}
}

func requireBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode = %o, want %o", info.Mode().Perm(), want)
	}
}

func TestWritePublishesGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "record.json")
	if err := Write(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireBytes(t, path, "old\n")
	requireMode(t, path, 0o644)

	// Replacement normalizes the mode and leaves an open reader on the
	// generation it opened.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := Write(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(opened)
	if err != nil || string(got) != "old\n" {
		t.Fatalf("open generation = %q, %v", got, err)
	}
	requireBytes(t, path, "new\n")
	requireMode(t, path, 0o644)
	requireNoTemps(t, path)

	if err := Write(path, []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path, 0o600)
}

func TestWritePartialWriteKeepsDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preserveOps(t)
	failure := errors.New("partial temporary write")
	wrote := false
	write = func(f *os.File, body []byte) (int, error) {
		n, err := f.Write(body[:len(body)/2])
		if err != nil {
			t.Fatal(err)
		}
		wrote = true
		// A reader during the partial write still opens the old record.
		requireBytes(t, path, "original\n")
		return n, failure
	}
	err := Write(path, []byte("replacement\n"), 0o644)
	if !wrote || !errors.Is(err, failure) {
		t.Fatalf("partial write = %v, wrote = %t", err, wrote)
	}
	requireBytes(t, path, "original\n")
	requireNoTemps(t, path)
}

func TestWritePreparationFailure(t *testing.T) {
	for _, operation := range []string{"create", "write and close", "close", "chmod"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.json")
			const original = "original bytes\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			preserveOps(t)
			failure := errors.New(operation + " failed")
			closeFailure := errors.New("close after failed write")
			switch operation {
			case "create":
				createTemp = func(string, string) (*os.File, error) { return nil, failure }
			case "write and close":
				write = func(*os.File, []byte) (int, error) { return 0, failure }
				closeFile = func(f *os.File) error { return errors.Join(f.Close(), closeFailure) }
			case "close":
				closeFile = func(f *os.File) error { return errors.Join(f.Close(), failure) }
			case "chmod":
				chmod = func(string, os.FileMode) error { return failure }
			}
			published := false
			rename = func(string, string) error { published = true; return nil }
			err := Write(path, []byte("replacement\n"), 0o644)
			if !errors.Is(err, failure) || published {
				t.Fatalf("preparation failure = %v, published = %t", err, published)
			}
			if operation == "write and close" && !errors.Is(err, closeFailure) {
				t.Fatalf("lost close error: %v", err)
			}
			requireBytes(t, path, original)
			requireNoTemps(t, path)
		})
	}
}

func TestWriteRenameFailureCleansTemp(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.json")
			failure := errors.New("rename failed")
			if directory {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				preserveOps(t)
				rename = func(string, string) error { return failure }
			}
			err := Write(path, []byte("replacement\n"), 0o644)
			if err == nil || (!directory && !errors.Is(err, failure)) {
				t.Fatalf("rename failure = %v", err)
			}
			if directory {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("destination directory changed: %v, %v", entries, err)
				}
			} else {
				requireBytes(t, path, "original\n")
			}
			requireNoTemps(t, path)
		})
	}
}

func TestWriteReportsCleanupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	preserveOps(t)
	failure := errors.New("rename failed")
	var tmp string
	rename = func(from, to string) error {
		tmp = from
		// A nonempty directory at the temporary path makes cleanup fail
		// under root as well, without introducing another operation seam.
		if err := os.Remove(from); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(from, "blocker"), 0o755); err != nil {
			t.Fatal(err)
		}
		return failure
	}
	err := Write(path, []byte("replacement\n"), 0o644)
	var cleanupErr *os.PathError
	if !errors.Is(err, failure) || !errors.As(err, &cleanupErr) || cleanupErr.Op != "remove" || cleanupErr.Path != tmp || !strings.Contains(err.Error(), "remove temporary file "+tmp) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed replacement created destination: %v", err)
	}
}

func TestWriteMissingTempAddsNoCleanupError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	preserveOps(t)
	failure := errors.New("rename failed")
	rename = func(from, to string) error {
		if err := os.Remove(from); err != nil {
			t.Fatal(err)
		}
		return failure
	}
	if err := Write(path, []byte("replacement\n"), 0o644); err != failure {
		t.Fatalf("not-found cleanup changed the error: %v", err)
	}
}
