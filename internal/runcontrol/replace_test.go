package runcontrol

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func preserveReplaceOps(t *testing.T) {
	t.Helper()
	create, write, close, chmod, rename := replaceCreateTemp, replaceWrite, replaceClose, replaceChmod, replaceRename
	t.Cleanup(func() {
		replaceCreateTemp, replaceWrite, replaceClose, replaceChmod, replaceRename = create, write, close, chmod, rename
	})
}

func requireNoControlTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".control-*.json"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary residue = %v, error = %v", matches, err)
	}
}

func requireControlBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func openControlGeneration(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func requireControlGeneration(t *testing.T, f *os.File, want string) {
	t.Helper()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != want {
		t.Fatalf("open generation = %q, %v; want %q", got, err, want)
	}
}

func TestWriteFilePublishesGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, Path("moe", "r1"))
	const old = "{\n  \"revision\": 0,\n  \"requests\": []\n}\n"
	if err := writeFile(path, File{}); err != nil {
		t.Fatal(err)
	}
	requireControlBytes(t, path, old)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("created mode = %o, want 644", info.Mode().Perm())
	}
	f, err := Load(root, "moe", "r1")
	if err != nil || f.Requests == nil || len(f.Requests) != 0 {
		t.Fatalf("Load empty record = %+v, %v", f, err)
	}
	// Replacement deliberately normalizes the machine-owned record's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := openControlGeneration(t, path)
	if err := writeFile(path, File{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	const next = "{\n  \"revision\": 1,\n  \"requests\": []\n}\n"
	requireControlGeneration(t, opened, old)
	requireControlBytes(t, path, next)
	f, err = Load(root, "moe", "r1")
	if err != nil || f.Revision != 1 || f.Requests == nil || len(f.Requests) != 0 {
		t.Fatalf("Load replaced record = %+v, %v", f, err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("replacement mode = %o, want 644", info.Mode().Perm())
	}
	requireNoControlTemps(t, path)
}

func TestRestorePublishesGeneration(t *testing.T) {
	root := seedRoot(t)
	rel := Path("moe", "r1")
	path := filepath.Join(root, rel)
	const original = " { \"revision\" : 0, \"requests\" : [] }\n\n"
	write(t, root, rel, original)
	before := readControlPath(t, root, rel)
	if err := writeFile(path, File{Revision: 1}); err != nil {
		t.Fatal(err)
	}
	attempted := readControlPath(t, root, rel)
	opened := openControlGeneration(t, path)
	cause := errors.New("rejected commit")
	if err := restore(root, rel, path, before.body, before.existed, before.index, cause); !errors.Is(err, cause) || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("restore = %v", err)
	}
	requireControlGeneration(t, opened, string(attempted.body))
	requireControlBytes(t, path, original)
	requireNoControlTemps(t, path)
}

func TestSubmitPartialWritePreservesDestination(t *testing.T) {
	for _, existed := range []bool{false, true} {
		t.Run(fmt.Sprintf("existed=%t", existed), func(t *testing.T) {
			root := seedRoot(t)
			rel := Path("moe", "r1")
			path := filepath.Join(root, rel)
			if existed {
				submit(t, root, Request{ID: "original", Kind: KindFinish})
			}
			before := readControlPath(t, root, rel)
			head := gittest.HeadSHA(t, root)
			preserveReplaceOps(t)
			failure := errors.New("partial temporary write")
			wrote := false
			replaceWrite = func(f *os.File, body []byte) (int, error) {
				n, err := f.Write(body[:len(body)/2])
				if err != nil {
					t.Fatal(err)
				}
				wrote = true
				// A reader during the partial write still opens the old record.
				requireControlPath(t, root, rel, before)
				if _, err := Load(root, "moe", "r1"); err != nil {
					t.Fatalf("reader during partial write: %v", err)
				}
				return n, failure
			}
			_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
			if !wrote || !errors.Is(err, failure) {
				t.Fatalf("partial write = %v, wrote = %t", err, wrote)
			}
			requireControlPath(t, root, rel, before)
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed write moved HEAD")
			}
			requireNoControlTemps(t, path)
		})
	}
}

func TestReplacePreparationFailure(t *testing.T) {
	for _, operation := range []string{"create", "write and close", "close", "chmod"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			const original = "original bytes\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			preserveReplaceOps(t)
			failure := errors.New(operation + " failed")
			closeFailure := errors.New("close after failed write")
			switch operation {
			case "create":
				replaceCreateTemp = func(string, string) (*os.File, error) { return nil, failure }
			case "write and close":
				replaceWrite = func(*os.File, []byte) (int, error) { return 0, failure }
				replaceClose = func(f *os.File) error { return errors.Join(f.Close(), closeFailure) }
			case "close":
				replaceClose = func(f *os.File) error { return errors.Join(f.Close(), failure) }
			case "chmod":
				replaceChmod = func(string, os.FileMode) error { return failure }
			}
			published := false
			replaceRename = func(string, string) error { published = true; return nil }
			err := replaceFile(path, []byte("replacement\n"))
			if !errors.Is(err, failure) || published {
				t.Fatalf("preparation failure = %v, published = %t", err, published)
			}
			if operation == "write and close" && !errors.Is(err, closeFailure) {
				t.Fatalf("lost close error: %v", err)
			}
			requireControlBytes(t, path, original)
			requireNoControlTemps(t, path)
		})
	}
}

func TestReplaceRenameFailureCleansTemp(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			failure := errors.New("rename failed")
			if directory {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				preserveReplaceOps(t)
				replaceRename = func(string, string) error { return failure }
			}
			err := replaceFile(path, []byte("replacement\n"))
			if err == nil || (!directory && !errors.Is(err, failure)) {
				t.Fatalf("rename failure = %v", err)
			}
			if directory {
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("destination directory changed: %v, %v", entries, err)
				}
			} else {
				requireControlBytes(t, path, "original\n")
			}
			requireNoControlTemps(t, path)
		})
	}
}

func TestReplaceReportsCleanupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	preserveReplaceOps(t)
	failure := errors.New("rename failed")
	var tmp string
	replaceRename = func(from, to string) error {
		tmp = from
		// A nonempty directory at the temporary path makes cleanup fail
		// under root as well, without introducing another operation seam.
		if err := os.Remove(from); err != nil {
			t.Fatal(err)
		}
		write(t, from, "blocker", "cleanup blocked")
		return failure
	}
	err := replaceFile(path, []byte("replacement\n"))
	var cleanupErr *os.PathError
	if !errors.Is(err, failure) || !errors.As(err, &cleanupErr) || cleanupErr.Path != tmp || !strings.Contains(err.Error(), "remove temporary file "+tmp) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if info, err := os.Stat(tmp); err != nil || !info.IsDir() {
		t.Fatalf("surviving temporary path = %v, %v", info, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed replacement created destination: %v", err)
	}
}

func TestControlCommitFailureKeepsGenerationWhenRestoreFails(t *testing.T) {
	for _, operation := range []string{"partial write", "rename"} {
		t.Run(operation, func(t *testing.T) {
			root := seedRoot(t)
			submit(t, root, Request{ID: "original", Kind: KindFinish})
			rel := Path("moe", "r1")
			path := filepath.Join(root, rel)
			// Distinct HEAD/staged/working bytes and staged mode must
			// survive independently of failed working-file restoration.
			original := string(readControlPath(t, root, rel).body)
			write(t, root, rel, original+"\n")
			gittest.Run(t, root, "add", "--", rel)
			gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
			write(t, root, rel, original+"\n\n")
			before := readControlPath(t, root, rel)
			beforeHead := head(t, root)
			rejectControlCommits(t, root, "")
			preserveReplaceOps(t)
			failure := errors.New("rollback " + operation + " failed")
			calls := 0
			var attempted string
			if operation == "partial write" {
				replaceWrite = func(f *os.File, body []byte) (int, error) {
					calls++
					if calls == 2 {
						attempted = string(readControlPath(t, root, rel).body)
						n, err := f.Write(body[:len(body)/2])
						if err != nil {
							t.Fatal(err)
						}
						requireControlBytes(t, path, attempted)
						return n, failure
					}
					return f.Write(body)
				}
			} else {
				replaceRename = func(from, to string) error {
					calls++
					if calls == 2 {
						attempted = string(readControlPath(t, root, rel).body)
						return failure
					}
					return os.Rename(from, to)
				}
			}
			_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
			// Keep restore's existing error composition: it wraps the
			// commit cause and includes recovery errors in the diagnostic.
			if calls != 2 || err == nil || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), failure.Error()) || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "restore file") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "nothing recorded") {
				t.Fatalf("failed rollback = %v, operation calls = %d", err, calls)
			}
			requireControlBytes(t, path, attempted)
			f := load(t, root)
			if len(f.Requests) != 2 || f.Requests[1].ID != "new" || !f.Held() || f.Hold.Request != "new" {
				t.Fatalf("attempted generation incomplete: %+v", f)
			}
			if got := gittest.Output(t, root, "ls-files", "-s", "--", rel); got != before.index {
				t.Fatalf("prior index not restored: %q, want %q", got, before.index)
			}
			if head(t, root) != beforeHead {
				t.Fatal("rejected commit moved HEAD")
			}
			requireNoControlTemps(t, path)
		})
	}
}
