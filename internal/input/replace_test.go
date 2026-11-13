package input

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

func requireNoInputTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".inputs-*.json"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary residue = %v, error = %v", matches, err)
	}
}

func requireInputBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func openInputGeneration(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func requireInputGeneration(t *testing.T, f *os.File, want string) {
	t.Helper()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != want {
		t.Fatalf("open generation = %q, %v; want %q", got, err, want)
	}
}

func TestWriteFilePublishesGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, Path("moe", "destination"))
	const old = "{\n  \"notes\": null\n}\n"
	if err := writeFile(path, File{}); err != nil {
		t.Fatal(err)
	}
	requireInputBytes(t, path, old)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("created mode = %o, want 644", info.Mode().Perm())
	}
	f, err := Load(root, "moe", "destination")
	if err != nil || len(f.Notes) != 0 {
		t.Fatalf("Load empty record = %+v, %v", f, err)
	}
	// Replacement deliberately normalizes the machine-owned record's mode.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := openInputGeneration(t, path)
	if err := writeFile(path, File{Notes: []Entry{{ID: 1, Text: "new note"}}}); err != nil {
		t.Fatal(err)
	}
	const next = "{\n  \"notes\": [\n    {\n      \"id\": 1,\n      \"text\": \"new note\"\n    }\n  ]\n}\n"
	requireInputGeneration(t, opened, old)
	requireInputBytes(t, path, next)
	f, err = Load(root, "moe", "destination")
	if err != nil || len(f.Notes) != 1 || f.Notes[0].Text != "new note" {
		t.Fatalf("Load replaced record = %+v, %v", f, err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("replacement mode = %o, want 644", info.Mode().Perm())
	}
	requireNoInputTemps(t, path)
}

func TestRestorePublishesGeneration(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	rel := Path("moe", "destination")
	path := filepath.Join(root, rel)
	const original = " { \"notes\" : [ {\"id\":1, \"text\":\"operator note\"} ] }\n\n"
	write(t, root, rel, original)
	snap, err := snapFile(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(path, File{Notes: []Entry{{ID: 1, Text: "attempted note"}}}); err != nil {
		t.Fatal(err)
	}
	attempted := readInputPath(t, root, rel)
	opened := openInputGeneration(t, path)
	cause := errors.New("rejected commit")
	if err := snap.restore(root, cause); !errors.Is(err, cause) || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("restore = %v", err)
	}
	requireInputGeneration(t, opened, string(attempted.body))
	requireInputBytes(t, path, original)
	requireNoInputTemps(t, path)
}

func TestAddPartialWritePreservesDestination(t *testing.T) {
	for _, existed := range []bool{false, true} {
		t.Run(fmt.Sprintf("existed=%t", existed), func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "destination")
			rel := Path("moe", "destination")
			path := filepath.Join(root, rel)
			if existed {
				add(t, root, "destination", "original note")
			}
			before := readInputPath(t, root, rel)
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
				requireInputPath(t, root, rel, before)
				if _, err := Load(root, "moe", "destination"); err != nil {
					t.Fatalf("reader during partial write: %v", err)
				}
				return n, failure
			}
			_, err := Add(root, "moe", "destination", "rejected note")
			if !wrote || !errors.Is(err, failure) {
				t.Fatalf("partial write = %v, wrote = %t", err, wrote)
			}
			requireInputPath(t, root, rel, before)
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed write moved HEAD")
			}
			requireNoInputTemps(t, path)
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
			requireInputBytes(t, path, original)
			requireNoInputTemps(t, path)
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
				requireInputBytes(t, path, "original\n")
			}
			requireNoInputTemps(t, path)
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
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "remove temporary file "+tmp) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed replacement created destination: %v", err)
	}
}

func TestInputCommitFailureKeepsGenerationWhenRestoreFails(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	add(t, root, "destination", "original note")
	rel := Path("moe", "destination")
	path := filepath.Join(root, rel)
	// Distinct staged bytes and mode must survive failed file restoration.
	original := string(readInputPath(t, root, rel).body)
	write(t, root, rel, original+"\n")
	gittest.Run(t, root, "add", "--", rel)
	gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
	before := readInputPath(t, root, rel)
	head := gittest.HeadSHA(t, root)
	rejectInputCommits(t, root, "")
	preserveReplaceOps(t)
	failure := errors.New("rollback rename failed")
	calls := 0
	var attempted string
	replaceRename = func(from, to string) error {
		calls++
		if calls == 2 {
			attempted = string(readInputPath(t, root, rel).body)
			return failure
		}
		return os.Rename(from, to)
	}
	_, err := Add(root, "moe", "destination", "rejected note")
	if calls != 2 || !errors.Is(err, failure) || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "restore file") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("failed rollback = %v, rename calls = %d", err, calls)
	}
	requireInputBytes(t, path, attempted)
	f, err := Load(root, "moe", "destination")
	if err != nil || len(f.Notes) != 2 || f.Notes[1].Text != "rejected note" {
		t.Fatalf("attempted generation incomplete: %+v, %v", f, err)
	}
	if got := gittest.Output(t, root, "ls-files", "-s", "--", rel); got != before.index {
		t.Fatalf("prior index not restored: %q, want %q", got, before.index)
	}
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("rejected commit moved HEAD")
	}
	requireNoInputTemps(t, path)
}
