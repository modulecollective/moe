package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/stylesheet"
)

const (
	liveSheet     = "* { model: live-model; }\n"
	proposedSheet = "* { model: next-model; }\n"
)

// proposeStylesheet commits a live root stylesheet and the run's
// proposed replacement, the state a stage turn leaves behind.
func (f *pushFixture) proposeStylesheet(body string) {
	f.t.Helper()
	gittest.WriteAndCommit(f.t, f.root, stylesheet.FileName, liveSheet, "seed root stylesheet")
	rel := filepath.Join(run.Dir(f.projectID, f.runID), stylesheet.FileName)
	gittest.WriteAndCommit(f.t, f.root, rel, body, "work: update design\n\nMoE-Run: "+f.runID+"\n")
}

// rootSheet reads the root stylesheet from disk.
func (f *pushFixture) rootSheet() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, stylesheet.FileName))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// sheetAt reads the root stylesheet as committed at rev, restoring the
// trailing newline gittest.Output trims.
func (f *pushFixture) sheetAt(rev string) string {
	f.t.Helper()
	return gittest.Output(f.t, f.root, "show", rev+":"+stylesheet.FileName) + "\n"
}

// requireCleanRoot fails when the bureaucracy tree has anything
// modified or staged — an applied-but-uncommitted stylesheet would steer
// every later turn on the box.
func (f *pushFixture) requireCleanRoot() {
	f.t.Helper()
	entries, err := git.Status(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		if e.XY != "??" {
			f.t.Fatalf("bureaucracy tree dirty: %#v", entries)
		}
	}
}

// TestPushMergeAppliesProposedStylesheet: the ff-merge record is where
// the switch goes live — the root file carries the proposed bytes in
// that commit and not before, and the run copy stays as the record of
// what was proposed.
func TestPushMergeAppliesProposedStylesheet(t *testing.T) {
	f := newPushFixture(t)
	f.proposeStylesheet(proposedSheet)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	// HEAD is the auto-bump; HEAD~1 is the merge record.
	if subject := gittest.Output(t, f.root, "log", "-1", "--format=%s", "HEAD~1"); !strings.Contains(subject, "merged") {
		t.Fatalf("HEAD~1 = %q, want the merge record", subject)
	}
	if got := f.sheetAt("HEAD~1"); got != proposedSheet {
		t.Fatalf("root stylesheet in merge record = %q, want %q", got, proposedSheet)
	}
	if got := f.sheetAt("HEAD~2"); got != liveSheet {
		t.Fatalf("root stylesheet before the merge record = %q, want %q", got, liveSheet)
	}
	proposed := filepath.Join(run.Dir(f.projectID, f.runID), stylesheet.FileName)
	if got := gittest.Output(t, f.root, "show", "HEAD:"+proposed) + "\n"; got != proposedSheet {
		t.Fatalf("run copy at HEAD = %q, want it kept as %q", got, proposedSheet)
	}
	f.requireCleanRoot()
}

// TestPushMergeWithoutProposalLeavesStylesheet: a run that proposed
// nothing must not touch the root file.
func TestPushMergeWithoutProposalLeavesStylesheet(t *testing.T) {
	f := newPushFixture(t)
	gittest.WriteAndCommit(t, f.root, stylesheet.FileName, liveSheet, "seed root stylesheet")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	changes := gittest.Output(t, f.root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD~1")
	if strings.Contains(changes, stylesheet.FileName) {
		t.Fatalf("merge record touched the stylesheet:\n%s", changes)
	}
	if got := f.rootSheet(); got != liveSheet {
		t.Fatalf("root stylesheet = %q, want %q", got, liveSheet)
	}
}

// TestPushRefusesInvalidProposedStylesheet: a proposal that wouldn't
// load refuses the push before origin work. Landed after the merge, it
// would make every later turn on the box refuse at load.
func TestPushRefusesInvalidProposedStylesheet(t *testing.T) {
	f := newPushFixture(t)
	f.proposeStylesheet("* { agent: no-such-agent; }\n")
	originBefore := f.originHead()

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code == 0 {
		t.Fatalf("expected refusal\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "proposed stylesheet") || !strings.Contains(stderr, "no-such-agent") {
		t.Fatalf("stderr should name the proposal and the bad value:\n%s", stderr)
	}
	if got := f.originHead(); got != originBefore {
		t.Fatalf("origin/main moved to %s on a refused push", got)
	}
	if f.originHasRef("refs/heads/" + f.branch) {
		t.Fatalf("refused push still pushed %s", f.branch)
	}
	if md := f.reloadRun(); md.Status != run.StatusInProgress {
		t.Fatalf("status = %s, want in-progress", md.Status)
	}
	if got := f.rootSheet(); got != liveSheet {
		t.Fatalf("root stylesheet = %q, want %q", got, liveSheet)
	}
	f.requireCleanRoot()
}

// TestPushResumedMergeRecordCarriesStylesheet: the applied root file
// rides the pending record's paths, so a resume lands it in the record.
func TestPushResumedMergeRecordCarriesStylesheet(t *testing.T) {
	f := newPushFixture(t)
	f.proposeStylesheet(proposedSheet)
	lift := failMergeRecordCommits(t, f.root)
	if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code == 0 {
		t.Fatalf("expected the stranding push to fail; stderr=%s", stderr)
	}
	if p := readPendingRecord(t, f); !slices.Contains(p.Paths, stylesheet.FileName) {
		t.Fatalf("pending paths missing %s: %v", stylesheet.FileName, p.Paths)
	}
	lift()

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := f.sheetAt("HEAD~1"); got != proposedSheet {
		t.Fatalf("root stylesheet in resumed record = %q, want %q", got, proposedSheet)
	}
	f.requireCleanRoot()
}

// TestPushNoShipCloseAppliesProposedStylesheet: a stylesheet-only run
// rides to push and closes through `ship: none`; that close is its ship.
func TestPushNoShipCloseAppliesProposedStylesheet(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	f.proposeStylesheet(proposedSheet)

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if subject := gittest.Output(t, f.root, "log", "-1", "--format=%s"); !strings.Contains(subject, "no ship") {
		t.Fatalf("HEAD = %q, want the no-ship close", subject)
	}
	if got := f.sheetAt("HEAD"); got != proposedSheet {
		t.Fatalf("root stylesheet in close commit = %q, want %q", got, proposedSheet)
	}
	f.requireCleanRoot()
}

// TestPushNoShipCloseFailureRestoresStylesheet: a close that dies after
// applying must put the root file back — left applied, it would go live
// without a record and wedge every later close on the dirty gate.
func TestPushNoShipCloseFailureRestoresStylesheet(t *testing.T) {
	f := newPushFixture(t)
	f.rewindBranchToDefault()
	f.writeTestGate(noShipGate)
	f.proposeStylesheet(proposedSheet)
	defer failCloseCommits(t, f.root)()

	if _, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID); code == 0 {
		t.Fatalf("expected the close to fail; stderr=%s", stderr)
	}
	if got := f.rootSheet(); got != liveSheet {
		t.Fatalf("root stylesheet after failed close = %q, want %q", got, liveSheet)
	}
	f.requireCleanRoot()
}

// TestPushPRRecordAppliesProposedStylesheet: the PR route applies at the
// record commit when the PR opens — the last point moe controls.
func TestPushPRRecordAppliesProposedStylesheet(t *testing.T) {
	f := newPRRecordFixture(t)
	f.proposeStylesheet(proposedSheet)

	stdout, stderr, code := f.runInRoot("sdlc", "push", "--pr", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := f.sheetAt("HEAD"); got != proposedSheet {
		t.Fatalf("root stylesheet in PR record = %q, want %q", got, proposedSheet)
	}
	f.requireCleanRoot()
}

// TestPushResumedPRRecordCarriesStylesheet: the pending PR record says
// it applied the stylesheet, so the resume stages the root file too.
func TestPushResumedPRRecordCarriesStylesheet(t *testing.T) {
	f := newPRRecordFixture(t)
	f.proposeStylesheet(proposedSheet)
	lift, _ := strandPRRecord(t, f)
	if p := readPRPendingRecord(t, f.pushFixture); !p.Stylesheet {
		t.Fatalf("pending PR record should flag the applied stylesheet: %+v", p)
	}
	lift()
	// The failed commit leaves its paths staged; drop that so the
	// resume has to stage the root file from the record, as it must
	// after anything else resets the index in between.
	gittest.Run(t, f.root, "reset", "-q")

	stdout, stderr, code := f.runInRoot("sdlc", "push", f.projectID+"/"+f.runID)
	if code != 0 {
		t.Fatalf("resume: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if got := f.sheetAt("HEAD"); got != proposedSheet {
		t.Fatalf("root stylesheet in resumed PR record = %q, want %q", got, proposedSheet)
	}
	f.requireCleanRoot()
}
