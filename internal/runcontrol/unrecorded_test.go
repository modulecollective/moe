package runcontrol

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func requireNoUnrecordedTemps(t *testing.T, path string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary residue = %v, error = %v", matches, err)
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
