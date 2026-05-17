package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestEnsureMaterializedSilentOnMissingMountpoint covers the
// "not registered here / project doesn't exist" shape — caller's own
// existence check fires later. The gate does not invent an error.
func TestEnsureMaterializedSilentOnMissingMountpoint(t *testing.T) {
	root := t.TempDir()
	if err := EnsureMaterialized(root, "ghost"); err != nil {
		t.Fatalf("missing mountpoint should be silent, got %v", err)
	}
}

// TestEnsureMaterializedSilentOnPopulatedSrc covers the warm-path
// short-circuit: a non-empty src means the submodule is already
// materialized and we do nothing.
func TestEnsureMaterializedSilentOnPopulatedSrc(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, SubmoduleDir("thing"))
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMaterialized(root, "thing"); err != nil {
		t.Fatalf("populated src should be silent, got %v", err)
	}
}

// TestEnsureMaterializedSilentOnUndeclaredSubmodule covers the
// "empty dir but .gitmodules doesn't claim it" shape — an unrelated
// empty projects/<id>/src/ on disk is not the gate's problem.
func TestEnsureMaterializedSilentOnUndeclaredSubmodule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, SubmoduleDir("thing")), 0o755); err != nil {
		t.Fatal(err)
	}
	// No .gitmodules at all — silent.
	if err := EnsureMaterialized(root, "thing"); err != nil {
		t.Fatalf("missing .gitmodules should be silent, got %v", err)
	}
	// .gitmodules without our stanza — still silent.
	other := "[submodule \"projects/other/src\"]\n\tpath = projects/other/src\n\turl = file:///nope\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMaterialized(root, "thing"); err != nil {
		t.Fatalf("undeclared submodule should be silent, got %v", err)
	}
}

// TestEnsureMaterializedFailureSurfacesTypedError covers the
// fail-loud path: a declared submodule with a bogus URL surfaces a
// *SubmoduleInitError whose message includes the --recursive retry
// command. Keeps the error shape sandbox used to emit, and locks in
// the design's "recursive retry" choice.
func TestEnsureMaterializedFailureSurfacesTypedError(t *testing.T) {
	gittest.SetupEnv(t)
	root := t.TempDir()
	gittest.Run(t, root, "init", "-b", "main")

	if err := os.MkdirAll(filepath.Join(root, SubmoduleDir("thing")), 0o755); err != nil {
		t.Fatal(err)
	}
	gm := "[submodule \"projects/thing/src\"]\n" +
		"\tpath = projects/thing/src\n" +
		"\turl = file:///definitely-does-not-exist-xyz\n"
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte(gm), 0o644); err != nil {
		t.Fatal(err)
	}

	err := EnsureMaterialized(root, "thing")
	if err == nil {
		t.Fatal("EnsureMaterialized should fail on bogus submodule URL")
	}
	var sie *SubmoduleInitError
	if !errors.As(err, &sie) {
		t.Fatalf("expected *SubmoduleInitError, got %T: %v", err, err)
	}
	if sie.ProjectID != "thing" {
		t.Errorf("ProjectID = %q, want thing", sie.ProjectID)
	}
	want := "git submodule update --init --recursive projects/thing/src"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error should name the recursive retry command (%q): %v", want, err)
	}
}
