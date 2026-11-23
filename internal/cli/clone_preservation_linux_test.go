//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

func TestCloneGCInspectsRawTrackedBytesWithoutFilters(t *testing.T) {
	for _, kind := range []string{"clean", "dirty", "clean-filter", "process-filter", "clean-with-filter", "crlf", "ident"} {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/apply=%t", kind, apply), func(t *testing.T) {
				at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
				root, path := gcFixture(t, run.StatusClosed, at)
				source := filepath.Join(root, "projects", "tele", "src")
				canonical := "canonical bytes\n"
				attribute := "tracked filter=strip-local\n"
				if kind == "crlf" {
					attribute = "tracked text\n"
				} else if kind == "ident" {
					canonical = "$Id$\n"
					attribute = "tracked ident\n"
				}
				gittest.WriteAndCommit(t, source, "tracked", canonical, "tracked file")
				gittest.WriteAndCommit(t, source, ".gitattributes", attribute, "content attributes")
				gittest.Run(t, path, "fetch", "origin")
				gittest.Run(t, path, "reset", "--hard", "origin/main")
				marker := filepath.Join(root, "filter-executed")
				want := canonical
				if strings.Contains(kind, "filter") {
					gittest.Run(t, path, "config", "filter.strip-local.clean", "echo invoked >> '"+marker+"'; sed '/^LOCAL:/d'")
					// Establish a filtered index/stat cache, then change only same-size
					// local notes. These bytes never become a Git object.
					writeFile(t, filepath.Join(path, "tracked"), canonical+"LOCAL: stored experiment notes\n")
					gittest.Run(t, path, "add", "tracked")
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
					want += "LOCAL: unique experiment notes\n"
					if kind == "process-filter" {
						gittest.Run(t, path, "config", "filter.strip-local.process", "echo invoked >> '"+marker+"'; exit 1")
					}
				}
				if kind == "clean-with-filter" {
					want = canonical
				} else if kind == "dirty" {
					want = "unique experiment bytes\n"
				} else if kind == "crlf" {
					want = "canonical bytes\r\n"
				} else if kind == "ident" {
					want = "$Id: unique experiment notes $\n"
				}
				writeFile(t, filepath.Join(path, "tracked"), want)
				if kind == "crlf" || kind == "ident" {
					// Cache the converted content as clean without storing the raw
					// worktree bytes. A stat-cache hit is still not preservation.
					gittest.Run(t, path, "add", "tracked")
				}
				indexBefore, err := os.ReadFile(filepath.Join(path, ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				result := collectClone(root, "tele", "finish", apply, true, at.Add(time.Hour), io.Discard, io.Discard)
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("collector executed a content filter: %v", err)
				}
				clean := kind == "clean" || kind == "clean-with-filter"
				if clean {
					outcome := "eligible"
					if apply {
						outcome = "removed"
					}
					if result.Outcome != outcome || result.Reason != "independently_preserved" {
						t.Fatalf("clean control: %+v", result)
					}
					if apply {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("clean clone not deleted: %v", err)
						}
						return
					}
				} else if result.Outcome != "held" || result.Reason != "local_work_retained" {
					t.Errorf("unsafe collection: %+v", result)
				}
				got, err := os.ReadFile(filepath.Join(path, "tracked"))
				if err != nil || string(got) != want {
					t.Errorf("lost local bytes: %q %v", got, err)
				}
				indexAfter, err := os.ReadFile(filepath.Join(path, ".git", "index"))
				if err != nil || !bytes.Equal(indexBefore, indexAfter) {
					t.Errorf("collector changed index: %v", err)
				}
			})
		}
	}
}

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
