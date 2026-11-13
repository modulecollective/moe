package run

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/trailers"
)

func newFailureHook(t *testing.T, root, body string) {
	t.Helper()
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
}

func assertNewAbsent(t *testing.T, root, id string) {
	t.Helper()
	rel := Dir("tele", id)
	if _, err := os.Lstat(filepath.Join(root, rel)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory survived: %v", err)
	}
	if got := gittest.Output(t, root, "ls-files", "--stage", "--", rel); got != "" {
		t.Fatalf("run index entries survived: %s", got)
	}
	if taken, err := SlugTaken(root, "tele", id); err != nil || taken {
		t.Fatalf("slug not retryable: taken=%v, err=%v", taken, err)
	}
}

func TestNewSuccessPreservesUnrelatedIndex(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes", "HEAD notes")
	write("deleted", "delete me")
	write("executable", "mode change")
	gittest.Commit(t, root, "fixture")
	write("notes", "staged notes")
	write("added", "staged addition")
	if err := os.Chmod(filepath.Join(root, "executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", "notes", "added", "executable")
	gittest.Run(t, root, "rm", "--", "deleted")
	write("notes", "working notes")
	write("added", "working addition")
	write("stray", "untracked")
	foreign := []string{"notes", "added", "deleted", "executable", "stray"}
	indexArgs := append([]string{"ls-files", "--stage", "--"}, foreign...)
	index := gittest.Output(t, root, indexArgs...)
	if !strings.Contains(index, "100755 ") {
		t.Fatal("fixture has no executable index entry")
	}
	status := gittest.Output(t, root, "status", "--porcelain")
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	opts := Options{ID: "fix-it", Workflow: "sdlc", SeedDocs: map[string]string{
		"design": "# Design\n", "code": "# Code\n",
	}, Trailers: trailers.Block{Idea: "source", SpawnedBy: "tele/sweep", Consent: "dynamic"}}
	if md, err := New(root, "tele", opts); md != nil || err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("default dirty-tree refusal = %+v, %v", md, err)
	}
	assertNewAbsent(t, root, opts.ID)
	opts.AllowDirty = true
	md, err := New(root, "tele", opts)
	if err != nil || md.ID != opts.ID || md.Workflow != opts.Workflow || len(md.Documents) != 0 {
		t.Fatalf("New = %+v, %v", md, err)
	}
	if got := gittest.Output(t, root, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("open commits = %s, want 1", got)
	}
	wantPaths := []string{filepath.Join(Dir("tele", opts.ID), "run.json")}
	for doc, want := range opts.SeedDocs {
		rel := ContentPath("tele", opts.ID, doc)
		wantPaths = append(wantPaths, rel)
		if got := gittest.Output(t, root, "show", "HEAD:"+rel); got != strings.TrimSpace(want) {
			t.Fatalf("committed seed %s = %q, want %q", doc, got, want)
		}
	}
	slices.Sort(wantPaths)
	paths := strings.Fields(gittest.Output(t, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("open commit paths = %v, want %v", paths, wantPaths)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, want := range []string{"Open run tele/fix-it", "MoE-Run: fix-it", "MoE-Project: tele", "MoE-Idea: source", "MoE-Spawned-By: tele/sweep", "MoE-Consent: dynamic"} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "MoE-Document:") {
		t.Fatal("seed satisfied a stage")
	}
	if got := gittest.Output(t, root, indexArgs...); got != index {
		t.Fatalf("foreign index changed:\n%s\nwant:\n%s", got, index)
	}
	if got := gittest.Output(t, root, "status", "--porcelain"); got != status {
		t.Fatalf("foreign status changed:\n%s\nwant:\n%s", got, status)
	}
	for rel, want := range map[string]string{"notes": "working notes", "added": "working addition", "executable": "mode change", "stray": "untracked"} {
		if b, err := os.ReadFile(filepath.Join(root, rel)); err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", rel, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged deletion restored: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "executable")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("working executable mode changed: %v, %v", info, err)
	}
}

func TestNewCommitFailureRestoresAndRetries(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	opts := Options{ID: "fix-it", Workflow: "sdlc", SeedDocs: map[string]string{
		"design": "# Design\n", "code": "# Code\n",
	}, Trailers: trailers.Block{Idea: "source", SpawnedBy: "tele/sweep", Consent: "dynamic"}}
	newFailureHook(t, root, "echo reject-new >&2\nexit 1\n")
	md, err := New(root, "tele", opts)
	if md != nil || err == nil || !strings.Contains(err.Error(), "reject-new") {
		t.Fatalf("failed New = %+v, %v", md, err)
	}
	if !strings.Contains(err.Error(), "nothing recorded") {
		t.Errorf("successful cleanup not reported: %v", err)
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("failed New moved HEAD")
	}
	assertNewAbsent(t, root, opts.ID)
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	md, err = New(root, "tele", opts)
	if err != nil || md.ID != opts.ID || len(md.Documents) != 0 {
		t.Fatalf("retry = %+v, %v", md, err)
	}
	for doc, want := range opts.SeedDocs {
		b, err := os.ReadFile(filepath.Join(root, ContentPath("tele", opts.ID, doc)))
		if err != nil || string(b) != want {
			t.Fatalf("seed %s = %q, %v", doc, b, err)
		}
	}
	if got := gittest.Output(t, root, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("retry commits = %s", got)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, want := range []string{"Open run tele/fix-it", "MoE-Run: fix-it", "MoE-Project: tele", "MoE-Idea: source", "MoE-Spawned-By: tele/sweep", "MoE-Consent: dynamic"} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "MoE-Document:") {
		t.Fatal("seed satisfied a stage")
	}
}

func TestNewFailurePreservesDirtyWork(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Old target content has no run trailers. Its staged deletion is the
	// prior index state; rollback must not resurrect it from HEAD.
	oldTarget := filepath.Join(Dir("tele", "fix-it"), "run.json")
	write(oldTarget, "old target")
	write("notes", "HEAD")
	write("deleted", "delete me")
	gittest.Commit(t, root, "fixture")
	gittest.Run(t, root, "rm", "--", oldTarget, "deleted")
	if err := os.RemoveAll(filepath.Join(root, Dir("tele", "fix-it"))); err != nil {
		t.Fatal(err)
	}
	write("notes", "staged")
	gittest.Run(t, root, "add", "--", "notes")
	write("notes", "working")
	write("scratch", "untracked")
	index := gittest.Output(t, root, "ls-files", "--stage")
	status := gittest.Output(t, root, "status", "--porcelain")
	head := gittest.Output(t, root, "rev-parse", "HEAD")
	newFailureHook(t, root, "echo reject-dirty >&2\nexit 1\n")
	md, err := New(root, "tele", Options{ID: "fix-it", Workflow: "idea", AllowDirty: true,
		SeedDocs: map[string]string{"idea": "new idea"}})
	if md != nil || err == nil || !strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("New = %+v, %v", md, err)
	}
	assertNewAbsent(t, root, "fix-it")
	if got := gittest.Output(t, root, "ls-files", "--stage"); got != index {
		t.Fatalf("index changed:\n%s\nwant:\n%s", got, index)
	}
	if got := gittest.Output(t, root, "status", "--porcelain"); got != status {
		t.Fatalf("status changed: %s, want %s", got, status)
	}
	for rel, want := range map[string]string{"notes": "working", "scratch": "untracked"} {
		if b, err := os.ReadFile(filepath.Join(root, rel)); err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", rel, b, err)
		}
	}
	if gittest.Output(t, root, "rev-parse", "HEAD") != head {
		t.Fatal("failed New moved HEAD")
	}
}

func TestNewRefusesUnownedNamespace(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "index", "unmerged-index"} {
		t.Run(kind, func(t *testing.T) {
			root := newTestRoot(t)
			seedProject(t, root, "tele")
			rel := Dir("tele", "fix-it")
			abs := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory", "index", "unmerged-index":
				if err := os.Mkdir(abs, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(abs, "mine"), []byte("mine"), 0o644); err != nil {
					t.Fatal(err)
				}
				if kind == "unmerged-index" {
					gittest.Commit(t, root, "base")
					for _, body := range []string{"ours", "theirs"} {
						if err := os.WriteFile(filepath.Join(abs, "mine"), []byte(body), 0o644); err != nil {
							t.Fatal(err)
						}
						gittest.Commit(t, root, body)
					}
					gittest.Run(t, root, "read-tree", "--reset", "-u", "HEAD~1")
					gittest.Run(t, root, "read-tree", "-m", "HEAD~2", "HEAD~1", "HEAD")
					if got := gittest.Output(t, root, "ls-files", "--unmerged", "--", rel); got == "" {
						t.Fatal("fixture has no unmerged entries")
					}
				}
				if kind == "index" || kind == "unmerged-index" {
					if kind == "index" {
						gittest.Run(t, root, "add", "--", rel)
					}
					if err := os.RemoveAll(abs); err != nil {
						t.Fatal(err)
					}
				}
			case "file":
				if err := os.WriteFile(abs, []byte("mine"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-target", abs); err != nil {
					t.Fatal(err)
				}
			}
			index := gittest.Output(t, root, "ls-files", "--stage")
			md, err := New(root, "tele", Options{ID: "fix-it", Workflow: "sdlc", AllowDirty: true})
			if md != nil || err == nil {
				t.Fatalf("unowned New = %+v, %v", md, err)
			}
			if got := gittest.Output(t, root, "ls-files", "--stage"); got != index {
				t.Fatal("refusal changed index")
			}
			switch kind {
			case "index", "unmerged-index":
				if _, err := os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("index refusal wrote directory: %v", err)
				}
			case "symlink":
				if got, err := os.Readlink(abs); err != nil || got != "missing-target" {
					t.Fatalf("symlink = %q, %v", got, err)
				}
			default:
				path := abs
				if kind == "directory" {
					path = filepath.Join(abs, "mine")
				}
				if b, err := os.ReadFile(path); err != nil || string(b) != "mine" {
					t.Fatalf("existing bytes = %q, %v", b, err)
				}
			}
		})
	}
}

func TestNewIndexReadFailureWritesNothing(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")
	index := filepath.Join(root, ".git", "index")
	if err := os.WriteFile(index, []byte("broken index"), 0o644); err != nil {
		t.Fatal(err)
	}
	md, err := New(root, "tele", Options{ID: "fix-it", Workflow: "sdlc", AllowDirty: true})
	if md != nil || err == nil || !strings.Contains(err.Error(), "read index") {
		t.Fatalf("New with unreadable index = %+v, %v", md, err)
	}
	if _, err := os.Lstat(filepath.Join(root, Dir("tele", "fix-it"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index read failure wrote directory: %v", err)
	}
	if b, err := os.ReadFile(index); err != nil || string(b) != "broken index" {
		t.Fatalf("pre-existing index changed: %q, %v", b, err)
	}
}

func TestNewEarlierFailures(t *testing.T) {
	for _, kind := range []string{"invalid-seed", "mkdir-seed", "add"} {
		t.Run(kind, func(t *testing.T) {
			root := newTestRoot(t)
			seedProject(t, root, "tele")
			opts := Options{ID: "fix-it", Workflow: "sdlc", SeedDocs: map[string]string{"design": "valid"}}
			switch kind {
			case "invalid-seed":
				opts.SeedDocs["../escape"] = "bad"
			case "mkdir-seed":
				opts.SeedDocs[strings.Repeat("a", 256)] = "too long for filesystem"
			case "add":
				if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("content.md\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			head := gittest.Output(t, root, "rev-parse", "HEAD")
			if md, err := New(root, "tele", opts); md != nil || err == nil {
				t.Fatalf("New = %+v, %v", md, err)
			}
			assertNewAbsent(t, root, opts.ID)
			if gittest.Output(t, root, "rev-parse", "HEAD") != head {
				t.Fatal("failure moved HEAD")
			}
		})
	}
}

func TestNewCleanupFailureReportsBothCauses(t *testing.T) {
	root := newTestRoot(t)
	seedProject(t, root, "tele")
	// A directory survives git's removal of its own lock and prevents
	// rollback from acquiring the index. Git retries for its bounded cap.
	// Partial commits hold index.lock during hooks; move that fixture
	// lock aside before replacing it with the obstruction.
	newFailureHook(t, root, "if [ -f .git/index.lock ]; then mv .git/index.lock .git/fixture-index.lock; fi\nmkdir .git/index.lock\necho reject-locked >&2\nexit 1\n")
	md, err := New(root, "tele", Options{ID: "fix-it", Workflow: "sdlc"})
	if md != nil || err == nil {
		t.Fatalf("New = %+v, %v", md, err)
	}
	for _, want := range []string{"reject-locked", "cleanup incomplete", "remove index entries", Dir("tele", "fix-it")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "nothing recorded") {
		t.Fatalf("failed cleanup claimed success: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, Dir("tele", "fix-it"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory removal was not attempted: %v", err)
	}
	if got := gittest.Output(t, root, "ls-files", "--stage", "--", Dir("tele", "fix-it")); got == "" {
		t.Fatal("fixture did not block index cleanup")
	}
	if err := os.Remove(filepath.Join(root, ".git", "index.lock")); err != nil {
		t.Fatal(err)
	}
}
