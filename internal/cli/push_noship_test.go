package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// noShipGate is a filled, ready test canvas declaring that the run
// deliberately ships no project-repo change.
const noShipGate = `# Test

## Gate

` + "```json" + `
{"status":"ready","ship":"none"}
` + "```" + `

## What was verified

the knowledge topic renders; ` + "`go test ./...`" + ` is green

## What wasn't verified

nothing — the run's whole deliverable is bureaucracy commits
`

// plainReadyGate is the same canvas without the declaration — today's
// behaviour, whatever the branch looks like.
var plainReadyGate = strings.Replace(noShipGate, `{"status":"ready","ship":"none"}`, `{"status":"ready"}`, 1)

// writeTestGate commits a test canvas into the run, the way the test
// stage's own turn would. Committing matters: close refuses a dirty
// bureaucracy tree, so a canvas left uncommitted would fail the close
// for the wrong reason.
func (f *pushFixture) writeTestGate(body string) {
	f.t.Helper()
	writeContent(f.t, f.root, f.projectID, f.runID, "test", body)
	gittest.Run(f.t, f.root, "add", run.ContentPath(f.projectID, f.runID, "test"))
	gittest.Run(f.t, f.root, "commit", "-m",
		"work: update test\n\nMoE-Run: "+f.runID+"\nMoE-Document: test\n")
}

// rewindBranchToDefault drops the fixture's feature commit so moe/<run>
// sits exactly at the default branch — the shape of a run whose code
// stage attached a sandbox and committed nothing to it.
func (f *pushFixture) rewindBranchToDefault() {
	f.t.Helper()
	gittest.Run(f.t, f.clonePath, "reset", "--hard", "main")
}

// TestPushNoShipGateClosesRun is the headline path: a bureaucracy-only
// run whose test gate declares `ship: none` and whose branch carries no
// commits gets closed by push instead of refused, exits 0 so a cascade
// counts it as shipped, and leaves nothing on origin.
func TestPushNoShipGateClosesRun(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	originBefore := f.originHead()

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no project change to ship — closed "+f.branch) {
		t.Errorf("stdout = %q, want the no-ship close line naming the branch", stdout)
	}
	if md := f.reloadRun(); md.Status != run.StatusClosed {
		t.Fatalf("status = %s, want closed", md.Status)
	}
	if sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Error("expected the sandbox clone released by the close cleanup")
	}
	if got := f.originHead(); got != originBefore {
		t.Errorf("origin/main moved to %s; a no-ship close must not touch the target repo", got)
	}
	if f.originHasRef("refs/heads/" + f.branch) {
		t.Errorf("expected %s never pushed to origin", f.branch)
	}
	// The close commit subject is the run's record — it has to say the
	// run ended without a ship, not merely that it was closed.
	subject := gittest.Output(t, f.root, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "no ship: no project change") {
		t.Errorf("close commit subject = %q, want it to name the no-ship ending", subject)
	}
}

// TestPushNoShipGateIsIdempotent: a re-run lands in the existing
// already-closed arm rather than erroring on a released sandbox.
func TestPushNoShipGateIsIdempotent(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)

	if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code != 0 {
		t.Fatalf("first push exit=%d: %s", code, stderr)
	}
	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("re-run exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "already closed") {
		t.Errorf("stdout = %q, want `already closed`", stdout)
	}
}

// TestPushNoShipGateRefusesBranchWithCommits is the guard the second key
// buys: a gate that says "nothing to ship" while the branch carries
// commits is a contradiction, and closing would delete the clone holding
// them. Push refuses and leaves the run in progress.
func TestPushNoShipGateRefusesBranchWithCommits(t *testing.T) {
	f := newPushFixture(t) // fixture leaves one commit on moe/<run>
	f.writeTestGate(noShipGate)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("exit=0, want a refusal\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "1 commit(s) ahead") {
		t.Errorf("stderr = %q, want the count that contradicts the gate", stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status = %s, want in_progress — the refusal must not terminate the run", md.Status)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Error("expected the sandbox kept: it holds the commits the gate disagrees with")
	}
	if got := f.originHead(); got == f.tipSHA {
		t.Error("origin/main advanced; a refused push must not ship")
	}
}

// TestPushWithoutNoShipGateKeepsNothingToPushRefusal pins the accident
// detector. A branch with no commits and no declaration is the code
// stage that was meant to produce a diff and didn't — today's loud
// refusal stays exactly as it was.
func TestPushWithoutNoShipGateKeepsNothingToPushRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		canvas string
	}{
		{"ready gate without ship", plainReadyGate},
		{"no test canvas at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPushFixture(t)
			f.rewindBranchToDefault()
			if tc.canvas != "" {
				f.writeTestGate(tc.canvas)
			}

			stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
			if code == 0 {
				t.Fatalf("exit=0, want a refusal\nstdout=%s\nstderr=%s", stdout, stderr)
			}
			if !strings.Contains(stderr, "no commits ahead") {
				t.Errorf("stderr = %q, want today's nothing-to-push refusal", stderr)
			}
			if md := f.reloadRun(); md.Status != run.StatusInProgress {
				t.Fatalf("status = %s, want in_progress", md.Status)
			}
			if !sandbox.Exists(f.root, f.projectID, f.runID) {
				t.Error("expected the sandbox kept on a refusal")
			}
		})
	}
}

// TestPushNoShipGateRefusesUncomputableCount: when the ahead-count can't
// be computed the routing fails closed, the opposite of
// CheckBranchHasCommits's fail-open guard. Closing is destructive — it
// deletes the clone — so an unverifiable branch never reaches it. The
// unknown base is induced by pointing the project's default branch at a
// ref the clone doesn't have.
func TestPushNoShipGateRefusesUncomputableCount(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	writeFile(t, filepath.Join(f.root, "projects", f.projectID, "project.json"),
		`{"id":"`+f.projectID+`","submodule":"projects/`+f.projectID+`/src","remote":"`+f.origin+`","default_branch":"trunk"}`+"\n")
	gittest.Run(t, f.root, "add", filepath.Join("projects", f.projectID, "project.json"))
	gittest.Run(t, f.root, "commit", "-m", "point default_branch at a ref the clone lacks")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("exit=0, want a refusal\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "could not be computed") {
		t.Errorf("stderr = %q, want the uncomputable-count refusal", stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status = %s, want in_progress", md.Status)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Error("expected the sandbox kept: the count that would have justified deleting it is unknown")
	}
}

// TestPushNoShipGateClosesOnPRPath: `--pr` routes to the same close.
// There is nothing to open a PR for, and reaching GitHub at all would be
// wrong — the assertion that no PR was attempted is the point.
func TestPushNoShipGateClosesOnPRPath(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)

	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusClosed {
		t.Fatalf("status = %s, want closed (not pushed)", md.Status)
	}
	if strings.Contains(stdout, "PR") {
		t.Errorf("stdout = %q, want no PR work on a run with nothing to ship", stdout)
	}
	if f.originHasRef("refs/heads/" + f.branch) {
		t.Errorf("expected %s never pushed to origin", f.branch)
	}
}

// TestPushNoShipGateThreadsPulseInterrupt: the close-ship tails its own
// sweep and hands the "operator skipped" bool back, so a Ctrl-C'd survey
// halts a `!!!` ride here exactly as it does on the merge path. This is
// why the close is driven with tailPulse=false and the pulse fired from
// push — closeRunInProcess drops the bool.
func TestPushNoShipGateThreadsPulseInterrupt(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)

	orig := firePulse
	firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) bool { return true }
	t.Cleanup(func() { firePulse = orig })
	t.Cleanup(withRideMode(rideStatic))

	t.Setenv("MOE_HOME", f.root)
	t.Setenv("NO_COLOR", "1")
	var stdout, stderr bytes.Buffer
	code, interrupted, err := runPushTyped("sdlc", []string{f.projectID + "/" + f.runID}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runPushTyped: %v; stderr=%s", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%s", code, stderr.String())
	}
	if !interrupted {
		t.Fatal("close-ship dropped the tail pulse's interrupt; a `!!!` ride would carry on to the next run")
	}
	if md := f.reloadRun(); md.Status != run.StatusClosed {
		t.Fatalf("status = %s, want closed — the skipped sweep must not undo the close", md.Status)
	}
}

// TestPushNoShipGateSkipsPrePushHooks: the close routing sits ahead of
// the pre-push hook chain, so a project whose hooks would fail doesn't
// block a run with no tree to vet.
func TestPushNoShipGateSkipsPrePushHooks(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	hookDir := filepath.Join(f.root, "projects", f.projectID, "hooks", "pre-push.d")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hookDir, "10-fail.sh")
	writeFile(t, hook, "#!/bin/sh\necho 'hook ran' >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, f.root, "add", filepath.Join("projects", f.projectID, "hooks"))
	gittest.Run(t, f.root, "commit", "-m", "add a failing pre-push hook")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "hook ran") {
		t.Errorf("stderr = %q, want the pre-push hooks skipped: there is no tree to vet", stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusClosed {
		t.Fatalf("status = %s, want closed", md.Status)
	}
}

// TestPushNoShipGateRefusesDirtySandbox: the clean-tree guard stays
// upstream of the routing. Uncommitted edits in the sandbox are exactly
// the work a close would delete, so they must still refuse.
func TestPushNoShipGateRefusesDirtySandbox(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	writeFile(t, filepath.Join(f.clonePath, "stray.txt"), "uncommitted work\n")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("exit=0, want a refusal\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status = %s, want in_progress — the uncommitted edits would have gone with the clone", md.Status)
	}
	if !sandbox.Exists(f.root, f.projectID, f.runID) {
		t.Error("expected the sandbox clone kept")
	}
}
