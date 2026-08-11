package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

const testTick = 20 * time.Minute

// quietFixture is spawnFixture with the project's registration commit
// pushed back out of the quiet window.
//
// Registering a project is an operator act — no machine trailers on it —
// and in a fixture it lands milliseconds before the gate looks, so every
// board would otherwise read as "the operator's hands are still on it".
// That reading is correct: a real board registered thirty seconds ago
// does get one tick of hesitation. It just isn't what these tests are
// about, and a board at rest is the shape they all start from.
func quietFixture(t *testing.T) string {
	t.Helper()
	root := spawnFixture(t)
	backdateHead(t, root, time.Hour)
	return root
}

// backdateHead rewrites HEAD's committer date to ago in the past, which
// is the field the quiet window compares.
func backdateHead(t *testing.T, root string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago).Format(time.RFC3339)
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + when, "GIT_AUTHOR_DATE=" + when},
		"commit", "--amend", "--no-edit", "--date="+when)
}

// journalCommit stamps an empty commit touching the project's tree, so
// the tip walk has something project-scoped to find. trailers is the
// block appended verbatim — empty for an operator-authored commit, a
// MoE-Consent line for a machine one.
func journalCommit(t *testing.T, root, projectID, subject, trailers string) {
	t.Helper()
	rel := "projects/" + projectID + "/.journal-probe"
	if err := os.WriteFile(root+"/"+rel, []byte(subject+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(root, "add", "--", rel); err != nil {
		t.Fatal(err)
	}
	msg := subject
	if trailers != "" {
		msg += "\n\n" + trailers
	}
	if err := git.Run(root, "commit", "-m", msg); err != nil {
		t.Fatal(err)
	}
}

// dueProjects runs one gate pass and returns what it would sweep.
func dueProjects(t *testing.T, g *heartbeatGate) []string {
	t.Helper()
	var log bytes.Buffer
	got := g.Due(testTick, &log)
	t.Logf("gate log:\n%s", log.String())
	return got
}

// TestHeartbeatFirstLookIsQuiet: a serve that just armed has, correctly,
// never looked. Seeding the cursor lazily is what keeps a restart from
// sweeping every registered project on a board where nothing is waiting
// — the parked-work leg is how an armed serve picks up real work, and
// that leg means something.
func TestHeartbeatFirstLookIsQuiet(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "operator: a note", "")

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v on a first look at a quiet board, want none", got)
	}
}

// TestHeartbeatSweepsOnJournalDelta: the primary trigger. Something
// landed in the project since the last sweep, so the machine looks.
func TestHeartbeatSweepsOnJournalDelta(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	g := newHeartbeatGate(root)
	dueProjects(t, g) // seeds the cursor

	journalCommit(t, root, "moe", "machine: another merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v, want [moe] once the journal moved", got)
	}
}

// TestHeartbeatSweptMovesTheCursor: a survey writes its own run-open and
// close commits, so without this the board would read as moved forever
// and every quiet tick would cost an agent turn — the exact cost the
// gate exists to avoid.
func TestHeartbeatSweptMovesTheCursor(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	g := newHeartbeatGate(root)
	dueProjects(t, g)

	journalCommit(t, root, "moe", "machine: another merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 {
		t.Fatalf("due = %v, want the delta sweep", got)
	}
	// The sweep runs and leaves its own commits behind.
	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjects(t, g); len(got) != 0 {
		t.Errorf("due = %v after its own sweep, want none — a quiet board must cost nothing", got)
	}
}

// TestHeartbeatSweepsWhenSettledWorkIsParked: the leg that makes arming
// serve pick up a board that was already waiting. No delta at all, but a
// machine-minted thread with a settled design and nobody inside is
// exactly what the self-kick admits.
func TestHeartbeatSweepsWhenSettledWorkIsParked(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := newHeartbeatGate(root)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v, want [moe] — settled work is parked", got)
	}
}

// sweptOnceOverParkedWork is the shape every parked-leg cursor test
// starts from: a board with one parked settled thread, a gate that has
// offered it once, and the run-open/close commits a survey leaves
// behind. The caller says how that survey ended.
//
// Returns the gate, ready for the tick *after* the sweep.
func sweptOnceOverParkedWork(t *testing.T, root string, clean bool) *heartbeatGate {
	t.Helper()
	g := newHeartbeatGate(root)
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the first look, want [moe] — settled work is parked", got)
	}
	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", clean)
	return g
}

// TestHeartbeatStopsOfferingWorkASweepAlreadyDeclined is the bug. The
// parked leg is a pure predicate over board state, so a sweep that
// looked at a thread and deliberately parked it with a reason leaves the
// board byte-identical to what it just declined — and without a memory
// of having looked, every tick after re-asks the same question at the
// cost of a full agent turn, forever.
func TestHeartbeatStopsOfferingWorkASweepAlreadyDeclined(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)

	for tick := range 3 {
		if got := dueProjects(t, g); len(got) != 0 {
			t.Fatalf("due = %v on tick %d after a clean sweep declined this board, want none", got, tick+1)
		}
	}
}

// TestHeartbeatKeepsOfferingWorkAfterAFailedSweep is the other side, and
// the property the fix is most at risk of breaking. A sweep that died
// answered nothing, so its board must keep being offered — the backoff
// ledger is what paces the retry, and it can only do that job if the
// gate keeps saying yes.
func TestHeartbeatKeepsOfferingWorkAfterAFailedSweep(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, false /*died*/)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after a sweep died, want [moe] — the retry is the backoff's to pace, not the gate's to swallow", got)
	}
}

// TestHeartbeatDeltaReArmsADeclinedBoard: standing down is scoped to the
// board the survey actually saw. Anything landing in the project moves
// the journal, and every input the parked leg reads is journal-derived —
// so the moved leg is the whole re-arm, and a clean sweep quiets it
// again.
func TestHeartbeatDeltaReArmsADeclinedBoard(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)
	if got := dueProjects(t, g); len(got) != 0 {
		t.Fatalf("due = %v right after the sweep, want none", got)
	}

	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v once the journal moved, want [moe]", got)
	}

	g.Swept("moe", true)
	if got := dueProjects(t, g); len(got) != 0 {
		t.Errorf("due = %v after the second sweep, want none", got)
	}
}

// TestHeartbeatReapReArmsADeclinedBoard: a reap frees a thread by
// removing a branch and a worktree, and writes no journal commit — so it
// changes the board invisibly to both cursors. Clearing the surveyed
// cursor is what keeps "moe died mid-turn" recoverable, and because reap
// runs at the top of Due it lands in the same tick.
func TestHeartbeatReapReArmsADeclinedBoard(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)
	if got := dueProjects(t, g); len(got) != 0 {
		t.Fatalf("due = %v right after the sweep, want none", got)
	}

	// The sweep kicked the thread and the walk died holding its branch.
	s, err := session.Open(root, "moe", minted["fix-a"], "design")
	if err != nil {
		t.Fatal(err)
	}
	writeDeadMachineClaim(t, s)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after the reap freed the thread, want [moe] in the same tick", got)
	}
	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Error("session branch survived the reap")
	}
}

// TestHeartbeatHoldsForOneQuietTick is the staging race, closed. The
// operator's last act on this project is minutes old, so the machine
// waits a full tick rather than picking work up from under their hands.
func TestHeartbeatHoldsForOneQuietTick(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	// An operator-authored commit lands now: no machine trailers on it.
	journalCommit(t, root, "moe", "chain: edit", "")

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v while the operator's hands are on the board, want none", got)
	}
}

// TestHeartbeatMovesOnceTheOperatorIsQuiet: the same board a tick later.
// The window is a hesitation, not a hold.
func TestHeartbeatMovesOnceTheOperatorIsQuiet(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	journalCommit(t, root, "moe", "chain: edit", "")

	// A zero-length tick makes any commit older than "now" quiet, which
	// is the same predicate a real tick applies twenty minutes later.
	var log bytes.Buffer
	if got := newHeartbeatGate(root).Due(0, &log); len(got) != 1 {
		t.Errorf("due = %v once the quiet window passed, want [moe]\n%s", got, log.String())
	}
}

// TestHeartbeatHoldsWhenAMachineCommitMasksTheOperator is the masking
// case: the operator arranges loose runs by hand, and a ride merging and
// closing lands its own commit on top before the next tick. Reading only
// the tip, the machine sees a machine commit and helps itself to a board
// whose arrangement is minutes old — the staging race the window exists
// to close, arriving through the one door a tip-only read leaves open.
func TestHeartbeatHoldsWhenAMachineCommitMasksTheOperator(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	journalCommit(t, root, "moe", "chain: edit", "")
	journalCommit(t, root, "moe", "push: a ride merged", "MoE-Consent: dynamic")

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v with the operator's act masked by a machine commit, want none", got)
	}
}

// TestHeartbeatMovesOnceAMaskedOperatorIsQuiet: the masked board a tick
// later. The window scan is a hesitation like the tip check it joins, so
// it must expire on the same terms.
func TestHeartbeatMovesOnceAMaskedOperatorIsQuiet(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	journalCommit(t, root, "moe", "chain: edit", "")
	journalCommit(t, root, "moe", "push: a ride merged", "MoE-Consent: dynamic")

	var log bytes.Buffer
	if got := newHeartbeatGate(root).Due(0, &log); len(got) != 1 {
		t.Errorf("due = %v once the quiet window passed, want [moe]\n%s", got, log.String())
	}
}

// TestHeartbeatStandsDownForALiveSession: a ride mid-hop or an operator
// sitting in a stage means somebody is already inside the project, and
// the tail-pulse path owns the next sweep.
func TestHeartbeatStandsDownForALiveSession(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	if _, err := session.Open(root, "moe", minted["fix-a"], "design"); err != nil {
		t.Fatal(err)
	}

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v with a live session in the project, want none", got)
	}
}

// TestHeartbeatStandsDownForASurveyMidTurn: the pulse's single-flight,
// read the only way that stays honest — a survey holds a session branch
// for its whole agent turn, exactly like any other stage. A sweep
// already running owns this generation.
func TestHeartbeatStandsDownForASurveyMidTurn(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "pulse-in-flight", pulseWorkflow, run.StatusInProgress, time.Now().Local(), nil)
	if _, err := session.Open(root, "moe", "pulse-in-flight", pulseDoc); err != nil {
		t.Fatal(err)
	}

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v with a survey mid-turn, want none", got)
	}
}

// TestHeartbeatSweepsPastALingeringOpenSurvey is the other side, and the
// one that keeps the loop resident. A sweep that died leaves its run
// open on the dash's ACTIVE list forever — nothing closes it but a
// human. Standing down on that would let the first vendor failure wedge
// this project's heartbeat until somebody noticed, and would leave the
// failure backoff pacing a loop that never gets to run. The open run is
// the operator's tell; the backoff is what bounds the pile.
func TestHeartbeatSweepsPastALingeringOpenSurvey(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	// A run open with no session branch under it: the shape a survey
	// leaves behind when its agent turn died.
	seedRun(t, root, "moe", "pulse-that-died", pulseWorkflow, run.StatusInProgress, time.Now().Local(), nil)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v, want [moe] — a dead sweep's lingering run must not wedge the heartbeat", got)
	}
}

// TestHeartbeatIgnoresAnOperatorStagedHead: a hand-minted head is
// stageless, so it never clears the settled-design admit and the parked
// leg does not fire on it. Staging a batch by hand must not summon the
// machine.
func TestHeartbeatIgnoresAnOperatorStagedHead(t *testing.T) {
	root := quietFixture(t)
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	seedRun(t, root, "moe", "sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# A thought\n\nseed\n"})
	chainEdgeCommit(t, root, "moe/"+head.ID, "moe/sketch")

	g := newHeartbeatGate(root)
	dueProjects(t, g) // seed the cursor past the edge commit
	if got := dueProjects(t, g); len(got) != 0 {
		t.Errorf("due = %v for a hand-staged batch, want none", got)
	}
}

// TestHeartbeatReapsADeadMachineSession is the recovery half: `moe`
// died mid-turn, the session branch it left behind holds the run under
// the occupancy guard, and nothing else ever clears it. A robot half
// turn is regenerable, so the branch goes and the run re-parks.
func TestHeartbeatReapsADeadMachineSession(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	s, err := session.Open(root, "moe", minted["fix-a"], "design")
	if err != nil {
		t.Fatal(err)
	}
	writeDeadMachineClaim(t, s)

	var log bytes.Buffer
	newHeartbeatGate(root).Due(testTick, &log)

	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Errorf("session branch survived the reap\n%s", log.String())
	}
	if !strings.Contains(log.String(), "reaped dead machine session") {
		t.Errorf("log = %q, want the reap named", log.String())
	}
}

// TestHeartbeatNeverReapsAnUnmarkedSession: absence is unknown, never
// operator — and unknown is never touched. The operator's own stage
// session, and any session opened by a binary that predates the claim,
// both land here.
func TestHeartbeatNeverReapsAnUnmarkedSession(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	s, err := session.Open(root, "moe", minted["fix-a"], "design")
	if err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	newHeartbeatGate(root).Due(testTick, &log)

	if !git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Errorf("an unclaimed session was reaped; only a proven robot corpse may go\n%s", log.String())
	}
}

// writeDeadMachineClaim stages the liveness record a machine walk
// leaves behind when its process dies mid-turn: machine-marked, owned by
// a same-host pid that has exited, and with a heartbeat old enough to
// have stopped vouching. Both dead signals are needed, so both are here.
//
// The pid comes from a child run to completion rather than an invented
// number — it is the only way to name a pid this host is genuinely
// finished with.
func writeDeadMachineClaim(t *testing.T, s *session.Session) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run throwaway child: %v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname on this box; the same-host leg can't be staged")
	}
	body, err := json.Marshal(session.Claim{
		Branch:      s.Branch,
		Machine:     true,
		Owner:       fmt.Sprintf("%s/%d", host, cmd.Process.Pid),
		StartedAt:   time.Now().Add(-time.Hour).UTC(),
		HeartbeatAt: time.Now().Add(-2 * session.StaleAfter).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := session.ClaimPath(s.Root, s.Project, s.Run, s.Doc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
