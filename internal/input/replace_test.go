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

func preserveReplaceFile(t *testing.T) {
	t.Helper()
	orig := replaceFile
	t.Cleanup(func() { replaceFile = orig })
}

func requireNoInputTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
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

func TestAddPublishFailurePreservesDestination(t *testing.T) {
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
			preserveReplaceFile(t)
			failure := errors.New("publish failed")
			called := false
			replaceFile = func(string, []byte) error { called = true; return failure }
			_, err := Add(root, "moe", "destination", "rejected note")
			if !called || !errors.Is(err, failure) {
				t.Fatalf("failed publish = %v, called = %t", err, called)
			}
			requireInputPath(t, root, rel, before)
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed write moved HEAD")
			}
			requireNoInputTemps(t, path)
		})
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
	preserveReplaceFile(t)
	failure := errors.New("rollback publish failed")
	calls := 0
	var attempted string
	publish := replaceFile
	replaceFile = func(path string, body []byte) error {
		calls++
		if calls == 2 {
			attempted = string(readInputPath(t, root, rel).body)
			return failure
		}
		return publish(path, body)
	}
	_, err := Add(root, "moe", "destination", "rejected note")
	if calls != 2 || !errors.Is(err, failure) || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "restore file") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("failed rollback = %v, publish calls = %d", err, calls)
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
