package run_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func TestArtifactReaderUsesSparseIndexWithoutHydration(t *testing.T) {
	root := gittest.Init(t)
	path := run.ThreadPathFor("codex", "beta", "history", "design")
	missing := run.ThreadPathFor("claude", "alpha", "active", "design")
	diagnostic := "projects/beta/runs/history/diagnostics/design/attempt/thread-claude.jsonl"
	for _, p := range []string{path, missing, diagnostic} {
		gittest.WriteAndCommit(t, root, p, "old\n", "seed")
	}
	s, err := session.Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	gittest.WriteAndCommit(t, root, missing, "indexed\n", "index donor")
	// Stage a newer blob directly in this index without touching main.
	oid := gittest.Output(t, root, "rev-parse", "HEAD:"+missing)
	gittest.WriteAndCommit(t, root, missing, "main changed\n", "advance main")
	gittest.Run(t, s.WorktreePath, "update-index", "--cacheinfo", "100644,"+oid+","+path)
	gittest.Run(t, s.WorktreePath, "update-index", "--skip-worktree", "--", path)
	a := run.NewArtifactReader(s.WorktreePath)
	read := func(p, want string) {
		t.Helper()
		f, err := a.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", p, b, err)
		}
	}
	for _, p := range []string{path, diagnostic} {
		exists, err := a.Exists(p)
		if err != nil || !exists {
			t.Fatalf("Exists: %v %v", exists, err)
		}
		want := "old\n"
		if p == path {
			want = "indexed\n"
		}
		read(p, want)
		if _, err := os.Stat(filepath.Join(s.WorktreePath, p)); !os.IsNotExist(err) {
			t.Fatalf("hydrated %s", p)
		}
	}
	ids, err := a.DocumentIDs("beta", "history")
	if err != nil || !reflect.DeepEqual(ids, []string{"design"}) {
		t.Fatalf("stages=%v %v", ids, err)
	}
	// A materialized local edit wins even when the index says skip-worktree.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.WorktreePath, path)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.WorktreePath, path), []byte("local\n"), 0644); err != nil {
		t.Fatal(err)
	}
	read(path, "local\n")
	// A real deletion in the included run is not resurrected from the index.
	if err := os.Remove(filepath.Join(s.WorktreePath, missing)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Open(missing); !os.IsNotExist(err) {
		t.Fatalf("deleted file resurrected: %v", err)
	}
	if exists, err := a.Exists(missing); err != nil || exists {
		t.Fatalf("deleted Exists=%v %v", exists, err)
	}
	if _, err := a.Open("../outside"); err == nil {
		t.Fatal("accepted escaping path")
	}
}

func TestArtifactReaderReturnsIndexErrors(t *testing.T) {
	root := gittest.Init(t)
	gittest.Commit(t, root, "seed")
	if err := os.WriteFile(filepath.Join(root, ".git", "index"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	a := run.NewArtifactReader(root)
	if _, err := a.Exists("missing"); err == nil {
		t.Fatal("index failure became absence")
	}
	if _, err := a.Open("missing"); err == nil || os.IsNotExist(err) {
		t.Fatalf("index failure became absence: %v", err)
	}
}

func TestArtifactReaderReturnsBlobAndFilesystemErrors(t *testing.T) {
	root := gittest.Init(t)
	path := run.ThreadPathFor("claude", "beta", "history", "design")
	gittest.WriteAndCommit(t, root, path, "archive\n", "seed")
	s, err := session.Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, s.WorktreePath, "update-index", "--cacheinfo", "100644,"+strings.Repeat("1", 40)+","+path)
	gittest.Run(t, s.WorktreePath, "update-index", "--skip-worktree", "--", path)
	a := run.NewArtifactReader(s.WorktreePath)
	if exists, err := a.Exists(path); err != nil || !exists {
		t.Fatalf("indexed artifact not discovered: %v, %v", exists, err)
	}
	if _, err := a.Open(path); err == nil || os.IsNotExist(err) {
		t.Fatalf("missing blob became an absent artifact: %v", err)
	}
	file := filepath.Join(s.WorktreePath, path)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	// A present but unreadable path must not fall back to its indexed blob.
	if err := os.Symlink(filepath.Base(file), file); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Open(path); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("filesystem read error lost: %v", err)
	}
	if _, err := a.Exists(path); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("filesystem discovery error lost: %v", err)
	}
}
