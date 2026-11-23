package clonereclaim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func preservationFixture(t *testing.T) (string, string, context.Context) {
	t.Helper()
	source := gittest.Init(t)
	gittest.WriteAndCommit(t, source, "tracked", "original\n", "seed")
	clone := filepath.Join(t.TempDir(), "clone")
	gittest.Run(t, "", "clone", "--shared", source, clone)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return source, clone, ctx
}

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPreservationRetainedClone(t *testing.T) {
	source, clone, ctx := preservationFixture(t)
	got, err := InspectPreservation(ctx, clone, source, nil)
	if err != nil || got.Reason != "" || got.Objects != 3 {
		t.Fatalf("clean retained clone: %+v %v", got, err)
	}
	// Shared objects not reachable from canonical refs deliberately delay
	// disposal even when this particular clone never referred to them.
	put(t, filepath.Join(source, "scratch"), "unreachable in canonical")
	gittest.Run(t, source, "hash-object", "-w", "scratch")
	got, err = InspectPreservation(ctx, clone, source, nil)
	if err != nil || got.Reason != "unique_git_objects" {
		t.Fatalf("unreachable shared object: %+v %v", got, err)
	}
}

func TestPreservationRawBlobFormats(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			gittest.SetupEnv(t)
			source := t.TempDir()
			gittest.Run(t, source, "init", "--object-format="+format)
			put(t, filepath.Join(source, "tracked"), "retained\n")
			if err := os.Symlink("tracked", filepath.Join(source, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(source, "tracked"), 0o755); err != nil {
				t.Fatal(err)
			}
			gittest.Commit(t, source, "executable and symlink")
			clone := filepath.Join(t.TempDir(), "clone")
			gittest.Run(t, "", "clone", "--shared", source, clone)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := InspectPreservation(ctx, clone, source, nil)
			if err != nil || got.Reason != "" {
				t.Fatalf("retained raw blobs: %+v %v", got, err)
			}
			if err := os.Remove(filepath.Join(clone, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("unique target", filepath.Join(clone, "link")); err != nil {
				t.Fatal(err)
			}
			got, err = InspectPreservation(ctx, clone, source, nil)
			if err != nil || got.Reason != "local_modifications" {
				t.Fatalf("changed raw link: %+v %v", got, err)
			}
		})
	}
}

func TestPreservationLocalWork(t *testing.T) {
	for _, kind := range []string{"dirty", "staged", "untracked", "ignored", "stash", "detached", "ref", "reflog", "dangling", "operation"} {
		t.Run(kind, func(t *testing.T) {
			source, clone, ctx := preservationFixture(t)
			want := "unique_git_objects"
			switch kind {
			case "dirty", "staged", "stash":
				put(t, filepath.Join(clone, "tracked"), "experiment")
				want = "local_modifications"
				if kind == "staged" {
					gittest.Run(t, clone, "add", "tracked")
				}
				if kind == "stash" {
					gittest.Run(t, clone, "stash", "push")
					want = "stash"
				}
			case "untracked", "ignored":
				put(t, filepath.Join(clone, "scratch"), "experiment")
				if kind == "ignored" {
					put(t, filepath.Join(clone, ".git", "info", "exclude"), "scratch\n")
				}
				want = "local_output"
			case "detached", "ref", "reflog":
				if kind == "detached" {
					gittest.Run(t, clone, "checkout", "--detach")
				}
				put(t, filepath.Join(clone, "tracked"), "unique commit")
				sha := gittest.Commit(t, clone, "experiment")
				if kind == "ref" {
					gittest.Run(t, clone, "tag", "experiment", sha)
				}
				if kind != "detached" {
					gittest.Run(t, clone, "reset", "--hard", "origin/main")
				}
			case "dangling":
				put(t, filepath.Join(clone, "scratch"), "unique unreachable blob")
				gittest.Run(t, clone, "hash-object", "-w", "scratch")
				if err := os.Remove(filepath.Join(clone, "scratch")); err != nil {
					t.Fatal(err)
				}
			case "operation":
				put(t, filepath.Join(clone, ".git", "MERGE_HEAD"), gittest.HeadSHA(t, clone))
				want = "unfinished_git_operation"
			}
			got, err := InspectPreservation(ctx, clone, source, nil)
			if err != nil || got.Reason != want {
				t.Fatalf("%s: %+v %v, want %s", kind, got, err, want)
			}
		})
	}
}

func TestPreservationArtifactsNeedExactOwnership(t *testing.T) {
	source, clone, ctx := preservationFixture(t)
	put(t, filepath.Join(clone, ".git", "info", "exclude"), ".moe/\n")
	put(t, filepath.Join(clone, ".moe", "identity.json"), "owned identity")
	sum := sha256.Sum256([]byte("owned identity"))
	artifacts := map[string]string{".moe/identity.json": hex.EncodeToString(sum[:])}
	got, err := InspectPreservation(ctx, clone, source, artifacts)
	if err != nil || got.Reason != "" {
		t.Fatalf("owned artifact: %+v %v", got, err)
	}
	put(t, filepath.Join(clone, ".moe", "identity.json"), "substituted")
	got, err = InspectPreservation(ctx, clone, source, artifacts)
	if err != nil || got.Reason != "artifact_mismatch" {
		t.Fatalf("substituted artifact: %+v %v", got, err)
	}
	put(t, filepath.Join(clone, ".moe", "identity.json"), "owned identity")
	put(t, filepath.Join(clone, ".moe", "experiment"), "unowned ignored output")
	got, err = InspectPreservation(ctx, clone, source, artifacts)
	if err != nil || got.Reason != "local_output" {
		t.Fatalf("blanket .moe exemption: %+v %v", got, err)
	}
}

func TestPreservationRejectsIncompleteOrDependentStorage(t *testing.T) {
	for _, kind := range []string{"shallow", "promisor", "replacement", "graft", "source-dependent", "source-symlink", "missing-object"} {
		t.Run(kind, func(t *testing.T) {
			source, clone, ctx := preservationFixture(t)
			sha := gittest.HeadSHA(t, clone)
			switch kind {
			case "shallow":
				put(t, filepath.Join(clone, ".git", "shallow"), sha+"\n")
			case "promisor":
				gittest.Run(t, clone, "config", "remote.origin.promisor", "true")
			case "replacement":
				gittest.Run(t, clone, "update-ref", "refs/replace/"+sha, sha)
			case "graft":
				put(t, filepath.Join(clone, ".git", "info", "grafts"), sha+"\n")
			case "source-dependent":
				put(t, filepath.Join(source, ".git", "objects", "info", "alternates"), filepath.Join(clone, ".git", "objects")+"\n")
			case "source-symlink":
				if err := os.Symlink(filepath.Join(clone, ".git", "objects"), filepath.Join(source, ".git", "objects", "foreign")); err != nil {
					t.Fatal(err)
				}
			case "missing-object":
				if err := os.Remove(filepath.Join(source, ".git", "objects", sha[:2], sha[2:])); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := InspectPreservation(ctx, clone, source, nil); err == nil {
				t.Fatalf("accepted %s: %+v", kind, got)
			}
		})
	}
}
