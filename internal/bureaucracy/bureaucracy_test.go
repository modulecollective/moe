package bureaucracy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func noEnv(string) string { return "" }

func writeMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, Marker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindWalksUpToMarker(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root)
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Find(nested, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantAbs, _ := filepath.Abs(root)
	gotAbs, _ := filepath.Abs(got)
	if gotAbs != wantAbs {
		t.Fatalf("got %q want %q", gotAbs, wantAbs)
	}
}

func TestFindReturnsNotFoundAtFilesystemRoot(t *testing.T) {
	dir := t.TempDir() // no marker anywhere up the chain to /
	_, err := Find(dir, noEnv)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestFindPrefersMoeHome(t *testing.T) {
	pwdRoot := t.TempDir()
	writeMarker(t, pwdRoot) // $PWD walk would find this
	homeRoot := t.TempDir()
	writeMarker(t, homeRoot)

	got, err := Find(pwdRoot, func(k string) string {
		if k == EnvHome {
			return homeRoot
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAbs, _ := filepath.Abs(homeRoot)
	if got != wantAbs {
		t.Fatalf("got %q want %q", got, wantAbs)
	}
}

func TestFindErrorsWhenMoeHomeLacksMarker(t *testing.T) {
	empty := t.TempDir()
	_, err := Find(t.TempDir(), func(k string) string {
		if k == EnvHome {
			return empty
		}
		return ""
	})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected descriptive error, got %v", err)
	}
}
