package run

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestJournalIndexUnborn(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "fresh"
		if orphan {
			name = "orphan"
		}
		t.Run(name, func(t *testing.T) {
			root := gittest.Init(t)
			if orphan {
				gittest.Commit(t, root, "Open run x/other\n\nMoE-Project: x\nMoE-Run: other\n")
				if _, err := BuildJournalIndex(root); err != nil {
					t.Fatal(err)
				}
				gittest.Run(t, root, "checkout", "--orphan", "empty")
			}
			idx, err := BuildJournalIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			if idx == nil || !reflect.DeepEqual(idx, newJournalWalk().finish()) {
				t.Fatalf("unborn history = %#v, want normal empty index", idx)
			}
			again, err := BuildJournalIndex(root)
			if err != nil || again == idx {
				t.Fatalf("empty history must stay uncached: idx=%p again=%p err=%v", idx, again, err)
			}
			gittest.Commit(t, root, "Open run x/first\n\nMoE-Project: x\nMoE-Run: first\n")
			after, err := BuildJournalIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.LastActivity["x/first"].IsZero() || !after.LastActivity["x/other"].IsZero() {
				t.Fatalf("first commit must replace empty history without other refs: %#v", after.LastActivity)
			}
		})
	}
}

func TestJournalIndexBrokenHistory(t *testing.T) {
	for _, failure := range []string{"missing-parent", "malformed-head-after-cache"} {
		t.Run(failure, func(t *testing.T) {
			root := gittest.Init(t)
			parent := gittest.Commit(t, root, "parent")
			if failure == "missing-parent" {
				gittest.Commit(t, root, "child")
				if err := os.Remove(filepath.Join(root, ".git", "objects", parent[:2], parent[2:])); err != nil {
					t.Fatal(err)
				}
				if _, err := git.HEAD(root); err != nil {
					t.Fatalf("fixture must have readable HEAD: %v", err)
				}
			} else {
				if _, err := BuildJournalIndex(root); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("broken\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := BuildJournalIndex(root); err == nil {
				t.Fatal("broken history must return an error")
			}
		})
	}
}
