package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// deadMachineSession opens a session on a run, puts one commit on its
// branch (what a real turn's `work: start session` leaves behind), and
// marks the claim dead. Returns the session and its branch tip — the sha
// the reap is supposed to record before it destroys the branch.
//
// The commit matters: without it the branch tip is main's, and a
// tombstone that recorded main would pass a test while pointing at
// nothing.
func deadMachineSession(t *testing.T, root, projectID, runID, doc string) (*session.Session, string) {
	t.Helper()
	s, err := session.Open(root, projectID, runID, doc)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(s.WorktreePath, run.Dir(projectID, runID), "session-probe.txt")
	if err := os.WriteFile(probe, []byte("half a turn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(s.WorktreePath, "add", "--", probe); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(s.WorktreePath, "commit", "-m", "work: start session for "+doc); err != nil {
		t.Fatal(err)
	}
	tip, err := git.RevParse(root, s.Branch)
	if err != nil {
		t.Fatal(err)
	}
	writeDeadClaim(t, s, true /*machine*/)
	return s, tip
}

func loadRun(t *testing.T, root, projectID, runID string) *run.Metadata {
	t.Helper()
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// TestReapStampsATombstone is the whole point of the change. A machine
// turn that dies before writing anything used to vanish twice over — the
// kick loop's account went to a stderr nobody keeps, and the reap then
// deleted the branch holding the only copy of the transcript. What was
// left on disk was byte-identical to a run the loop never reached.
func TestReapStampsATombstone(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	slug := minted["fix-a"]
	s, tip := deadMachineSession(t, root, "moe", slug, "design")

	g := newHeartbeatGate(root)
	dueDecisions(t, g, testTick)

	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Fatal("session branch survived the reap")
	}
	md := loadRun(t, root, "moe", slug)
	if md.Reaped == nil {
		t.Fatal("run carries no tombstone after the reap")
	}
	if md.Reaped.Doc != "design" {
		t.Errorf("tombstone doc = %q, want design", md.Reaped.Doc)
	}
	if md.Reaped.Tip != tip {
		t.Errorf("tombstone tip = %q, want the pre-abandon branch tip %q", md.Reaped.Tip, tip)
	}
	if md.Reaped.At == "" {
		t.Error("tombstone carries no timestamp")
	}
	// The evidence is only useful if it's still reachable: the abandoned
	// commits dangle rather than disappear, which is what the sha buys.
	if _, err := git.Output(root, "cat-file", "-p", tip); err != nil {
		t.Errorf("recorded tip is not readable in the object db: %v", err)
	}

	body := gitLogFormat(t, root, 1, "HEAD", "%B")
	wantSubject := "reap: moe/" + slug + " design turn died — abandoned " + git.ShortSHA(tip)
	for _, want := range []string{wantSubject, "MoE-Run: " + slug, "MoE-Project: moe", "MoE-Consent: dynamic"} {
		if !strings.Contains(body, want) {
			t.Errorf("tombstone commit missing %q:\n%s", want, body)
		}
	}
	// Machine-marked deliberately: an unmarked commit at the journal tip
	// reads as the operator's and holds the project for a quiet tick —
	// the opposite of what a reap is for.
	if !machineAuthored(body) {
		t.Error("tombstone commit reads as operator-authored")
	}
	if entries, err := git.Status(root); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("reap left the tree dirty:\n%v", entries)
	}
}

// TestReapLandsATombstoneWithoutWedgingTheTick: the reap's own commit
// moves the journal, and the gate must read that as ordinary machine
// motion — the freed thread still gets swept in the same tick, which is
// the recovery the reap exists to enable.
func TestReapTombstoneStillFreesTheThreadThisTick(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)
	if got := dueProjects(t, g); len(got) != 0 {
		t.Fatalf("due = %v right after the sweep, want none", got)
	}

	deadMachineSession(t, root, "moe", minted["fix-a"], "design")

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after the reap freed the thread, want [moe] in the same tick", got)
	}
}

// TestReapKeepsTheBranchWhenTheTombstoneFails is the ordering rule. The
// note is the whole point of the change, so a reap that cannot write one
// must not go on to destroy the evidence it was about to describe — it
// leaves the branch and the next tick retries.
func TestReapKeepsTheBranchWhenTheTombstoneFails(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	slug := minted["fix-a"]
	s, _ := deadMachineSession(t, root, "moe", slug, "design")

	// Scoped to this repo so only the tombstone's own commit fails; every
	// other commit in the fixture already landed.
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(root, "config", "core.hooksPath", hooks); err != nil {
		t.Fatal(err)
	}

	g := newHeartbeatGate(root)
	dueDecisions(t, g, testTick)

	if !git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Fatal("reap destroyed the branch after failing to record it")
	}
	if md := loadRun(t, root, "moe", slug); md.Reaped != nil {
		t.Errorf("run carries a tombstone the commit never landed: %+v", md.Reaped)
	}
	// The half-written run.json is rolled back rather than left loose:
	// the branch survives, so a stray edit in the canonical tree would
	// only give the next session's rebase something to trip on.
	if entries, err := git.Status(root); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Errorf("failed tombstone left the tree dirty:\n%v", entries)
	}

	// And the retry lands once the commit can succeed again.
	if err := git.Run(root, "config", "--unset", "core.hooksPath"); err != nil {
		t.Fatal(err)
	}
	dueDecisions(t, newHeartbeatGate(root), testTick)
	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Error("session branch survived the retry")
	}
	if md := loadRun(t, root, "moe", slug); md.Reaped == nil {
		t.Error("retry abandoned the branch without recording the tombstone")
	}
}

// TestReapDoesNotTombstoneALandingSession: the other reap ending puts
// real work on main. Nothing died there, so nothing is owed a note.
func TestReapDoesNotTombstoneALandingSession(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	slug := minted["fix-a"]

	s, err := session.Open(root, "moe", slug, "design")
	if err != nil {
		t.Fatal(err)
	}
	canvasRel := run.ContentPath("moe", slug, "design")
	canvas := filepath.Join(s.WorktreePath, canvasRel)
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvas, []byte("# a landed turn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(s.WorktreePath, "add", "--", canvasRel); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(s.WorktreePath, "commit", "-m", "work: update design"); err != nil {
		t.Fatal(err)
	}
	writeDeadClaim(t, s, true /*machine*/)

	dueDecisions(t, newHeartbeatGate(root), testTick)

	if md := loadRun(t, root, "moe", slug); md.Reaped != nil {
		t.Errorf("a landed turn earned a tombstone: %+v", md.Reaped)
	}
}

// TestSessionStartClearsTheTombstone is the eraser half. One writer at
// the reap, one eraser at the next session start: a run being worked
// again is the answer to "the last machine turn died", so the note is
// spent. Earlier tombstones stay in the journal's history.
func TestSessionStartClearsTheTombstone(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/revived"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}

	md := loadRun(t, root, "tele", "revived")
	md.Reaped = &run.ReapNote{Doc: "design", At: "2026-08-23T23:14:14Z", Tip: strings.Repeat("a", 40)}
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	if err := run.StageAndCommit(root, "reap: a prior death",
		filepath.Join(run.Dir("tele", "revived"), "run.json")); err != nil {
		t.Fatal(err)
	}

	fakeClaudeOnPath(t, quietFakeClaudeScript)
	out.Reset()
	errb.Reset()
	if code := openSdlcDesign("tele", "revived", true, "", &out, &errb); code != 0 {
		t.Fatalf("openSdlcDesign exit=%d stderr=%q", code, errb.String())
	}

	if md := loadRun(t, root, "tele", "revived"); md.Reaped != nil {
		t.Errorf("tombstone survived a session on the run: %+v", md.Reaped)
	}
}

// TestReapTombstoneMidSweepRefusesTheCursorsAndConverges is the
// interaction the design flagged. The tombstone is the first journal
// commit the reap has ever written, so it now lands in the window a
// sweep already in flight can't have seen. Its MoE-Workflow trailer
// makes it read as a ride's — which is the right answer: a thread the
// survey saw as occupied just got freed under it, so the follow-up
// sweep is owed. And the walk still ends, because that sweep's own
// window holds only its own bookkeeping.
func TestReapTombstoneMidSweepRefusesTheCursorsAndConverges(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	g := newHeartbeatGate(root)
	dueProjects(t, g)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 {
		t.Fatalf("due = %v, want the delta sweep that puts a child in flight", got)
	}

	// Mid-sweep: a ride the survey saw as occupied dies, and the next
	// tick's reap tombstones it inside the in-flight sweep's window.
	deadMachineSession(t, root, "moe", minted["fix-a"], "design")
	dueProjects(t, newHeartbeatGate(root))
	g.Swept("moe", true /*clean*/)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v after a reap landed mid-sweep, want [moe]", got)
	}

	// Generation 2 kicks nothing, so its window holds only its own
	// commits and the walk ends.
	journalCommit(t, root, "moe", "open: pulse-2", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-2", "MoE-Workflow: pulse\nMoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after a sweep that kicked nothing, want none", got)
	}
}
