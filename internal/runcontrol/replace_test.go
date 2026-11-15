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

func preserveReplaceFile(t *testing.T) {
	t.Helper()
	orig := replaceFile
	t.Cleanup(func() { replaceFile = orig })
}

func requireNoControlTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
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

func TestSubmitPublishFailurePreservesDestination(t *testing.T) {
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
			preserveReplaceFile(t)
			failure := errors.New("publish failed")
			called := false
			replaceFile = func(string, []byte) error { called = true; return failure }
			_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
			if !called || !errors.Is(err, failure) {
				t.Fatalf("failed publish = %v, called = %t", err, called)
			}
			requireControlPath(t, root, rel, before)
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed write moved HEAD")
			}
			requireNoControlTemps(t, path)
		})
	}
}

func TestControlCommitFailureKeepsGenerationWhenRestoreFails(t *testing.T) {
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
	preserveReplaceFile(t)
	failure := errors.New("rollback publish failed")
	calls := 0
	var attempted string
	publish := replaceFile
	replaceFile = func(path string, body []byte) error {
		calls++
		if calls == 2 {
			attempted = string(readControlPath(t, root, rel).body)
			return failure
		}
		return publish(path, body)
	}
	_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
	// Keep restore's existing error composition: it wraps the
	// commit cause and includes recovery errors in the diagnostic.
	if calls != 2 || err == nil || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), failure.Error()) || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "restore file") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("failed rollback = %v, publish calls = %d", err, calls)
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
}
