package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// holdOriginMainPush installs a pre-receive hook on the fixture's bare
// origin that blocks a push to refs/heads/main until release exists,
// touching entered once it is holding. Pushes to other refs (the run
// branch push before mergePath, the remote-branch delete after) pass
// straight through — keying on main is what keeps the test from
// deadlocking before the code under test runs. The hook gives up after
// a minute so a broken test can't wedge the suite.
func holdOriginMainPush(t *testing.T, f *pushFixture) (entered, release string) {
	t.Helper()
	dir := t.TempDir()
	entered = filepath.Join(dir, "entered")
	release = filepath.Join(dir, "release")
	hookPath := filepath.Join(f.origin, "hooks", "pre-receive")
	writeFile(t, hookPath, fmt.Sprintf(`#!/bin/sh
hold=
while read old new ref; do
  [ "$ref" = "refs/heads/main" ] && hold=1
done
[ -z "$hold" ] && exit 0
touch %q
i=0
while [ ! -f %q ]; do
  i=$((i+1))
  [ $i -gt 600 ] && exit 1
  sleep 0.1
done
exit 0
`, entered, release))
	if err := os.Chmod(hookPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return entered, release
}

// assertRootCleanDuringHeldMerge runs the merge push with origin's
// ff-push held open, and — while it is held — checks the bureaucracy
// root is clean and that a concurrent run open, taking the repo lock
// the way a sweep does, succeeds. This is the reproduction of the
// 2026-11-16 failure: a lightpool ship mid-merge failing the westworld
// and claudia sweeps' run.New on `working tree has uncommitted changes`.
func assertRootCleanDuringHeldMerge(t *testing.T, f *pushFixture) {
	t.Helper()
	entered, release := holdOriginMainPush(t, f)

	t.Setenv("MOE_HOME", f.root)
	t.Setenv("NO_COLOR", "1")
	type result struct {
		code           int
		stdout, stderr string
	}
	done := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"sdlc", "push", f.projectID + "/" + f.runID}, &stdout, &stderr)
		done <- result{code, stdout.String(), stderr.String()}
	}()
	finish := func() result {
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Error(err)
		}
		return <-done
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("push finished before the ff-push was held: exit=%d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
		default:
		}
		if time.Now().After(deadline) {
			r := finish()
			t.Fatalf("ff-push never reached origin's hook: exit=%d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	dirty, err := run.WorkingTreeDirty(f.root)
	if err != nil {
		finish()
		t.Fatal(err)
	}
	var status string
	if dirty {
		status = gittest.Output(t, f.root, "status", "--porcelain")
	}
	openErr := repolock.With(f.root, repolock.Options{
		Purpose: "run-new",
		Budget:  10 * time.Second,
	}, func() error {
		_, err := run.New(f.root, f.projectID, run.Options{ID: "sweep-open", Workflow: "sdlc"})
		return err
	})

	r := finish()
	if dirty {
		t.Errorf("bureaucracy root dirty while the ff-push was in flight:\n%s", status)
	}
	if openErr != nil {
		t.Errorf("run.New during the held ff-push: %v", openErr)
	}
	if r.code != 0 {
		t.Fatalf("push exit=%d\nstdout=%s\nstderr=%s", r.code, r.stdout, r.stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusMerged {
		t.Fatalf("status: want %s, got %s", run.StatusMerged, md.Status)
	}
	if d, err := run.WorkingTreeDirty(f.root); err != nil || d {
		t.Fatalf("root dirty after merge (err=%v):\n%s", err, gittest.Output(t, f.root, "status", "--porcelain"))
	}
}

// TestPushMergeLeavesRootCleanDuringFFPush: no captures, so the only
// tracked write at stake is run.json's flip to merged.
func TestPushMergeLeavesRootCleanDuringFFPush(t *testing.T) {
	f := newPushFixture(t)
	assertRootCleanDuringHeldMerge(t, f)
}

// TestPushMergeCommitsHarvestBeforeFFPush: a run with a committed,
// unchecked follow-up. The harvest's `[x]` rewrite is tracked dirt, so
// the root only stays clean if the push-harvest window commits it —
// the deferred flip alone doesn't cover this case. Afterwards the
// rewrite sits on a `harvest:` commit and the merge record no longer
// carries followups.md.
func TestPushMergeCommitsHarvestBeforeFFPush(t *testing.T) {
	f := newPushFixture(t)
	writeFollowups(t, f.root, f.projectID, f.runID,
		"- [ ] `chase-it` — Chase the thing\n")
	followupsRel := run.FollowupsPath(f.projectID, f.runID)
	gittest.Run(t, f.root, "add", followupsRel)
	gittest.Run(t, f.root, "commit", "-m", "work: file a followup")

	assertRootCleanDuringHeldMerge(t, f)

	harvest := gittest.Output(t, f.root, "log", "--format=%H",
		"--grep=^harvest: capture follow-ups, lore, and twin observations for "+f.projectID+"/"+f.runID)
	if harvest == "" {
		t.Fatalf("no harvest: commit in the journal:\n%s", gittest.Output(t, f.root, "log", "--format=%s"))
	}
	if strings.Contains(harvest, "\n") {
		t.Fatalf("want one harvest: commit, got:\n%s", harvest)
	}
	files := gittest.Output(t, f.root, "show", "--name-only", "--format=", harvest)
	if !strings.Contains(files, followupsRel) {
		t.Fatalf("harvest commit does not carry %s:\n%s", followupsRel, files)
	}
	show := gittest.Output(t, f.root, "show", harvest, "--", followupsRel)
	if !strings.Contains(show, "+- [x] `chase-it`") {
		t.Fatalf("harvest commit does not carry the [x] rewrite:\n%s", show)
	}
	merge := gittest.Output(t, f.root, "log", "--format=%H", "-1",
		"--grep=^push: "+f.projectID+"/"+f.runID+" merged")
	mergeFiles := gittest.Output(t, f.root, "show", "--name-only", "--format=", merge)
	if strings.Contains(mergeFiles, followupsRel) {
		t.Fatalf("merge record still touches %s:\n%s", followupsRel, mergeFiles)
	}
	if !strings.Contains(mergeFiles, filepath.Join(run.Dir(f.projectID, f.runID), "run.json")) {
		t.Fatalf("merge record missing run.json:\n%s", mergeFiles)
	}
}

// TestPushNoteFailureAfterFFPushLeavesPendingRecord: the push note is
// now written inside the push-merge window, after the flip. A write
// that fails there — past the point of no return — takes the same
// pending-marker route as a failed record commit, so the re-run
// finishes the record instead of stranding run.json as merged with no
// marker to resume from.
func TestPushNoteFailureAfterFFPushLeavesPendingRecord(t *testing.T) {
	f := newPushFixture(t)
	// A file where the push canvas's directory belongs makes the note's
	// MkdirAll fail. Untracked, so it trips no dirty-tree gate.
	blocker := filepath.Join(f.root, filepath.Dir(run.ContentPath(f.projectID, f.runID, "push")))
	writeFile(t, blocker, "in the way\n")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero exit when the push note can't be written\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if got := f.originHead(); got != f.tipSHA {
		t.Fatalf("origin/main: want %s, got %s", f.tipSHA, got)
	}
	if md := f.reloadRun(); md.Status != run.StatusMerged {
		t.Fatalf("status on disk: want %s, got %s", run.StatusMerged, md.Status)
	}
	p := readPendingRecord(t, f)
	if want := []string{filepath.Join(run.Dir(f.projectID, f.runID), "run.json")}; !slices.Equal(p.Paths, want) {
		t.Fatalf("pending paths: want %v, got %v", want, p.Paths)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if sha := push.MergedSHA(f.root, f.projectID, f.runID); sha != f.tipSHA {
		t.Fatalf("MoE-Merged after resume: want %s, got %q", f.tipSHA, sha)
	}
	if pendingRecordExists(f) {
		t.Fatalf("marker still on disk after resume")
	}
	if dirty, err := run.WorkingTreeDirty(f.root); err != nil || dirty {
		t.Fatalf("root dirty after resume (err=%v):\n%s", err, gittest.Output(t, f.root, "status", "--porcelain"))
	}
}
