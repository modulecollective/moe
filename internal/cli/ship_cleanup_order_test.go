package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
)

func TestPushCleanupFailureReportsCommittedMergeAndRetainsCache(t *testing.T) {
	f := newPushFixture(t)
	installCloseResourceHooks(t, f.root, f.projectID)
	resources := allocateCloseResources(t, f.root, f.projectID, f.runID)
	script := filepath.Join(f.root, "projects", f.projectID, "hooks", devEnvTeardownDirRel, "10-resources.sh")
	writeFile(t, script, "#!/bin/sh\nexit 21\n")
	gittest.Run(t, f.root, "add", script)
	gittest.Run(t, f.root, "commit", "-m", "test: fail post-merge cleanup")
	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 1 || !strings.Contains(stdout, "merged "+f.projectID+"/"+f.runID) || !strings.Contains(stderr, "is merged; cleanup failed") {
		t.Fatalf("wrong outcome: exit=%d\n%s\n%s", code, stdout, stderr)
	}
	if got := f.originHead(); got != f.tipSHA {
		t.Fatal("merge did not land")
	}
	if got := push.MergedSHA(f.root, f.projectID, f.runID); got != f.tipSHA {
		t.Fatalf("merge record missing: %s", got)
	}
	if pendingRecordExists(f) {
		t.Fatal("cleanup failure created ship debt")
	}
	resources.assertRetained(t)
}

func TestSyncCommitFailureKeepsCloneAndEnvironment(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	installCloseResourceHooks(t, f.root, f.projectID)
	resources := allocateCloseResources(t, f.root, f.projectID, f.runID)
	fakeGh(t, map[string]string{f.prURL: `{"state":"CLOSED","mergeCommit":null}`})
	allow := failCommits(t, f.root)
	defer allow()
	var out, errb bytes.Buffer
	moved, err := reconcilePushedRuns(f.root, "", &out, &errb)
	if err == nil || moved != 0 {
		t.Fatalf("failed commit outcome: moved=%d err=%v", moved, err)
	}
	if got := f.reload().Status; got != run.StatusPushed {
		t.Fatalf("status=%s", got)
	}
	resources.assertRetained(t)
}

func TestSyncCleanupFailureStillCountsAndReportsTerminalTransition(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	installCloseResourceHooks(t, f.root, f.projectID)
	resources := allocateCloseResources(t, f.root, f.projectID, f.runID)
	script := filepath.Join(f.root, "projects", f.projectID, "hooks", devEnvTeardownDirRel, "10-resources.sh")
	writeFile(t, script, "#!/bin/sh\nexit 21\n")
	gittest.Run(t, f.root, "add", script)
	gittest.Run(t, f.root, "commit", "-m", "test: fail post-reconcile cleanup")
	fakeGh(t, map[string]string{f.prURL: `{"state":"CLOSED","mergeCommit":null}`})
	var out, errb bytes.Buffer
	moved, err := reconcilePushedRuns(f.root, "", &out, &errb)
	if moved != 1 || err == nil || !strings.Contains(out.String(), "pushed -> closed") || !strings.Contains(err.Error(), "is closed; cleanup failed") {
		t.Fatalf("terminal outcome: moved=%d err=%v out=%q", moved, err, &out)
	}
	if got := f.reload().Status; got != run.StatusClosed {
		t.Fatalf("terminal truth lost: %s", got)
	}
	resources.assertRetained(t)
}

func TestPushAndSyncCleanupHooksRunOutsideRepositoryLock(t *testing.T) {
	for _, kind := range []string{"push", "sync"} {
		t.Run(kind, func(t *testing.T) {
			var root, projectID, runID string
			var pf *pushFixture
			var sf *reconcileFixture
			if kind == "push" {
				pf = newPushFixture(t)
				root, projectID, runID = pf.root, pf.projectID, pf.runID
			} else {
				sf = newReconcileFixture(t, run.StatusPushed)
				root, projectID, runID = sf.root, sf.projectID, sf.runID
			}
			installCloseResourceHooks(t, root, projectID)
			script := filepath.Join(root, "projects", projectID, "hooks", devEnvTeardownDirRel, "10-resources.sh")
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, script, strings.Replace(string(body), "#!/bin/sh\n", "#!/bin/sh\nset -eu\ntest ! -e \"$MOE_BUREAUCRACY/.moe/lock\"\n", 1))
			gittest.Run(t, root, "add", script)
			gittest.Run(t, root, "commit", "-m", "test: assert cleanup releases repository lock")
			resources := allocateCloseResources(t, root, projectID, runID)
			if pf != nil {
				if out, errb, code := pf.runInRoot("sdlc", "push", projectID+"/"+runID); code != 0 {
					t.Fatalf("push exit=%d %s %s", code, out, errb)
				}
			} else {
				fakeGh(t, map[string]string{sf.prURL: `{"state":"CLOSED","mergeCommit":null}`})
				if _, err := reconcilePushedRuns(root, "", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
					t.Fatal(err)
				}
			}
			resources.assertReleased(t)
		})
	}
}
