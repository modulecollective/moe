//go:build linux

package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func TestCloneGCRefusesHiddenTrackedEdits(t *testing.T) {
	for _, kind := range []string{"clean", "dirty", "assume-unchanged", "skip-worktree", "core-worktree", "expired-core-worktree", "common-dir"} {
		t.Run(kind, func(t *testing.T) {
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			root, path := gcFixture(t, run.StatusClosed, at)
			source := filepath.Join(root, "projects", "tele", "src")
			gittest.WriteAndCommit(t, source, "tracked", "canonical bytes\n", "tracked file")
			gittest.Run(t, path, "fetch", "origin")
			gittest.Run(t, path, "reset", "--hard", "origin/main")
			switch kind {
			case "assume-unchanged", "skip-worktree":
				gittest.Run(t, path, "update-index", "--"+kind, "tracked")
			case "core-worktree", "expired-core-worktree":
				gittest.Run(t, path, "config", "core.worktree", source)
			case "common-dir":
				writeFile(t, filepath.Join(path, ".git", "commondir"), filepath.Join(source, ".git")+"\n")
			}
			if kind != "clean" {
				writeFile(t, filepath.Join(path, "tracked"), "unique uncommitted "+kind+"\n")
			}
			indexBefore, err := os.ReadFile(filepath.Join(path, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			now := at.Add(time.Hour)
			if kind == "expired-core-worktree" {
				now = at.Add(run.CloneRetention)
			}
			result := collectClone(root, "tele", "finish", true, true, now, io.Discard, io.Discard)
			if kind == "clean" {
				if result.Outcome != "removed" || result.Reason != "independently_preserved" {
					t.Fatalf("clean positive: %+v", result)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("clean tree not deleted: %v", err)
				}
			} else {
				if result.Outcome != "held" {
					t.Fatalf("unsafe apply: %+v", result)
				}
				if kind == "dirty" || kind == "assume-unchanged" || kind == "skip-worktree" {
					if result.Reason != "local_work_retained" {
						t.Fatalf("preservation hold: %+v", result)
					}
				} else if result.Reason != "generation_evidence_invalid" {
					t.Fatalf("layout hold: %+v", result)
				}
				got, err := os.ReadFile(filepath.Join(path, "tracked"))
				if err != nil || string(got) != "unique uncommitted "+kind+"\n" {
					t.Fatalf("lost candidate bytes: %q %v", got, err)
				}
				indexAfter, err := os.ReadFile(filepath.Join(path, ".git", "index"))
				if err != nil || !bytes.Equal(indexBefore, indexAfter) {
					t.Fatalf("inspection changed index: %v", err)
				}
			}
			got, err := os.ReadFile(filepath.Join(source, "tracked"))
			if err != nil || string(got) != "canonical bytes\n" {
				t.Fatalf("outside worktree changed: %q %v", got, err)
			}
		})
	}
}
