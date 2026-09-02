package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// setMode flips a project's mode the way the CLI verb does, minus the
// journal push (fixtures have no remote).
func setMode(t *testing.T, root, projectID string, mode project.Mode) {
	t.Helper()
	if err := project.SetMode(root, projectID, mode); err != nil {
		t.Fatal(err)
	}
	// The flip is an operator commit landing milliseconds before the gate
	// looks, which is exactly the shape the quiet window holds. True of a
	// real board too; it just isn't what these tests are about.
	backdateHead(t, root, time.Hour)
}

// asClock runs fn as the heartbeat's own child. The mode binds the
// clock, so nothing below fires without it.
func asClock(fn func()) {
	defer withClockInvoked()()
	fn()
}

// advanceRun lands a real work-turn at docID and the operator's `a` on
// top of it — the marker safe's admit reads. Through the production
// seams, because the staleness rule the admit inherits is a comparison
// between exactly these two commits.
func advanceRun(t *testing.T, root, runID, docID string) {
	t.Helper()
	md, err := run.Load(root, "moe", runID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, run.ContentPath("moe", runID, docID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# a turn\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The document entry a real session would have minted; commitTurn
	// reads its session id for the trailer block.
	if md.Documents[docID] == nil {
		md.Documents[docID] = &run.Document{}
	}
	if err := commitTurn(root, md, docID, 0); err != nil {
		t.Fatalf("commitTurn: %v", err)
	}
	if err := commitAdvance(root, md, docID); err != nil {
		t.Fatalf("commitAdvance: %v", err)
	}
}

// --- the verb ---------------------------------------------------------

// TestProjectModeVerbReadsAndSets: no argument is the question, an
// argument is the command — the shape the operator reaches for far more
// often to ask than to change.
func TestProjectModeVerbReadsAndSets(t *testing.T) {
	root := spawnFixture(t)

	var out, errb bytes.Buffer
	if code := Run([]string{"project", "mode", "moe"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := out.String(); got != "moe: auto\n" {
		t.Errorf("read on a fresh project = %q, want auto", got)
	}

	out.Reset()
	if code := Run([]string{"project", "mode", "moe", "safe"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if mode, err := project.ReadMode(root, "moe"); err != nil || mode != project.ModeSafe {
		t.Fatalf("ReadMode = %q, %v; want safe", mode, err)
	}

	out.Reset()
	if code := Run([]string{"project", "mode", "moe"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if got := out.String(); got != "moe: safe\n" {
		t.Errorf("read after the set = %q, want safe", got)
	}
}

// TestProjectModeVerbRefusesJunk: a mode nobody recognises is exactly
// the case where guessing would arm the machine.
func TestProjectModeVerbRefusesJunk(t *testing.T) {
	root := spawnFixture(t)
	for _, args := range [][]string{
		{"project", "mode", "moe", "snooze"},
		{"project", "mode", "moe", "SAFE"},
		{"project", "mode"},
		{"project", "mode", "moe", "safe", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code == 0 {
			t.Errorf("%v exited 0; stdout=%q", args, out.String())
		}
	}
	if mode, _ := project.ReadMode(root, "moe"); mode != project.ModeAuto {
		t.Errorf("a rejected mode must not have been written: %q", mode)
	}
}

// --- the shared predicate --------------------------------------------

// TestOperatorMarkedAdmitsTheThreeShapes and its sibling below are the
// admit `safe` holds the clock to. Every leg is a disk fact about the
// work; keying on who opened the run is the proxy that stranded runs
// twice in July.
func TestOperatorMarkedAdmitsATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	key := parkTaggedIdea(t, root, "cleanup-foo", "sdlc")
	sc := mustPulseScan(t, root)

	if !operatorMarked(root, sc.byKey[key], sc.mds, sc.idx) {
		t.Error("a workflow tag is the operator's licence and must admit")
	}
}

// TestOperatorMarkedHoldsAnUntaggedIdea: untagged means human, and an
// idea nobody has licensed is the operator's inbox.
func TestOperatorMarkedHoldsAnUntaggedIdea(t *testing.T) {
	root := quietFixture(t)
	key := parkTaggedIdea(t, root, "needs-triage", "")
	sc := mustPulseScan(t, root)

	if operatorMarked(root, sc.byKey[key], sc.mds, sc.idx) {
		t.Error("an untagged idea carries no mark")
	}
}

// TestOperatorMarkedHoldsAMachineSpawn is the shape safe exists for: a
// survey-invented fix run is settled by rootDesignSettled's lights and
// carries no operator look at all.
func TestOperatorMarkedHoldsAMachineSpawn(t *testing.T) {
	root := quietFixture(t)
	key := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]
	sc := mustPulseScan(t, root)

	md := sc.byKey[key]
	if settled, _ := rootDesignSettled(root, md, sc.idx); !settled {
		t.Fatal("fixture: a machine spawn should read as settled")
	}
	if operatorMarked(root, md, sc.mds, sc.idx) {
		t.Error("a machine-baked design nobody has looked at is not an operator mark")
	}
}

// TestOperatorMarkedAdmitsAnAdvancedRun: the marker is recorded
// permission to carry the thread forward, and AdvancedTo asks about
// whatever stage the run waits at — so it admits a mid-ladder resume as
// readily as a design the operator signed off.
func TestOperatorMarkedAdmitsAnAdvancedRun(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	advanceRun(t, root, id, "design")
	sc := mustPulseScan(t, root)

	if !operatorMarked(root, sc.byKey["moe/"+id], sc.mds, sc.idx) {
		t.Error("an advance marker is the operator saying carry this forward")
	}
}

// TestOperatorMarkedAdmitsAChoreRootedRun: the seed is the chore's
// operator-authored prompt.md, so standing intent is an operator mark by
// construction. Its own leg because openChoreInProcess is the one
// machine-open path that stamps no SpawnedBy — the shape that stranded a
// judged chore on 2026-07-22, and the one safe would strand again if the
// admit only looked at lineage.
func TestOperatorMarkedAdmitsAChoreRootedRun(t *testing.T) {
	root, threadRoot, groomed, _ := choreKickFixture(t)

	if !operatorMarked(root, groomed.byKey[threadRoot], groomed.mds, groomed.idx) {
		t.Error("a chore's prompt.md is standing operator intent and must admit")
	}
}

// TestOperatorMarkedAdmitsARunPromotedFromATaggedIdea is the subtlest of
// the legs: the mark was spent before the run existed. The tag licensed
// the work, a sweep promoted the idea, and the destination run carries no
// tag of its own — so the admit walks back through the journal's
// promotion edge to the idea's persisted PromoteTo. Driven through the
// real mint rather than a hand-built index, because both halves of that
// walk are facts about the production path: promotion leaves PromoteTo in
// place, and the edge points idea→run.
func TestOperatorMarkedAdmitsARunPromotedFromATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	parkTaggedIdea(t, root, "cleanup-foo", "sdlc")

	minted := mintSpecs(root, "moe", "pulse-one",
		[]pulseRunSpec{{Slug: "cleanup-foo", Title: "cleanup foo"}}, io.Discard, os.Stderr)
	destID := minted["cleanup-foo"]
	if destID == "" {
		t.Fatalf("fixture: the tagged idea should have promoted; minted=%v", minted)
	}

	sc := mustPulseScan(t, root)
	if got := sc.idx.PromotedTo["moe/cleanup-foo"]; got != "moe/"+destID {
		t.Fatalf("fixture: promotion edge = %q, want moe/%s", got, destID)
	}
	if !operatorMarked(root, sc.byKey["moe/"+destID], sc.mds, sc.idx) {
		t.Error("the tag was the licence; the run it promoted into inherits the mark")
	}
}

// --- the parked-leg pre-ask ------------------------------------------

// TestParkedLegUnderSafeHoldsAnUnmarkedBoard: a board with plenty parked
// and nothing marked must cost no agent turn to discover, every twenty
// minutes, forever.
func TestParkedLegUnderSafeHoldsAnUnmarkedBoard(t *testing.T) {
	root := quietFixture(t)
	want := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]
	sc := mustPulseScan(t, root)

	if got := parkedKickableThread(root, sc, "moe", false); got != want {
		t.Fatalf("fixture: auto should offer %q, got %q", want, got)
	}
	if got := parkedKickableThread(root, sc, "moe", true); got != "" {
		t.Errorf("parkedKickableThread under safe = %q, want \"\" — nothing here is marked", got)
	}
}

// TestParkedLegUnderSafeStillSeesATaggedIdea: the tag *is* the mark, so
// the fourth shape passes safe by construction.
func TestParkedLegUnderSafeStillSeesATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	want := parkTaggedIdea(t, root, "cleanup-foo", "sdlc")

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", true); got != want {
		t.Errorf("parkedKickableThread under safe = %q, want the tagged idea %q", got, want)
	}
}

// --- the gate ---------------------------------------------------------

// TestPausedProjectIsNeverSwept is the whole of what paused promises:
// not even the read that would decide to sweep.
func TestPausedProjectIsNeverSwept(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	setMode(t, root, "moe", project.ModePaused)
	g := newHeartbeatGate(root)

	decisions := dueDecisions(t, g, testTick)
	if got := sweepIDs(decisions); len(got) != 0 {
		t.Errorf("due = %v on a paused project, want none", got)
	}
	if got := reasonFor(decisions, "moe"); got != "paused" {
		t.Errorf("reason = %q, want %q", got, "paused")
	}
	// Not held: the operator chose this state, so a held row every twenty
	// minutes would be the panel nagging about a decision. The mode badge
	// carries the visibility instead.
	if heldFor(decisions, "moe") {
		t.Error("a paused project must collapse into the quiet count, not earn a held row")
	}
}

// TestSafeProjectSweepsOnADeltaButNotOnAnUnmarkedParkedBoard: safe stops
// the machine disposing, not proposing. A journal move still deserves a
// survey; a parked board with nothing marked does not.
func TestSafeProjectSweepsOnADeltaButNotOnAnUnmarkedParkedBoard(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	setMode(t, root, "moe", project.ModeSafe)

	g := newHeartbeatGate(root)
	decisions := dueDecisions(t, g, 0)
	if got := sweepIDs(decisions); len(got) != 0 {
		t.Fatalf("due = %v, want none — nothing on this board is marked", got)
	}
	if got := reasonFor(decisions, "moe"); !strings.Contains(got, "operator-marked") {
		t.Errorf("reason = %q, want the safe-mode wording", got)
	}

	// A delta the operator landed still summons a survey.
	journalCommit(t, root, "moe", "operator: a note", "")
	backdateHead(t, root, time.Hour)
	if got := sweepIDs(dueDecisions(t, g, 0)); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after a journal move, want [moe] — safe still grooms", got)
	}
}

// TestUnpausingSummonsASweepOffItsOwnCommit is why SetMode commits
// trailer-free on main rather than writing project.json quietly: the
// flip moves the project's journal tip, so the un-pause *is* the delta
// the moved leg fires on and newly licensed work starts without anyone
// remembering to pulse.
//
// Driven from a warm cursor — a serve that swept this project before the
// pause — because that is the shape the claim is about. A gate that
// first sees the project while it is paused has no cursor to compare
// against, so its first tick after the flip lazily seeds one instead;
// nothing is stranded by that, since anything the operator then marks is
// itself another commit, and the sibling tests above cover the parked
// leg finding marked work on a cold gate.
func TestUnpausingSummonsASweepOffItsOwnCommit(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := newHeartbeatGate(root)

	if got := sweepIDs(dueDecisions(t, g, testTick)); len(got) != 1 {
		t.Fatalf("fixture: the board should be sweepable before the pause, got %v", got)
	}
	g.Swept("moe", true)
	if got := sweepIDs(dueDecisions(t, g, testTick)); len(got) != 0 {
		t.Fatalf("fixture: a swept tip should stand down, got %v", got)
	}

	setMode(t, root, "moe", project.ModePaused)
	if got := reasonFor(dueDecisions(t, g, testTick), "moe"); got != "paused" {
		t.Fatalf("reason = %q, want paused", got)
	}

	setMode(t, root, "moe", project.ModeSafe)
	decisions := dueDecisions(t, g, testTick)
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v after the un-pause, want [moe] — the flip is its own delta", got)
	}
	if got := reasonFor(decisions, "moe"); !strings.Contains(got, "journal moved") {
		t.Errorf("reason = %q, want the moved leg — the mode commit is the move", got)
	}
}

// --- the kick ---------------------------------------------------------

// planFor builds the kick plan a sweep over root's board would walk.
func planFor(t *testing.T, root string) kickPlan {
	t.Helper()
	sc := mustPulseScan(t, root)
	return planKick(root, groomResult{
		byKey: sc.byKey, mds: sc.mds, graph: sc.graph, idx: sc.idx, projectID: "moe",
	})
}

// holdFor returns one root's hold in a plan, or "" when it is queued.
func holdFor(t *testing.T, plan kickPlan, rootKey string) string {
	t.Helper()
	for _, step := range plan.Steps {
		if step.Root == rootKey {
			return step.Hold
		}
	}
	t.Fatalf("plan has no step for %s: %+v", rootKey, plan.Steps)
	return ""
}

// TestKickUnderSafeHoldsAnUnmarkedRoot: the kick's admit and the parked
// leg's pre-ask are one predicate, so what the tick declined to offer is
// also what a sweep that got there anyway declines to start.
func TestKickUnderSafeHoldsAnUnmarkedRoot(t *testing.T) {
	root := quietFixture(t)
	key := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]
	setMode(t, root, "moe", project.ModeSafe)

	// Typed, the mode is not consulted at all: the operator saying "sweep
	// and start things" is consent, whatever the standing config says.
	if got := holdFor(t, planFor(t, root), key); got != "" {
		t.Errorf("a hand-typed sweep read the mode: %q", got)
	}

	asClock(func() {
		if got := holdFor(t, planFor(t, root), key); !strings.Contains(got, "safe mode") {
			t.Errorf("hold = %q, want the safe-mode reason", got)
		}
	})
}

// TestKickUnderSafeStartsAMarkedRoot: safe is a filter, not a stop.
func TestKickUnderSafeStartsAMarkedRoot(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	advanceRun(t, root, id, "design")
	setMode(t, root, "moe", project.ModeSafe)

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); got != "" {
			t.Errorf("an advanced root was held under safe: %q", got)
		}
	})
}

// TestKickUnderPausedStartsNothing covers the mode flip that lands
// inside the gate→child window: the gate decided to sweep, the operator
// paused the project, and the child that arrives afterwards obeys.
func TestKickUnderPausedStartsNothing(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	advanceRun(t, root, id, "design")
	setMode(t, root, "moe", project.ModePaused)

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); !strings.Contains(got, "paused mode") {
			t.Errorf("hold = %q, want the paused reason even for a marked root", got)
		}
	})
}

// TestSafeModeContextLineReachesOnlyTheClock: the survey is told what
// its grooming can expect to start, and only when the mode actually
// binds this invocation.
func TestSafeModeContextLineReachesOnlyTheClock(t *testing.T) {
	root := quietFixture(t)
	setMode(t, root, "moe", project.ModeSafe)

	if got := projectModeContextLine(root, "moe"); got != "" {
		t.Errorf("a hand-typed sweep got the mode line: %q", got)
	}
	asClock(func() {
		if got := projectModeContextLine(root, "moe"); !strings.Contains(got, "**safe** mode") {
			t.Errorf("context line = %q, want the safe-mode block", got)
		}
	})
	setMode(t, root, "moe", project.ModeAuto)
	asClock(func() {
		if got := projectModeContextLine(root, "moe"); got != "" {
			t.Errorf("auto is the ordinary state and earns no line: %q", got)
		}
	})
}

// TestKickSectionNamesTheModeHold: the canvas section and the stderr
// skip line are one wording, so an operator reading a sweep's report
// sees the same sentence the terminal would have shown.
func TestKickSectionNamesTheModeHold(t *testing.T) {
	root := quietFixture(t)
	key := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]
	setMode(t, root, "moe", project.ModePaused)

	asClock(func() {
		section := renderKickSection(planFor(t, root))
		if !strings.Contains(section, key+" — parked board, is held by paused mode") {
			t.Errorf("kick section missing the mode hold:\n%s", section)
		}
	})
}
