package runcontrol

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preserveUnrecordedOps(t *testing.T) {
	t.Helper()
	write, close, rename := unrecordedWrite, unrecordedClose, unrecordedRename
	t.Cleanup(func() {
		unrecordedWrite, unrecordedClose, unrecordedRename = write, close, rename
	})
}

func requireNoUnrecordedTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".unrecorded-*.json"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary residue = %v, error = %v", matches, err)
	}
}

func requireUnrecordedDestination(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination should remain absent: %q, %v", got, err)
		}
	} else if err != nil || string(got) != string(want) {
		t.Fatalf("destination = %q, %v; want %q", got, err, want)
	}
}

func TestUnrecordedPublishesPrivateReceipts(t *testing.T) {
	root := t.TempDir()
	path := unrecordedPath(root, "moe", "r1")
	want := map[string]Settlement{}
	check := func() {
		t.Helper()
		got, err := Unrecorded(root, "moe", "r1")
		if err != nil || !maps.Equal(got, want) {
			t.Fatalf("receipts = %+v, %v; want %+v", got, err, want)
		}
		body, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(body), "\n") || !strings.Contains(string(body), "\n  \"") {
			t.Fatalf("indented JSON with final newline = %q, %v", body, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("receipt mode = %o, want 600", info.Mode().Perm())
		}
		requireNoUnrecordedTemps(t, path)
	}
	for _, entry := range []struct {
		id string
		s  Settlement
	}{
		{"completed", Settlement{State: StateCompleted, Boundary: "stage code", Outcome: "held", RecordingError: "commit refused"}},
		{"failed", Settlement{State: StateFailed, Boundary: "stage test", Outcome: "stopped", Recovery: true, RecordingError: "lock timed out"}},
		{"completed", Settlement{State: StateCompleted, Boundary: "stage review", Outcome: "finished", RecordingError: "hook refused"}},
	} {
		if err := KeepUnrecorded(root, "moe", "r1", entry.id, entry.s); err != nil {
			t.Fatal(err)
		}
		want[entry.id] = entry.s
		check()
	}
	if err := dropUnrecorded(root, "moe", "r1", "completed"); err != nil {
		t.Fatal(err)
	}
	delete(want, "completed")
	check()
}

func TestUnrecordedRenameRefusalCleansTemp(t *testing.T) {
	root := t.TempDir()
	path := unrecordedPath(root, "moe", "r1")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := writeUnrecorded(root, "moe", "r1", map[string]Settlement{"f1": {State: StateFailed}})
	var renameErr *os.LinkError
	if !errors.As(err, &renameErr) || renameErr.Op != "rename" || renameErr.New != path {
		t.Fatalf("rename refusal = %v", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination directory changed: %v, %v", entries, err)
	}
	requireNoUnrecordedTemps(t, path)
}

func TestUnrecordedRenameFailurePreservesDestination(t *testing.T) {
	for _, existed := range []bool{false, true} {
		t.Run(fmt.Sprintf("existed=%t", existed), func(t *testing.T) {
			root := t.TempDir()
			path := unrecordedPath(root, "moe", "r1")
			var original []byte
			if existed {
				if err := KeepUnrecorded(root, "moe", "r1", "old", Settlement{State: StateCompleted}); err != nil {
					t.Fatal(err)
				}
				var err error
				original, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			preserveUnrecordedOps(t)
			failure := errors.New("rename refused")
			unrecordedRename = func(string, string) error { return failure }
			err := KeepUnrecorded(root, "moe", "r1", "new", Settlement{State: StateFailed})
			if !errors.Is(err, failure) {
				t.Fatalf("publication error = %v", err)
			}
			requireUnrecordedDestination(t, path, original)
			requireNoUnrecordedTemps(t, path)
		})
	}
}

func TestUnrecordedPreparationFailure(t *testing.T) {
	for _, operation := range []string{"write", "close", "write and close"} {
		for _, existed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existed=%t", operation, existed), func(t *testing.T) {
				root := t.TempDir()
				path := unrecordedPath(root, "moe", "r1")
				var original []byte
				if existed {
					original = []byte("previous receipt bytes\n")
					write(t, root, unrecordedPath("", "moe", "r1"), string(original))
				}
				preserveUnrecordedOps(t)
				writeFailure := errors.New("partial write failed")
				closeFailure := errors.New("close failed")
				failWrite := operation != "close"
				failClose := operation != "write"
				wrote, closed, published := false, false, false
				if failWrite {
					unrecordedWrite = func(f *os.File, body []byte) (int, error) {
						n, err := f.Write(body[:len(body)/2])
						if err != nil {
							t.Fatal(err)
						}
						wrote = true
						requireUnrecordedDestination(t, path, original)
						return n, writeFailure
					}
				}
				unrecordedClose = func(f *os.File) error {
					if failWrite && !wrote {
						t.Fatal("closed before attempting write")
					}
					closed = true
					err := f.Close()
					if failClose {
						return errors.Join(err, closeFailure)
					}
					return err
				}
				unrecordedRename = func(string, string) error { published = true; return nil }
				err := writeUnrecorded(root, "moe", "r1", map[string]Settlement{"new": {State: StateFailed}})
				if !closed || published || (failWrite && !errors.Is(err, writeFailure)) || (failClose && !errors.Is(err, closeFailure)) {
					t.Fatalf("preparation error = %v, closed = %t, published = %t", err, closed, published)
				}
				requireUnrecordedDestination(t, path, original)
				requireNoUnrecordedTemps(t, path)
			})
		}
	}
}

func TestUnrecordedCleanupFailure(t *testing.T) {
	for _, existed := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("existed=%t/missing=%t", existed, missing), func(t *testing.T) {
				root := t.TempDir()
				path := unrecordedPath(root, "moe", "r1")
				var original []byte
				if existed {
					original = []byte("previous receipt bytes\n")
					write(t, root, unrecordedPath("", "moe", "r1"), string(original))
				}
				preserveUnrecordedOps(t)
				failure := errors.New("publication failed")
				var tmp string
				unrecordedRename = func(from, to string) error {
					tmp = from
					if err := os.Remove(from); err != nil {
						t.Fatal(err)
					}
					if !missing {
						// A nonempty directory refuses Remove even under root.
						write(t, from, "blocker", "cleanup blocked")
					}
					return failure
				}
				err := writeUnrecorded(root, "moe", "r1", map[string]Settlement{"new": {State: StateFailed}})
				if !errors.Is(err, failure) {
					t.Fatalf("lost publication error: %v", err)
				}
				if missing {
					if err != failure {
						t.Fatalf("not-found cleanup added an error: %v", err)
					}
					requireNoUnrecordedTemps(t, path)
				} else {
					var cleanupErr *os.PathError
					if !errors.As(err, &cleanupErr) || cleanupErr.Op != "remove" || cleanupErr.Path != tmp || !strings.Contains(err.Error(), "remove temporary file "+tmp) {
						t.Fatalf("cleanup failure = %v", err)
					}
					body, readErr := os.ReadFile(filepath.Join(tmp, "blocker"))
					if readErr != nil || string(body) != "cleanup blocked" {
						t.Fatalf("surviving temporary contents = %q, %v", body, readErr)
					}
				}
				requireUnrecordedDestination(t, path, original)
			})
		}
	}
}
