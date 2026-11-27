package cli

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// pulseNotesGateFixture is a quiet bureaucracy with the root project
// registered, so the gate answers for _cabinet.
func pulseNotesGateFixture(t *testing.T) string {
	t.Helper()
	root := parallelProjectFixture(t)
	if err := project.EnsureRoot(root, ""); err != nil {
		t.Fatal(err)
	}
	backdateHead(t, root, time.Hour)
	return root
}

func addPulseNote(t *testing.T, root, text string) {
	t.Helper()
	if _, err := input.AddPulse(root, text); err != nil {
		t.Fatal(err)
	}
}

// A pulse note is an operator commit under projects/_cabinet, so an
// armed serve's next tick sweeps on the moved leg with no gate change.
func TestHeartbeatSweepsOnAPulseNote(t *testing.T) {
	t.Parallel()
	root := pulseNotesGateFixture(t)
	g := newHeartbeatGate(root)
	dueProjectsPastTheWindow(t, g) // seeds the cursors

	addPulseNote(t, root, "do the lightpool fix next")
	decisions := dueDecisions(t, g, 0)
	if !containsID(sweepIDs(decisions), project.RootID) {
		t.Fatalf("due = %v after a pulse note, want %s", sweepIDs(decisions), project.RootID)
	}
	if got := reasonFor(decisions, project.RootID); got != "the journal moved" {
		t.Errorf("reason = %q, want the moved leg", got)
	}
}

// A serve restarted after the note has no cursor to move; the unswept
// leg catches it because the note is newer than the last pulse.
func TestHeartbeatSweepsAPulseNoteLeftWhileServeWasDown(t *testing.T) {
	t.Parallel()
	root := pulseNotesGateFixture(t)
	seedPulseRun(t, root, project.RootID, "pulse-1", run.StatusClosed, 30*time.Minute)
	addPulseNote(t, root, "bump Go everywhere")

	decisions := dueDecisions(t, newHeartbeatGate(root), 0)
	if got := reasonFor(decisions, project.RootID); got != "work landed since the last sweep" {
		t.Fatalf("reason = %q, want the unswept leg", got)
	}
}

// The sweep's own delivery stamp is machine bookkeeping: after a clean
// sweep that read the note, the board reads surveyed and the next tick
// stays quiet rather than sweeping again for the stamp.
func TestHeartbeatPulseNoteStampEarnsNoFollowUp(t *testing.T) {
	t.Parallel()
	root := pulseNotesGateFixture(t)
	g := newHeartbeatGate(root)
	dueProjectsPastTheWindow(t, g)

	addPulseNote(t, root, "do the lightpool fix next")
	if got := dueProjectsPastTheWindow(t, g); !containsID(got, project.RootID) {
		t.Fatalf("due = %v, want the note's sweep", got)
	}
	g.Dispatching()
	journalCommit(t, root, project.RootID, "x", "MoE-Kind: open\nMoE-Consent: dynamic\nMoE-Project: _cabinet")
	journalCommit(t, root, project.RootID, "work: update pulse", "MoE-Workflow: pulse\nMoE-Consent: dynamic\nMoE-Project: _cabinet")
	if err := input.MarkPulseDelivered(root, project.RootID+"/pulse-1", []int{1}, "dynamic"); err != nil {
		t.Fatal(err)
	}
	journalCommit(t, root, project.RootID, "Close pulse run _cabinet/pulse-1", "MoE-Workflow: pulse\nMoE-Consent: dynamic\nMoE-Project: _cabinet")
	g.Swept(project.RootID, true)

	if got := dueProjectsPastTheWindow(t, g); len(got) != 0 {
		t.Errorf("due = %v after a sweep that read and stamped the note, want none", got)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
