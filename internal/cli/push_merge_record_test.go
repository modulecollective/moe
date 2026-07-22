package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/push"
	"github.com/modulecollective/moe/internal/run"
)

// failMergeRecordCommits points root's core.hooksPath at a commit-msg
// hook that refuses only the merge record commit (`push: <p>/<r>
// merged`). Scoped like failCloseCommits rather than blanket: the
// harvest's own commits and the auto-bump still work, so the push dies
// on exactly the commit this resume path exists for. Returns a func
// that lifts the hook so the same fixture can retry.
func failMergeRecordCommits(t *testing.T, root string) func() {
	t.Helper()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	script := "#!/bin/sh\ngrep -q '^push: .* merged' \"$1\" && exit 1\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	return func() {
		t.Helper()
		gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	}
}

// readPendingRecord loads the untracked merge-record marker, failing
// the test when it isn't there.
func readPendingRecord(t *testing.T, f *pushFixture) pendingMergeRecord {
	t.Helper()
	path := filepath.Join(f.root, run.Dir(f.projectID, f.runID), mergeRecordPendingName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", mergeRecordPendingName, err)
	}
	var p pendingMergeRecord
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("parse %s: %v\n%s", mergeRecordPendingName, err, data)
	}
	return p
}

// pendingRecordExists reports whether the marker is on disk.
func pendingRecordExists(f *pushFixture) bool {
	_, err := os.Stat(filepath.Join(f.root, run.Dir(f.projectID, f.runID), mergeRecordPendingName))
	return err == nil
}

// TestPushMergeRecordFailurePersistsPendingRecord: when the record
// commit fails after the ff-push has landed, the flip to merged is
// true and stays — what gets written is the marker that makes the
// commit recoverable. The record can't be rebuilt later (the ff-push
// collapses the chore diff and the clone the tip came from is already
// released), so the trailers have to survive in the marker itself.
func TestPushMergeRecordFailurePersistsPendingRecord(t *testing.T) {
	f := newPushFixture(t)
	defer failMergeRecordCommits(t, f.root)()

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("expected non-zero exit when the record commit fails\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	// The merge itself is durable — that's why there's nothing to roll back.
	if got := f.originHead(); got != f.tipSHA {
		t.Fatalf("origin/main: want %s, got %s", f.tipSHA, got)
	}
	if md := f.reloadRun(); md.Status != run.StatusMerged {
		t.Fatalf("status on disk: want %s, got %s", run.StatusMerged, md.Status)
	}
	// Nothing committed, so nothing to read the merge SHA back from.
	if sha := push.MergedSHA(f.root, f.projectID, f.runID); sha != "" {
		t.Fatalf("record commit failed but MoE-Merged resolves to %s", sha)
	}

	p := readPendingRecord(t, f)
	if !strings.Contains(p.Msg, "MoE-Merged: "+f.tipSHA) {
		t.Fatalf("pending msg missing MoE-Merged trailer:\n%s", p.Msg)
	}
	if p.Tip != f.tipSHA {
		t.Fatalf("pending tip: want %s, got %s", f.tipSHA, p.Tip)
	}
	runJSON := filepath.Join(run.Dir(f.projectID, f.runID), "run.json")
	if !slices.Contains(p.Paths, runJSON) {
		t.Fatalf("pending paths missing %s: %v", runJSON, p.Paths)
	}
	if !slices.Contains(p.Paths, run.ContentPath(f.projectID, f.runID, "push")) {
		t.Fatalf("pending paths missing the push canvas: %v", p.Paths)
	}

	if !strings.Contains(stderr, "commit merge record:") {
		t.Fatalf("stderr should name the failing step, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "re-run `moe sdlc push "+f.projectID+"/"+f.runID+"`") {
		t.Fatalf("stderr should name the exact re-run, got:\n%s", stderr)
	}
}

// TestPushResumesStrandedMergeRecord: the re-run the failure message
// names actually finishes the job — it commits the persisted record
// (trailers verbatim, including the consent stamp from the walk that
// shipped it) instead of printing the "already merged" lie, and clears
// the marker.
func TestPushResumesStrandedMergeRecord(t *testing.T) {
	f := newPushFixture(t)
	lift := failMergeRecordCommits(t, f.root)

	// Strand under a machine walk so the record carries MoE-Consent...
	strand := func() {
		defer withRideMode(rideStatic)()
		if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code == 0 {
			t.Fatalf("expected the stranding push to fail; stderr=%s", stderr)
		}
	}
	strand()
	lift()

	// ...and resume outside one. The consent below can only have come
	// from the persisted message, not from this invocation.
	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stdout, "already merged") {
		t.Fatalf("resume printed the already-merged lie:\n%s", stdout)
	}
	if want := "merged " + f.projectID + "/" + f.runID + " at " + git.ShortSHA(f.tipSHA); !strings.Contains(stdout, want) {
		t.Fatalf("resume stdout missing %q:\n%s", want, stdout)
	}
	if pendingRecordExists(f) {
		t.Fatalf("%s should be gone after a successful resume", mergeRecordPendingName)
	}
	if sha := push.MergedSHA(f.root, f.projectID, f.runID); sha != f.tipSHA {
		t.Fatalf("MoE-Merged after resume: want %s, got %q", f.tipSHA, sha)
	}
	log := gittest.Output(t, f.root, "log", "-3", "--format=%B")
	if !strings.Contains(log, "push: "+f.projectID+"/"+f.runID+" merged") {
		t.Fatalf("merge record commit missing from log:\n%s", log)
	}
	if !strings.Contains(log, "MoE-Consent") {
		t.Fatalf("resume dropped the consent stamp the shipping walk earned:\n%s", log)
	}
	// The bump the stranded closure never reached runs on the resume.
	gitlink := gittest.Output(t, f.root, "ls-tree", "HEAD", "projects/"+f.projectID+"/src")
	if !strings.Contains(gitlink, f.tipSHA) {
		t.Fatalf("gitlink at HEAD should record tip SHA %s, got:\n%s", f.tipSHA, gitlink)
	}
}

// TestPushResumeAcceptsHandCommittedRecord: an operator who commits the
// staged record themselves has done the resume's work. StageAndCommit
// reports that as ErrNothingToCommit, which is success here — the
// resume clears the marker and exits 0 rather than failing on an empty
// commit.
func TestPushResumeAcceptsHandCommittedRecord(t *testing.T) {
	f := newPushFixture(t)
	lift := failMergeRecordCommits(t, f.root)
	if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code == 0 {
		t.Fatalf("expected the stranding push to fail; stderr=%s", stderr)
	}
	lift()

	// The failed commit leaves its pathspecs staged; commit them by hand.
	gittest.Run(t, f.root, "commit", "-m",
		"record the merge by hand\n\nMoE-Run: "+f.runID+"\nMoE-Merged: "+f.tipSHA+"\n")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume after hand commit: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if pendingRecordExists(f) {
		t.Fatalf("%s should be gone once the record is committed by any hand", mergeRecordPendingName)
	}
	if !strings.Contains(stdout, "merged "+f.projectID+"/"+f.runID+" at ") {
		t.Fatalf("resume stdout:\n%s", stdout)
	}
}

// TestPushResumeTailsPulseForStrandedShip: a ship that stranded never
// swept, so the resume is where the one sweep per merged ship lands.
// The stranding attempt must not fire — the merge isn't recorded yet.
func TestPushResumeTailsPulseForStrandedShip(t *testing.T) {
	f := newPushFixture(t)
	defer withRideMode(rideStatic)()
	fired := stubFirePulse(t)

	lift := failMergeRecordCommits(t, f.root)
	if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code == 0 {
		t.Fatalf("expected the stranding push to fail; stderr=%s", stderr)
	}
	if len(*fired) != 0 {
		t.Fatalf("stranded ship swept before its record landed: %v", *fired)
	}
	lift()

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	want := f.projectID + " " + f.runID
	if len(*fired) != 1 || (*fired)[0] != want {
		t.Fatalf("firePulse fired %v, want exactly one fire %q", *fired, want)
	}
}
