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
	"github.com/modulecollective/moe/internal/serve"
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

// dueProjects runs one gate pass and returns what it would sweep. Due
// answers for every project now, so the sweep subset is what these tests
// keep asserting on — the reasons have their own tests below.
func dueProjects(t *testing.T, g *heartbeatGate) []string {
	t.Helper()
	return sweepIDs(dueDecisions(t, g, testTick))
}

// dueProjectsPastTheWindow is dueProjects with a zero-length tick, which
// makes any commit older than *now* quiet — the same predicate a real
// tick applies twenty minutes later. Fixtures land fresh-dated commits,
// so a test whose shape needs an operator commit in the history needs
// this to reach the leg it is actually about.
func dueProjectsPastTheWindow(t *testing.T, g *heartbeatGate) []string {
	t.Helper()
	return sweepIDs(dueDecisions(t, g, 0))
}

// dueDecisions runs one gate pass and returns the whole verdict set.
func dueDecisions(t *testing.T, g *heartbeatGate, tick time.Duration) []serve.HeartbeatDecision {
	t.Helper()
	var log bytes.Buffer
	got := g.Due(tick, &log)
	t.Logf("gate log:\n%s", log.String())
	return got
}

func sweepIDs(decisions []serve.HeartbeatDecision) []string {
	var ids []string
	for _, d := range decisions {
		if d.Sweep {
			ids = append(ids, d.Project)
		}
	}
	return ids
}

// reasonFor returns the gate's words for one project in a verdict set,
// or "" when the project isn't in it at all.
func reasonFor(decisions []serve.HeartbeatDecision, projectID string) string {
	for _, d := range decisions {
		if d.Project == projectID {
			return d.Reason
		}
	}
	return ""
}

// heldFor reports whether one project's verdict was a hold — a sweep
// that wanted to run and didn't. False for a project that isn't in the
// set at all, which is the same answer as "nothing held it".
func heldFor(decisions []serve.HeartbeatDecision, projectID string) bool {
	for _, d := range decisions {
		if d.Project == projectID {
			return d.Held
		}
	}
	return false
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

// TestHeartbeatStandDownReasonsCrossTheSeam: the gate's verdicts used to
// leave only the sweeps behind, with every stand-down reason going to
// stderr and dying there. That is why four heartbeat bugs had to be
// diagnosed by reading code and inferring invisible cursor state. Now
// every project's reason crosses with it, in the gate's own words.
func TestHeartbeatStandDownReasonsCrossTheSeam(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "operator: a note", "")
	g := newHeartbeatGate(root)

	// A hand-commit inside the window: the operator gets a full tick with
	// their hands on the board before the machine moves.
	if got := reasonFor(dueDecisions(t, g, testTick), "moe"); !strings.Contains(got, "operator commit") {
		t.Errorf("reason = %q, want the quiet window named", got)
	}

	// Past the window, with a clean sweep on the current tip: whatever is
	// parked, a survey already saw and left parked on purpose.
	g.Swept("moe", true)
	if got := reasonFor(dueDecisions(t, g, 0), "moe"); !strings.Contains(got, "surveyed the current tip") {
		t.Errorf("reason = %q, want the surveyed cursor named", got)
	}
}

// TestHeartbeatMarksTheHeldStandDowns: /serve renders only the
// non-trivial, and this bit is what tells the two kinds of quiet apart.
// A sweep blocked by something outside the machine — an operator commit
// in the window, somebody inside the project — is news; "nothing to do"
// spelled three ways is not. The split lives here rather than in a
// reason-string match on the render side, which is what keeps the reason
// strings free to be reworded.
func TestHeartbeatMarksTheHeldStandDowns(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "operator: a note", "")
	g := newHeartbeatGate(root)

	if !heldFor(dueDecisions(t, g, testTick), "moe") {
		t.Error("an operator commit inside the window is a hold, not background hum")
	}

	// Past the window with a clean sweep on the tip: nothing to do, and
	// nothing for the operator to act on either.
	g.Swept("moe", true)
	if heldFor(dueDecisions(t, g, 0), "moe") {
		t.Error("a surveyed tip is the background hum, not a hold")
	}
}

// TestHeartbeatMarksALiveSessionAsHeld: the other hold. Somebody is
// inside the project, which is a fact about the board the operator can
// see the consequence of, so it earns its row on /serve.
func TestHeartbeatMarksALiveSessionAsHeld(t *testing.T) {
	root := quietFixture(t)
	minted := groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)

	// The journal moved, so this tick wants to sweep — and then finds
	// somebody inside. That is the shape a hold is for; a board with
	// nothing to do never reaches the occupancy check at all.
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	if _, err := session.Open(root, "moe", minted["fix-a"], "design"); err != nil {
		t.Fatal(err)
	}

	decisions := dueDecisions(t, g, testTick)
	if got := reasonFor(decisions, "moe"); !strings.Contains(got, "already inside") {
		t.Fatalf("reason = %q, want the occupancy stand-down — the test isn't reaching it", got)
	}
	if !heldFor(decisions, "moe") {
		t.Error("a live session holds a sweep that wanted to run — that is a hold")
	}
}

// TestHeartbeatEveryProjectGetsAVerdict: the record is per project, so a
// project the gate looked at and stood down on has to come back — an
// absent project would render as one the heartbeat has never heard of.
func TestHeartbeatEveryProjectGetsAVerdict(t *testing.T) {
	root := quietFixture(t)
	decisions := dueDecisions(t, newHeartbeatGate(root), testTick)
	if len(decisions) == 0 {
		t.Fatal("no verdicts at all on a registered board")
	}
	for _, d := range decisions {
		if d.Reason == "" {
			t.Errorf("project %s came back with no reason", d.Project)
		}
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

// sweptOverAMidSweepOperatorCommit stages the wedge: a gate that has
// dispatched a sweep, an operator commit landing during that sweep's
// agent turn — after its board read, so no survey ever saw it — and the
// survey's own open/close commits on top. The caller says how the sweep
// ended.
//
// Returns the gate, ready for the tick after the sweep.
func sweptOverAMidSweepOperatorCommit(t *testing.T, root string, clean bool) *heartbeatGate {
	t.Helper()
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	g := newHeartbeatGate(root)
	dueProjects(t, g) // seeds the cursor

	journalCommit(t, root, "moe", "machine: another merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want the delta sweep", got)
	}

	journalCommit(t, root, "moe", "chain: edit", "" /*operator*/)
	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", clean)
	return g
}

// TestHeartbeatSweepsACommitThatLandedMidSweep is the bug. A survey's
// turn lasts minutes, so an operator commit can land after its board read
// and before its close — below the tip Swept records, and swallowed by
// both cursors. The moved leg never fires for it and the surveyed cursor
// stands the parked leg down too, so a real journal move the machine
// misreads as its own goes nowhere, silently.
func TestHeartbeatSweepsACommitThatLandedMidSweep(t *testing.T) {
	root := quietFixture(t)
	g := sweptOverAMidSweepOperatorCommit(t, root, true /*clean*/)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want [moe] — a commit no survey saw must still get a sweep", got)
	}

	// The follow-up sweep's own range holds only its own commits, so it
	// advances both cursors and the board goes quiet. Convergence is the
	// half that keeps this from being a sweep-every-tick loop.
	journalCommit(t, root, "moe", "open: pulse-2", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-2", "MoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after the follow-up sweep saw it, want none", got)
	}
}

// TestHeartbeatSweepsACommitThatLandedMidFailedSweep: the refusal applies
// to a sweep that died too. Recording the tip on failure is deliberate —
// a dead sweep still wrote its run-open commit and a cursor left behind it
// would sweep straight back into the same wall unpaced — but that
// rationale only covers the sweep's *own* commits. An operator commit in
// the range was never anyone's to swallow, and the failure backoff still
// paces the spawn.
func TestHeartbeatSweepsACommitThatLandedMidFailedSweep(t *testing.T) {
	root := quietFixture(t)
	g := sweptOverAMidSweepOperatorCommit(t, root, false /*died*/)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after a sweep died over an operator commit, want [moe]", got)
	}
}

// sweptOverARideItKicked stages the autonomous path: a dispatched sweep
// whose survey kicked a ride, the ride's own commits landing inside the
// pulse child's lifetime, and the survey's open/close on top. The caller
// says how the sweep ended.
//
// Trailer shapes are the real ones — a stage turn and a push's merge,
// each machine-marked and workflow-stamped — because the machine mark is
// exactly what makes this case invisible to the operator-commit refusal.
func sweptOverARideItKicked(t *testing.T, root string, clean bool) *heartbeatGate {
	t.Helper()
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	g := newHeartbeatGate(root)
	dueProjects(t, g) // seeds the cursor

	journalCommit(t, root, "moe", "machine: another merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want the delta sweep", got)
	}

	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "work: update code", "MoE-Workflow: sdlc\nMoE-Consent: dynamic")
	journalCommit(t, root, "moe", "push: moe/a-ride merged", "MoE-Workflow: sdlc\nMoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", clean)
	return g
}

// TestHeartbeatSweepsWorkARideLandedMidSweep is the autonomous half of
// the same wedge. A dynamic sweep runs the rides it kicks inside the
// pulse child, so their commits sit below the tip the exit records — and
// the survey read the *pre-ride* board, so nothing has seen what they
// landed. Absorbing them is what stopped pulse_kick's walk at its first
// generation and left the board waiting on a human.
func TestHeartbeatSweepsWorkARideLandedMidSweep(t *testing.T) {
	root := quietFixture(t)
	g := sweptOverARideItKicked(t, root, true /*clean*/)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want [moe] — work a sweep's own ride landed must still get a survey", got)
	}

	// Generation 2: the follow-up sweep kicks nothing, so its window holds
	// only its own commits and the walk ends here. Convergence is the half
	// that keeps this from being a sweep-every-tick loop.
	journalCommit(t, root, "moe", "open: pulse-2", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-2", "MoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after a sweep that kicked nothing, want none", got)
	}
}

// TestHeartbeatSweepsWorkARideLandedMidFailedSweep: same terms as the
// operator case. The failure backoff — serve's, not the gate's — is what
// paces the respawn, so refusing the tips cursor here costs a cool-off,
// not a loop.
func TestHeartbeatSweepsWorkARideLandedMidFailedSweep(t *testing.T) {
	root := quietFixture(t)
	g := sweptOverARideItKicked(t, root, false /*died*/)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after a sweep died over a ride it kicked, want [moe]", got)
	}
}

// TestHeartbeatSweptMovesTheCursorOverItsOwnBookkeeping is the other
// side of the ride refusal, and the one that keeps it from costing every
// sweep a redundant follow-up. A survey that kicked nothing still writes
// a pile of commits — the run it minted for a chore, the ideas it
// harvested, the grooms it stamped on them, the pointer bump at the end.
// None of them is work landing under the survey's feet, and the board
// must go quiet.
func TestHeartbeatSweptMovesTheCursorOverItsOwnBookkeeping(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")
	g := newHeartbeatGate(root)
	dueProjects(t, g)

	journalCommit(t, root, "moe", "machine: another merge", "MoE-Consent: dynamic")
	if got := dueProjects(t, g); len(got) != 1 {
		t.Fatalf("due = %v, want the delta sweep", got)
	}

	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "work: update pulse", "MoE-Workflow: pulse\nMoE-Consent: dynamic")
	journalCommit(t, root, "moe", "Open run moe/a-chore", "MoE-Spawned-By: moe/pulse-1")
	journalCommit(t, root, "moe", "Open run moe/an-idea", "MoE-From-Run: moe/pulse-1\nMoE-Consent: dynamic")
	journalCommit(t, root, "moe", "Tag idea moe/an-idea (sdlc)", "MoE-Workflow: idea\nMoE-Consent: dynamic")
	journalCommit(t, root, "moe", "sync: bump project pointers", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Workflow: pulse\nMoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after a sweep that only did its own bookkeeping, want none", got)
	}
}

// TestRideAuthored pins the trailer census the refusal reads. Every row
// is a real commit shape from the journal: the top three are how a ride
// lands work, the rest are a sweep describing itself.
func TestRideAuthored(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"stage turn", "work: update code\n\nMoE-Workflow: sdlc\nMoE-Consent: dynamic\n", true},
		{"push merge", "push: moe/a-ride merged\n\nMoE-Workflow: sdlc\nMoE-Merged: 1\n", true},
		{"twin reflect", "work: update architecture\n\nMoE-Workflow: twin\nMoE-Consent: dynamic\n", true},
		{"the sweep's own close", "Close pulse run moe/pulse-1\n\nMoE-Workflow: pulse\n", false},
		{"a groom", "Tag idea moe/an-idea (sdlc)\n\nMoE-Workflow: idea\n", false},
		{"an intent edit", "work: update intent\n\nMoE-Workflow: intent\n", false},
		{"a gate mint", "Open run moe/a-chore\n\nMoE-Spawned-By: moe/pulse-1\n", false},
		{"a harvest mint", "Open run moe/an-idea\n\nMoE-From-Run: moe/pulse-1\n", false},
		{"a sync bump", "sync: bump project pointers\n\nMoE-Consent: dynamic\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rideAuthored(tc.body); got != tc.want {
				t.Errorf("rideAuthored = %v, want %v:\n%s", got, tc.want, tc.body)
			}
		})
	}
}

// TestHeartbeatSettlesAfterAnOperatorTriggeredSweep guards the one place
// the range base decides between a fix and a runaway. The commits that
// *triggered* a sweep landed before it looked, so the survey saw them —
// walking from the tips cursor would find the triggering operator commit
// in its own exit range, refuse to advance, re-offer, re-sweep and refuse
// again, every tick forever. The base has to be the tip the gate
// dispatched on.
func TestHeartbeatSettlesAfterAnOperatorTriggeredSweep(t *testing.T) {
	root := quietFixture(t)
	g := newHeartbeatGate(root)
	dueProjectsPastTheWindow(t, g) // seeds the cursor

	journalCommit(t, root, "moe", "chain: edit", "" /*operator*/)
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want [moe] — the operator's commit moved the journal", got)
	}

	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after the sweep that this commit triggered, want none", got)
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
	writeDeadClaim(t, s, true /*machine*/)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v after the reap freed the thread, want [moe] in the same tick", got)
	}
	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Error("session branch survived the reap")
	}
}

// seedPulseRun stamps a pulse run and the journal commit that stands
// for its activity: run.json on disk for the scan, plus a project-scoped
// commit carrying the MoE-Project / MoE-Run trailers the journal index
// keys LastActivity by. ago pushes that commit back into the past, which
// is how a test says "this pulse ran, and then things happened".
//
// Returns nothing — every caller wants the board, not the run.
func seedPulseRun(t *testing.T, root, projectID, id, status string, ago time.Duration) {
	t.Helper()
	seedRun(t, root, projectID, id, pulseWorkflow, status, time.Now().Local(), nil)
	journalCommit(t, root, projectID, "pulse: "+id,
		"MoE-Consent: dynamic\nMoE-Project: "+projectID+"\nMoE-Run: "+id)
	if ago > 0 {
		backdateHead(t, root, ago)
	}
}

// TestHeartbeatSweepsWorkThatLandedWhileServeWasDown is the bug. The
// moved leg is edge-triggered on an in-memory cursor, and seeding that
// cursor lazily makes whatever landed last the permanent baseline — so
// on a board with nothing parked, a restart on top of a merge had no leg
// left to fire and went quiet forever, not for one tick. moe is the
// project that lives this: reinstalling the binary restarts serve right
// on top of the merge that just landed.
func TestHeartbeatSweepsWorkThatLandedWhileServeWasDown(t *testing.T) {
	root := quietFixture(t)
	seedPulseRun(t, root, "moe", "pulse-1", run.StatusClosed, 30*time.Minute)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")

	decisions := dueDecisions(t, newHeartbeatGate(root), 0)
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on a restart with a merge the last sweep never saw, want [moe]", got)
	}
	if got, want := reasonFor(decisions, "moe"), "work landed since the last sweep"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// TestHeartbeatIsQuietOnARestartOverASweptBoard pins the convergence the
// leg rests on. The pulse run's close commit *is* the project tip, and
// both sides of the comparison read that same commit's committer time —
// so they compare equal and a restart on a cleanly-swept board sweeps
// nothing. Without the equality this leg would fire on every restart,
// forever, which is a worse bug than the one it fixes.
func TestHeartbeatIsQuietOnARestartOverASweptBoard(t *testing.T) {
	root := quietFixture(t)
	seedPulseRun(t, root, "moe", "pulse-1", run.StatusClosed, 0 /*at the tip*/)

	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v on a restart over a board the last sweep closed clean, want none", got)
	}
}

// TestHeartbeatIsQuietForAProjectThatWasNeverSwept is the storm guard.
// A project with no pulse history has a zero bound, which would make the
// comparison trivially true and sweep every never-surveyed project at
// once on every restart — most of the register, today. "Never swept" is
// not the claim "has unswept work", so the leg abstains and the project
// stays reachable through the moved and parked legs exactly as before.
func TestHeartbeatIsQuietForAProjectThatWasNeverSwept(t *testing.T) {
	root := quietFixture(t)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")

	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v for a project no sweep has ever run against, want none", got)
	}
}

// TestHeartbeatDeltaDoesNotRetriggerAFailedSweep: a sweep that died
// leaves its run open with its own last commit at the project tip, which
// is also that run's newest activity — equal, so the delta leg abstains
// rather than reading its own wreckage as unswept work and looping on it
// every tick. Pacing the retry stays the failure backoff's job, through
// the parked leg, which is what it was written for.
func TestHeartbeatDeltaDoesNotRetriggerAFailedSweep(t *testing.T) {
	root := quietFixture(t)
	seedPulseRun(t, root, "moe", "pulse-1", run.StatusClosed, 30*time.Minute)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")

	g := newHeartbeatGate(root)
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the restart tick, want [moe] — the merge is unswept", got)
	}

	// The sweep opens its run, gets partway, and dies.
	seedPulseRun(t, root, "moe", "pulse-2", run.StatusInProgress, 0 /*at the tip*/)
	g.Swept("moe", false /*died*/)

	for tick := range 3 {
		if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
			t.Fatalf("due = %v on tick %d after a sweep died, want none from this leg", got, tick+1)
		}
	}
}

// TestHeartbeatSurveyedShortCircuitsTheDelta is the ordering pin. While
// serve is alive the surveyed cursor is the authoritative answer — a
// clean sweep looked at this exact tip — and the delta leg exists only
// for the case where that in-memory answer is missing. Reaching it on a
// live serve would re-sweep a board the machine just finished with,
// because the sweep's own close commit lands after the pulse activity it
// is compared against on the *next* tick's read.
func TestHeartbeatSurveyedShortCircuitsTheDelta(t *testing.T) {
	root := quietFixture(t)
	seedPulseRun(t, root, "moe", "pulse-1", run.StatusClosed, 30*time.Minute)
	journalCommit(t, root, "moe", "machine: a merge", "MoE-Consent: dynamic")

	g := newHeartbeatGate(root)
	if got := dueProjectsPastTheWindow(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the restart tick, want [moe] — the merge is unswept", got)
	}
	g.Swept("moe", true /*clean*/)

	decisions := dueDecisions(t, g, 0)
	if got := sweepIDs(decisions); len(got) != 0 {
		t.Fatalf("due = %v after a clean sweep of the delta, want none", got)
	}
	if got, want := reasonFor(decisions, "moe"), "a sweep already surveyed the current tip"; got != want {
		t.Errorf("reason = %q, want %q — the surveyed cursor answers before the delta leg", got, want)
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
	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 1 {
		t.Errorf("due = %v once the quiet window passed, want [moe]", got)
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

	if got := dueProjectsPastTheWindow(t, newHeartbeatGate(root)); len(got) != 1 {
		t.Errorf("due = %v once the quiet window passed, want [moe]", got)
	}
}

// TestHeartbeatStandsDownForALiveSession: a ride mid-hop or an operator
// sitting in a stage means somebody is already inside the project; their
// commits move the journal tip when they land, so the next tick's moved
// leg picks the project back up.
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

// TestHeartbeatSweepsPastAStrandedOperatorSession is the wedge this
// change closes, in its everyday shape: the operator takes the widget's
// advertised Ctrl-C out of `moe pulse new`, and the branch, worktree and
// operator-marked claim all survive the process. The reap won't clear it
// — a human's session may only ever be surfaced — so counting it as
// occupancy stands this project's heartbeat down forever, silently,
// while the operator has been told only that the run stays open for
// review.
//
// Once the claim is provably dead it stops vouching for occupancy. What
// it must *not* do is stop existing: `moe session resolve` / `abandon`
// still expect the branch and the record exactly where they are.
func TestHeartbeatSweepsPastAStrandedOperatorSession(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "pulse-ctrl-c", pulseWorkflow, run.StatusInProgress, time.Now().Local(), nil)
	s, err := session.Open(root, "moe", "pulse-ctrl-c", pulseDoc)
	if err != nil {
		t.Fatal(err)
	}
	writeDeadClaim(t, s, false /*operator*/)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v, want [moe] — a Ctrl-C'd pulse must not wedge the heartbeat forever", got)
	}
	if !git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Error("the stranded branch was cleared; an operator's session may be ignored, never touched")
	}
	if _, ok := session.ReadClaim(root, "moe", "pulse-ctrl-c", pulseDoc); !ok {
		t.Error("the stranded claim was cleared; resolve/abandon still need it")
	}
}

// TestHeartbeatStandsDownForAHeldOperatorSession is the direction where a
// wrong answer costs something: an operator sitting in a live stage,
// beating. Dead means provably dead, and a live pid is not it.
func TestHeartbeatStandsDownForAHeldOperatorSession(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "pulse-in-flight", pulseWorkflow, run.StatusInProgress, time.Now().Local(), nil)
	s, err := session.Open(root, "moe", "pulse-in-flight", pulseDoc)
	if err != nil {
		t.Fatal(err)
	}
	release, err := session.Hold(s, false /*operator*/)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v with the operator inside the project, want none", got)
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

// strandFixture is the 2026-08-12 seed's exact shape: an
// operator-promoted sketch sitting at design with nothing but its seed,
// and a machine-minted run — settled by construction — chained behind
// it. The floor holds the head correctly; the question this fixture
// asks is whether anything ever looks at what queued up behind it.
//
// Returns the stranded member's run id.
func strandFixture(t *testing.T, root string) string {
	t.Helper()
	// Mint before seeding: seedRun leaves its run.json untracked, and the
	// minter commits into the same tree.
	stranded := groomFixture(t, root, "fix-a")["fix-a"]
	seedRun(t, root, "moe", "sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# A promoted idea\n\nseed\n"})
	chainEdgeCommit(t, root, "moe/sketch", "moe/"+stranded)
	return stranded
}

// TestParkedLegLooksPastAHeldHead is the bug. Both runs behind the seed
// on 2026-08-12 cleared the floor on their own, and the parked leg —
// asking the kick's root-only question — read the board as empty, so no
// tick ever fired and the documented "parks for the next pulse to
// place" recovery never got a pulse to run in.
func TestParkedLegLooksPastAHeldHead(t *testing.T) {
	root := quietFixture(t)
	want := "moe/" + strandFixture(t, root)

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Errorf("parkedKickableThread = %q, want %q — settled work queued behind a held head is not an empty board", got, want)
	}
}

// TestParkedLegHoldsAnOperatorStagedBatch is the fence the member walk
// is most at risk of breaking. A hand-minted `chain` head is the
// operator composing a batch, and the runs under it are machine-minted
// and therefore settled — the exact shape that would otherwise summon a
// sweep onto work somebody is still arranging.
func TestParkedLegHoldsAnOperatorStagedBatch(t *testing.T) {
	root := quietFixture(t)
	head, err := mintChainRun(root, "moe", "operator-topic", "" /*spawnedBy*/, "", io.Discard, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	chainEdgeCommit(t, root, "moe/"+head.ID, "moe/"+groomFixture(t, root, "fix-a")["fix-a"])

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want \"\" — the head is the operator's staging fence", got)
	}
}

// TestParkedLegSkipsAnOccupiedStrandedMember: the member walk owes the
// floor's whole question, not just the design half. Somebody sitting in
// the stranded run is the same occupancy that holds a root.
func TestParkedLegSkipsAnOccupiedStrandedMember(t *testing.T) {
	root := quietFixture(t)
	stranded := strandFixture(t, root)
	if _, err := session.Open(root, "moe", stranded, "design"); err != nil {
		t.Fatal(err)
	}

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want \"\" — somebody is inside the stranded run", got)
	}
}

// TestParkedLegStillReturnsAnAdmittingRoot pins the path that did not
// change: a root that clears the floor itself is still what the leg
// names, and the walk below it never runs.
func TestParkedLegStillReturnsAnAdmittingRoot(t *testing.T) {
	root := quietFixture(t)
	want := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Errorf("parkedKickableThread = %q, want %q", got, want)
	}
}

// TestParkedLegHoldsALoneHeldRun: a seed-only run with nothing behind
// it strands nobody. Offering it would spend an agent turn on a board
// whose only content is a door the operator has not opened.
func TestParkedLegHoldsALoneHeldRun(t *testing.T) {
	root := quietFixture(t)
	seedRun(t, root, "moe", "sketch", "sdlc", run.StatusInProgress, time.Now().Local(),
		map[string]string{"design": "# A promoted idea\n\nseed\n"})

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want \"\" — a lone seed-only run is the operator's", got)
	}
}

// parkTaggedIdea stamps a tagged idea and pushes the tag commit back out
// of the quiet window — the operator tagged it, a tick has passed, and
// the machine's turn to look has come.
func parkTaggedIdea(t *testing.T, root, slug, promoteTo string) string {
	t.Helper()
	md, err := run.New(root, "moe", run.Options{
		ID: slug, Workflow: "idea", PromoteTo: promoteTo,
		SeedDocs: map[string]string{"idea": "# " + slug + "\n\nWorth doing.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	backdateHead(t, root, time.Hour)
	return "moe/" + md.ID
}

// TestParkedLegSeesATaggedIdea is the other half of the bug. A tag is
// the licence to start, and it was invisible to the one leg that makes a
// quiet board warrant a sweep — so a tag stamped while serve was down
// bought nothing, and a tag stamped while it was up bought exactly one
// sweep it could not afford to lose.
func TestParkedLegSeesATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	want := parkTaggedIdea(t, root, "cleanup-foo", "sdlc")

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Errorf("parkedKickableThread = %q, want %q — a tagged idea is licensed work, not an empty board", got, want)
	}
}

// TestParkedLegSeesATwinTaggedIdea: a `(twin)` tag resolves through the
// reflect nomination rather than a promotion, and that is still motion.
func TestParkedLegSeesATwinTaggedIdea(t *testing.T) {
	root := quietFixture(t)
	want := parkTaggedIdea(t, root, "boundary-moved", "twin")

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Errorf("parkedKickableThread = %q, want %q", got, want)
	}
}

// TestParkedLegHoldsAnUntaggedIdea is the fence the new leg is most at
// risk of breaking. Untagged means human: an idea nobody has licensed is
// the operator's inbox, and summoning a sweep onto it is the whole thing
// the tag exists to gate.
func TestParkedLegHoldsAnUntaggedIdea(t *testing.T) {
	root := quietFixture(t)
	parkTaggedIdea(t, root, "needs-triage", "" /*untagged*/)

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want \"\" — untagged means human", got)
	}
}

// TestParkedLegHoldsAPromotedIdea: promotion is terminal for the idea,
// and the run it promoted into answers for the work from then on.
func TestParkedLegHoldsAPromotedIdea(t *testing.T) {
	root := quietFixture(t)
	parkTaggedIdea(t, root, "cleanup-foo", "sdlc")
	md, err := run.Load(root, "moe", "cleanup-foo")
	if err != nil {
		t.Fatal(err)
	}
	md.Status = run.StatusPromoted
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != "" {
		t.Errorf("parkedKickableThread = %q, want \"\" — a promoted idea is done being an idea", got)
	}
}

// TestParkedLegPrefersAParkedRunToATaggedIdea pins the walk order. The
// returned key only proves the board is not empty and feeds a display
// string — every real ordering call is the survey's — but runs kept
// today's claim on it deliberately, so the reason strings a settled
// board produces are unchanged by this leg existing.
func TestParkedLegPrefersAParkedRunToATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	want := "moe/" + groomFixture(t, root, "fix-a")["fix-a"]
	parkTaggedIdea(t, root, "cleanup-foo", "sdlc")

	if got := parkedKickableThread(root, mustPulseScan(t, root), "moe", false); got != want {
		t.Errorf("parkedKickableThread = %q, want the parked run %q", got, want)
	}
}

// TestHeartbeatSweepsForATaggedIdea is the operator's story end to end:
// a board with nothing on it but a tag, no journal delta to ride on, and
// a serve that was armed the whole time or has just restarted — either
// way the tick fires and says why in the operator's own terms.
func TestHeartbeatSweepsForATaggedIdea(t *testing.T) {
	root := quietFixture(t)
	idea := parkTaggedIdea(t, root, "cleanup-foo", "sdlc")
	g := newHeartbeatGate(root)

	decisions := dueDecisions(t, g, testTick)
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v, want [moe] — a tagged idea is parked", got)
	}
	if got, want := reasonFor(decisions, "moe"), "a tagged idea is parked at "+idea; got != want {
		t.Errorf("reason = %q, want %q — a tag is a licence nobody has spent, not settled work", got, want)
	}
}

// TestHeartbeatStopsOfferingATaggedIdeaASweepDeclined: the new leg owes
// the same contract every other parked shape has. A clean sweep that saw
// the idea and left it parked answered the question, and re-asking it
// every tick costs an agent turn forever.
func TestHeartbeatStopsOfferingATaggedIdeaASweepDeclined(t *testing.T) {
	root := quietFixture(t)
	parkTaggedIdea(t, root, "cleanup-foo", "sdlc")
	g := newHeartbeatGate(root)
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the first look, want [moe]", got)
	}
	g.Swept("moe", true /*clean*/)

	for tick := range 3 {
		if got := dueProjects(t, g); len(got) != 0 {
			t.Fatalf("due = %v on tick %d after a clean sweep declined this idea, want none", got, tick+1)
		}
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
	writeDeadClaim(t, s, true /*machine*/)

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

// writeDeadClaim stages the liveness record a session leaves behind when
// its process dies mid-turn: owned by a same-host pid that has exited,
// with a heartbeat old enough to have stopped vouching. Both dead
// signals are needed, so both are here. machine is who was driving — a
// walk that died, or the operator's own Ctrl-C.
//
// The pid comes from a child run to completion rather than an invented
// number — it is the only way to name a pid this host is genuinely
// finished with.
func writeDeadClaim(t *testing.T, s *session.Session, machine bool) {
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
		Machine:     machine,
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

// TestHeartbeatGCFreesABoardAClaimlessBranchHeldForever is the wedge
// this run closes, in the exact shape found five times on the real
// board: a run that is over with one stage's branch still on it and no
// claim to probe. The reap above can't help — there is nothing to prove
// dead — and a branch that can't be probed reads as occupancy, so the
// project stands down every tick until a human remembers `moe session
// gc`.
//
// Both halves are asserted because both are load-bearing: the branch
// goes, *and* the freed board is re-offered in the same tick. Nothing
// here moves the journal, so the surveyed cursor is what would
// otherwise keep declining a board the reap just changed.
func TestHeartbeatGCFreesABoardAClaimlessBranchHeldForever(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	g := sweptOnceOverParkedWork(t, root, true /*clean*/)
	if got := dueProjects(t, g); len(got) != 0 {
		t.Fatalf("due = %v right after the sweep, want none", got)
	}

	seedRun(t, root, "moe", "already-merged", "sdlc", run.StatusMerged, time.Now().Local(), nil)
	s, err := session.Open(root, "moe", "already-merged", "code")
	if err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	decisions := g.Due(testTick, &log)

	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Fatalf("a claimless branch on a merged run survived the tick\n%s", log.String())
	}
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v, want [moe] — the freed board must be re-offered in the same tick\nlog:\n%s",
			got, log.String())
	}
}

// TestHeartbeatGCClearsARefWithNoWorktree is the other door residue
// comes in by, and today's shape: an ending that deleted the worktree
// and left the ref. `session.Open` refuses to reuse it ("abandoned
// close?"), so the stage rerun the branch belongs to can never start
// until somebody clears it by hand.
func TestHeartbeatGCClearsARefWithNoWorktree(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "closed-by-hand", "sdlc", run.StatusClosed, time.Now().Local(), nil)
	branch := session.BranchName("moe", "closed-by-hand", "design")
	gittest.Run(t, root, "branch", branch)

	var log bytes.Buffer
	newHeartbeatGate(root).Due(testTick, &log)

	if git.HasRef(root, "refs/heads/"+branch) {
		t.Fatalf("the orphan ref survived the tick\n%s", log.String())
	}
	if _, err := session.Open(root, "moe", "closed-by-hand", "design"); err != nil {
		t.Errorf("the stage rerun is still refused after the tick: %v", err)
	}
}

// TestHeartbeatGCHoldsALiveClaimOnATerminalRun is the direction where a
// wrong answer destroys something. `moe <wf> close` has no open-session
// guard, so an operator sitting in a design pane while close is typed
// in another produces rule 1's exact shape — terminal status, branch
// present, claimant alive and beating. Reaping that removes the
// worktree with --force and takes the uncommitted work with it, which
// was tolerable for a hand verb and is not for a twenty-minute tick.
func TestHeartbeatGCHoldsALiveClaimOnATerminalRun(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "closed-underneath", "sdlc", run.StatusClosed, time.Now().Local(), nil)
	s, err := session.Open(root, "moe", "closed-underneath", "design")
	if err != nil {
		t.Fatal(err)
	}
	release, err := session.Hold(s, false /*operator*/)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	var log bytes.Buffer
	decisions := newHeartbeatGate(root).Due(testTick, &log)

	if !git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Fatalf("a live session was reaped out from under its claimant\n%s", log.String())
	}
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Errorf("the live session's worktree went with it: %v", err)
	}
	if got := sweepIDs(decisions); len(got) != 0 {
		t.Errorf("due = %v with somebody inside the project, want none", got)
	}
}

// TestHeartbeatGCReapsATerminalRunWithADeadClaim is the widened edge:
// rule 1 ignores claims entirely today, and the hold narrows it to
// *live* ones. A claim that is provably dead on a run that is over
// still goes — and this one is operator-marked, so the reap above would
// never touch it.
func TestHeartbeatGCReapsATerminalRunWithADeadClaim(t *testing.T) {
	root := quietFixture(t)
	groomFixture(t, root, "fix-a")
	seedRun(t, root, "moe", "merged-and-dead", "sdlc", run.StatusMerged, time.Now().Local(), nil)
	s, err := session.Open(root, "moe", "merged-and-dead", "code")
	if err != nil {
		t.Fatal(err)
	}
	writeDeadClaim(t, s, false /*operator*/)

	var log bytes.Buffer
	newHeartbeatGate(root).Due(testTick, &log)

	if git.HasRef(root, "refs/heads/"+s.Branch) {
		t.Errorf("a dead claim on a finished run kept the branch alive\n%s", log.String())
	}
}
