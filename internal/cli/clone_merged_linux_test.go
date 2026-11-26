//go:build linux

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

func TestCloneGCMergedDiscardsLocalContents(t *testing.T) {
	for _, status := range []string{run.StatusMerged, run.StatusClosed} {
		for _, kind := range []string{"tracked", "untracked", "ignored", "stash", "history", "dangling"} {
			t.Run(status+"/"+kind, func(t *testing.T) {
				root, path := gcFixture(t, status, time.Now().UTC().Add(-time.Hour))
				switch kind {
				case "tracked", "stash", "history":
					gittest.WriteAndCommit(t, path, "tracked", "unique local commit\n", "local history")
					writeFile(t, filepath.Join(path, "tracked"), "uncommitted local edits\n")
					if kind == "stash" {
						gittest.Run(t, path, "stash", "push")
					}
					if kind == "history" {
						gittest.Run(t, path, "reset", "--hard", "origin/main")
					}
				case "untracked", "ignored", "dangling":
					writeFile(t, filepath.Join(path, "output"), "local output\n")
					if kind == "ignored" {
						writeFile(t, filepath.Join(path, ".git", "info", "exclude"), "output\n")
					}
					if kind == "dangling" {
						gittest.Run(t, path, "hash-object", "-w", "output")
						if err := os.Remove(filepath.Join(path, "output")); err != nil {
							t.Fatal(err)
						}
					}
				}
				head := gittest.HeadSHA(t, root)
				index, err := os.ReadFile(filepath.Join(path, ".git", "index"))
				if err != nil {
					t.Fatal(err)
				}
				for _, apply := range []bool{false, true} {
					args := []string{"tele/finish"}
					if apply {
						args = append(args, "--apply")
					}
					report, code := gcReport(t, args...)
					result := report.Candidates[0]
					want := "held"
					if status == run.StatusMerged {
						want = "eligible"
						if apply {
							want = "removed"
						}
					}
					if code != 0 || result.Outcome != want {
						t.Fatalf("apply=%t: %d %+v", apply, code, result)
					}
					if status == run.StatusMerged && result.Reason != "merged_disposable" {
						t.Fatalf("merged reason: %+v", result)
					}
					if !apply || status == run.StatusClosed {
						after, err := os.ReadFile(filepath.Join(path, ".git", "index"))
						if err != nil || string(after) != string(index) {
							t.Fatal("inspection/hold changed index", err)
						}
					} else {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatal("merged clone survives", err)
						}
						if _, err := os.Stat(result.Receipt); err != nil {
							t.Fatal("receipt lost", err)
						}
					}
				}
				if gittest.HeadSHA(t, root) != head {
					t.Fatal("GC changed journal")
				}
			})
		}
	}
}

func TestCloneGCMergedLargeFiles(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, status := range []string{run.StatusMerged, run.StatusClosed} {
			t.Run(status+map[bool]string{false: "/untracked", true: "/tracked"}[tracked], func(t *testing.T) {
				root, path := gcFixture(t, status, time.Now().UTC().Add(-time.Hour))
				if tracked {
					source := filepath.Join(root, "projects", "tele", "src")
					gittest.WriteAndCommit(t, source, "large", "small retained blob\n", "tracked seed")
					gittest.Run(t, path, "fetch", "origin")
					gittest.Run(t, path, "reset", "--hard", "origin/main")
				} else {
					writeFile(t, filepath.Join(path, "large"), "")
				}
				if err := os.Truncate(filepath.Join(path, "large"), (1<<30)+1); err != nil {
					t.Fatal(err)
				}
				if status == run.StatusClosed && tracked {
					// Exercise the separate tracked-byte bound directly; the collector's
					// whole-tree bound would otherwise refuse first.
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, err := clonereclaim.InspectPreservation(ctx, path, filepath.Join(root, "projects", "tele", "src"), nil)
					if err == nil || !strings.Contains(err.Error(), "tracked byte inventory limit exceeded") {
						t.Fatalf("tracked control: %v", err)
					}
				}
				for _, apply := range []bool{false, true} {
					result := collectClone(root, "tele", "finish", apply, true, time.Now().UTC(), io.Discard, io.Discard)
					if status == run.StatusClosed {
						if result.Outcome != "failed" || !strings.Contains(result.Detail, "clone byte inventory limit exceeded") {
							t.Fatalf("bound changed: %+v", result)
						}
						if _, err := os.Stat(path); err != nil {
							t.Fatal(err)
						}
					} else {
						want := "eligible"
						if apply {
							want = "removed"
						}
						if result.Outcome != want || result.Reason != "merged_disposable" {
							t.Fatalf("large merged: %+v", result)
						}
						if apply {
							if _, err := os.Stat(path); !os.IsNotExist(err) {
								t.Fatal("large clone survives", err)
							}
						}
					}
				}
			})
		}
	}
}

func TestCloneGCMergedStillRequiresAuthorityAndConfinement(t *testing.T) {
	for _, kind := range []string{"uncommitted", "contradictory", "session", "pending-close", "owner", "unknown-generation", "replacement", "shell", "nested-repo", "bare-repo", "fifo", "entry-limit"} {
		t.Run(kind, func(t *testing.T) {
			root, path := gcFixture(t, run.StatusMerged, time.Now().UTC().Add(-time.Hour))
			want := ""
			switch kind {
			case "uncommitted":
				commitGCState(t, root, run.StatusInProgress, time.Now().UTC().Add(-time.Minute))
				md := loadRun(t, root, "tele", "finish")
				md.Status = run.StatusMerged
				if err := run.Save(root, md); err != nil {
					t.Fatal(err)
				}
				want = "terminal_history_unproven"
			case "contradictory":
				commitGCState(t, root, run.StatusClosed, time.Now().UTC().Add(-time.Minute))
				commitGCState(t, root, run.StatusMerged, time.Now().UTC().Add(-time.Minute))
				want = "terminal_history_unproven"
			case "session":
				gittest.Run(t, root, "update-ref", "refs/heads/session/tele/finish/old/claude", "HEAD")
				want = "lifecycle_evidence_missing"
			case "pending-close":
				writeFile(t, filepath.Join(root, ".moe", "session-closes", "tele", "finish", "old.json"), "{}")
				want = "lifecycle_evidence_missing"
			case "owner":
				h, err := runowner.Acquire(root, "tele", "finish")
				if err != nil {
					t.Fatal(err)
				}
				defer h.Release()
				want = "owner_busy"
			case "unknown-generation":
				if err := os.Remove(filepath.Join(path, ".moe", "clone-identity.json")); err != nil {
					t.Fatal(err)
				}
				want = "generation_evidence_invalid"
			case "replacement":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(path, "replacement"), "survives")
				want = "generation_evidence_invalid"
			case "shell":
				h, err := runowner.Acquire(root, "tele", "finish")
				if err != nil {
					t.Fatal(err)
				}
				r, err := clonereclaim.LoadRegistry(h)
				if err != nil {
					t.Fatal(err)
				}
				id := strings.Repeat("e", 32)
				body := `{"version":1,"generation":"` + r.Identity.Generation + `","id":"` + id + `","shell":"/bin/sh"}`
				writeFile(t, filepath.Join(r.Receipt(), "shells", id, "start.json"), body)
				if err := os.Chmod(filepath.Join(r.Receipt(), "shells"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(r.Receipt(), "shells", id), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(r.Receipt(), "shells", id, "start.json"), 0600); err != nil {
					t.Fatal(err)
				}
				r.Close()
				h.Release()
				want = "cleanup_probe_failed"
			case "entry-limit":
				for i := 0; i < 8193; i++ {
					writeFile(t, filepath.Join(path, "entries", fmt.Sprint(i)), "")
				}
				want = "tree_probe_failed"
			case "nested-repo":
				writeFile(t, filepath.Join(path, "nested", ".git", "HEAD"), "ref: refs/heads/main\n")
				want = "unsafe_tree"
			case "bare-repo":
				gittest.Run(t, path, "init", "--bare", "nested.git")
				want = "unsafe_tree"
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(path, "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
				want = "unsafe_tree"
			}
			result := collectClone(root, "tele", "finish", true, true, time.Now().UTC(), io.Discard, io.Discard)
			outcome := "held"
			if kind == "entry-limit" || kind == "shell" {
				outcome = "failed"
			}
			if result.Outcome != outcome || result.Reason != want {
				t.Fatalf("guard %s: %+v", kind, result)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("guard lost tree", err)
			}
		})
	}
}

func TestCloneGCMergedPendingRemoval(t *testing.T) {
	for _, change := range []string{"none", "reopened", "new-transition"} {
		t.Run(change, func(t *testing.T) {
			root, path := gcFixture(t, run.StatusMerged, time.Now().UTC().Add(-time.Hour))
			h, err := runowner.Acquire(root, "tele", "finish")
			if err != nil {
				t.Fatal(err)
			}
			r, err := clonereclaim.LoadRegistry(h)
			if err != nil {
				h.Release()
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			evidence, err := run.LatestTerminalTransition(ctx, root, loadRun(t, root, "tele", "finish"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(path, "partial"), "before deletion")
			d, err := r.BeginDisposal(h, clonereclaim.DisposalBasis{Purpose: "terminal", Terminal: evidence}, clonereclaim.CleanupPlan{})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.AuthorizeRemoval(h, d, clonereclaim.CleanupPlan{}); err != nil {
				t.Fatal(err)
			}
			tree := filepath.Join(r.Receipt(), "disposals", d.ID, "tree")
			if err := os.Rename(path, tree); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(tree, "partial")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(tree, "new-output"), "disposable")
			writeFile(t, filepath.Join(path, "replacement"), "survives")
			r.Close()
			h.Release()
			if change != "none" {
				commitGCState(t, root, run.StatusInProgress, time.Now().UTC().Add(-time.Minute))
				if change == "new-transition" {
					commitGCState(t, root, run.StatusMerged, time.Now().UTC().Add(-time.Minute))
				}
			}
			for _, apply := range []bool{false, true} {
				args := []string{"tele/finish"}
				if apply {
					args = append(args, "--apply")
				}
				report, code := gcReport(t, args...)
				result := report.Candidates[0]
				want, reason := "held", "nonterminal"
				if change == "none" {
					want, reason = "eligible", "pending_removal"
					if apply {
						want = "removed"
					}
				}
				if change == "new-transition" {
					reason = "pending_removal_unproven"
				}
				if code != 0 || result.Outcome != want || result.Reason != reason {
					t.Fatalf("pending retry: %d %+v", code, result)
				}
				_, err := os.Stat(tree)
				if change == "none" && apply {
					if !os.IsNotExist(err) {
						t.Fatal("pending tree survives", err)
					}
				} else if err != nil {
					t.Fatal("inspection/refusal lost tree", err)
				}
				if b, err := os.ReadFile(filepath.Join(path, "replacement")); err != nil || string(b) != "survives" {
					t.Fatal("replacement lost", err)
				}
			}
		})
	}
}

func TestMergedTerminalCleanupDisposesHookOutput(t *testing.T) {
	for _, route := range []string{"direct", "reconcile"} {
		t.Run(route, func(t *testing.T) {
			var root, p, id string
			var pf *pushFixture
			var sf *reconcileFixture
			if route == "direct" {
				pf = newPushFixture(t)
				root, p, id = pf.root, pf.projectID, pf.runID
			} else {
				sf = newReconcileFixture(t, run.StatusPushed)
				root, p, id = sf.root, sf.projectID, sf.runID
			}
			t.Setenv("MOE_HOME", root)
			installCloseResourceHooks(t, root, p)
			script := filepath.Join(root, "projects", p, "hooks", devEnvTeardownDirRel, "10-resources.sh")
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, script, string(body)+"printf 'disposable hook output\\n' > \"$MOE_SANDBOX/after-hook\"\n")
			gittest.Run(t, root, "add", script)
			gittest.Commit(t, root, "teardown emits disposable output")
			resources := allocateCloseResources(t, root, p, id)
			if pf != nil {
				out, errb, code := pf.runInRoot("sdlc", "push", p+"/"+id)
				if code != 0 || !strings.Contains(out, "merged "+p+"/"+id) {
					t.Fatalf("direct merge: %d %s %s", code, out, errb)
				}
			} else {
				// Reconciliation receives merge truth from GitHub; local leftovers have
				// not necessarily been fetched into canonical source storage.
				writeFile(t, filepath.Join(resources.clone, "local-notes"), "disposable")
				fakeGh(t, map[string]string{sf.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`})
				var out, errb strings.Builder
				moved, err := reconcilePushedRuns(root, "", &out, &errb)
				if err != nil || moved != 1 || !strings.Contains(out.String(), "pushed -> merged") {
					t.Fatalf("reconcile: %d %v %s %s", moved, err, &out, &errb)
				}
			}
			resources.assertReleased(t)
			md := loadRun(t, root, p, id)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			evidence, err := run.LatestTerminalTransition(ctx, root, md, time.Now().UTC())
			if err != nil || evidence.Status != run.StatusMerged {
				t.Fatalf("missing merged journal: %+v %v", evidence, err)
			}
		})
	}
}

func TestMergedTerminalTeardownFailureCanRetryGC(t *testing.T) {
	for _, route := range []string{"direct", "reconcile"} {
		t.Run(route, func(t *testing.T) {
			var root, p, id string
			var pf *pushFixture
			var sf *reconcileFixture
			if route == "direct" {
				pf = newPushFixture(t)
				root, p, id = pf.root, pf.projectID, pf.runID
			} else {
				sf = newReconcileFixture(t, run.StatusPushed)
				root, p, id = sf.root, sf.projectID, sf.runID
			}
			t.Setenv("MOE_HOME", root)
			parent := installCloseResourceHooks(t, root, p)
			script := filepath.Join(root, "projects", p, "hooks", devEnvTeardownDirRel, "10-resources.sh")
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, script, strings.Replace(string(body), "set -eu\n", "set -eu\nif [ -f \"$TEST_CLOSE_PARENT/fail\" ]; then exit 21; fi\n", 1))
			gittest.Run(t, root, "add", script)
			gittest.Commit(t, root, "retryable teardown failure")
			resources := allocateCloseResources(t, root, p, id)
			writeFile(t, filepath.Join(parent, "fail"), "fail")
			if pf != nil {
				var log strings.Builder
				code := Run([]string{"sdlc", "push", p + "/" + id}, &log, &log)
				success := strings.Index(log.String(), "merged "+p+"/"+id+" at ")
				failure := strings.Index(log.String(), "is merged; cleanup failed")
				if code != 1 || success < 0 || failure <= success || !strings.Contains(log.String(), "teardown_failed") {
					t.Fatalf("direct merge ordering: %d %s", code, &log)
				}
				if pf.originHead() != pf.tipSHA || pendingRecordExists(pf) {
					t.Fatal("cleanup failure changed merge truth")
				}
			} else {
				fakeGh(t, map[string]string{sf.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`})
				var out, errb strings.Builder
				moved, err := reconcilePushedRuns(root, "", &out, &errb)
				if moved != 1 || err == nil || !strings.Contains(out.String(), "pushed -> merged") || !strings.Contains(err.Error(), "is merged; cleanup failed") || !strings.Contains(err.Error(), "teardown_failed") {
					t.Fatalf("reconcile failure: %d %v %s %s", moved, err, &out, &errb)
				}
			}
			resources.assertRetained(t)
			md := loadRun(t, root, p, id)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := run.LatestTerminalTransition(ctx, root, md, time.Now().UTC()); err != nil || md.Status != run.StatusMerged {
				t.Fatal("failed teardown lost journal truth", err)
			}
			if err := os.Remove(filepath.Join(parent, "fail")); err != nil {
				t.Fatal(err)
			}
			report, code := gcReport(t, "--apply", p+"/"+id)
			if code != 0 || report.Candidates[0].Outcome != "removed" || report.Candidates[0].Reason != "merged_disposable" {
				t.Fatalf("GC retry: %d %+v", code, report)
			}
			resources.assertReleased(t)
		})
	}
}
