package cli

import (
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// The gate's two chore legs. Both answer the same gap: a chore coming
// due is a pure clock event on a quiet journal, so neither the moved leg
// nor the parked leg ever sees it, and before these legs a due chore sat
// in the backlog until a human clicked the web's open button.

// seedChoreDef writes a chore definition, commits it, and pushes the
// commit back out of the quiet window — a chore the operator registered
// a while ago and has since taken their hands off.
func seedChoreDef(t *testing.T, root, name, choreJSON string, ago time.Duration) {
	t.Helper()
	writeChoreDef(t, root, name, choreJSON, "do the thing\n")
	if err := git.Run(root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := git.Run(root, "commit", "-m", "register chore "+name); err != nil {
		t.Fatal(err)
	}
	backdateHead(t, root, ago)
}

// completeChoreAt stamps a MoE-Chore-Skipped marker for the chore, dated
// ago in the past. chore.Evaluate folds a skip into LastCompleted as of
// its commit time, so this is the cheapest way for a test to say "this
// chore was satisfied then" without minting and closing a run.
func completeChoreAt(t *testing.T, root, projectID, name string, ago time.Duration) {
	t.Helper()
	journalCommit(t, root, projectID, "chore: skip "+name,
		"MoE-Chore-Skipped: "+projectID+"/"+name)
	backdateHead(t, root, ago)
}

// TestHeartbeatSweepsForADueChore is the gap itself: nothing has landed,
// nothing is parked, and the only thing that changed is the clock.
func TestHeartbeatSweepsForADueChore(t *testing.T) {
	root := quietFixture(t)
	seedChoreDef(t, root, "readme-refresh", `{"cadence":"168h"}`, time.Hour)

	decisions := dueDecisions(t, newHeartbeatGate(root), testTick)
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v with a chore due on a quiet board, want [moe]", got)
	}
	if got, want := reasonFor(decisions, "moe"), "a chore is due"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// TestHeartbeatChoreLegOutlivesTheSurveyedCursor pins the placement. The
// surveyed early-return means "a sweep already looked at this exact
// board" — and that is precisely the state a quiet project sits in when
// a chore's clock elapses, so a leg consulted after it would never fire
// at all.
func TestHeartbeatChoreLegOutlivesTheSurveyedCursor(t *testing.T) {
	root := quietFixture(t)
	seedChoreDef(t, root, "readme-refresh", `{"cadence":"168h"}`, time.Hour)
	g := newHeartbeatGate(root)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the first look, want [moe]", got)
	}
	// The sweep runs and closes clean, but nothing satisfied the chore —
	// its run failed to open, say. The board it surveyed is the board the
	// chore is still due on.
	journalCommit(t, root, "moe", "open: pulse-1", "MoE-Consent: dynamic")
	journalCommit(t, root, "moe", "close: pulse-1", "MoE-Consent: dynamic")
	g.Swept("moe", true)

	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Errorf("due = %v on the tick after a clean sweep, want [moe] — the chore is still due", got)
	}
}

// TestHeartbeatChoreLegQuietsOnceTheChoreHasARun is the convergence the
// leg rests on: the sweep it fires auto-opens the chore's run, and
// chore.Evaluate's open-run guard makes the chore not-due from there. One
// firing, not one per tick forever.
func TestHeartbeatChoreLegQuietsOnceTheChoreHasARun(t *testing.T) {
	root := quietFixture(t)
	seedChoreDef(t, root, "readme-refresh", `{"cadence":"168h"}`, time.Hour)
	g := newHeartbeatGate(root)
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the first look, want [moe]", got)
	}

	// What the sweep does: opens the chore's run and closes its own.
	seedRun(t, root, "moe", "readme-refresh-2026-08-22", "sdlc", run.StatusInProgress, time.Now().Local(), nil)
	journalCommit(t, root, "moe", "open: readme-refresh-2026-08-22",
		"MoE-Consent: dynamic\nMoE-Project: moe\nMoE-Run: readme-refresh-2026-08-22\nMoE-Chore: moe/readme-refresh")
	g.Swept("moe", true)

	if got := dueProjects(t, g); len(got) != 0 {
		t.Errorf("due = %v once the chore has an open run, want none", got)
	}
}

// TestHeartbeatIsQuietForACoolingChore: the cooldown is the operator's
// pacing, and the leg reads chore.Evaluate's answer rather than
// re-deriving one of its own.
func TestHeartbeatIsQuietForACoolingChore(t *testing.T) {
	root := quietFixture(t)
	seedChoreDef(t, root, "readme-refresh", `{"cadence":"168h","cooldown":"168h"}`, time.Hour)
	completeChoreAt(t, root, "moe", "readme-refresh", 30*time.Minute)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v for a chore inside its cooldown, want none", got)
	}
}

// TestHeartbeatPausedCapsTheChoreClock: paused is the operator's
// standing cap and sits ahead of every read, chore legs included. A
// clock that outranked it would make the brake a suggestion.
func TestHeartbeatPausedCapsTheChoreClock(t *testing.T) {
	root := quietFixture(t)
	seedChoreDef(t, root, "readme-refresh", `{"cadence":"168h"}`, time.Hour)
	if err := project.SetMode(root, "moe", project.ModePaused); err != nil {
		t.Fatal(err)
	}

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v for a paused project with a due chore, want none", got)
	}
}

// judgedBoard seeds a judged chore with the given cooldown, satisfied
// two hours ago, and lands the project's most recent pulse run ninety
// minutes ago — on top of everything else. The pulse being the newest
// commit is deliberate: it keeps the gate's "work landed since the last
// sweep" leg abstaining, so these tests are about the judged leg alone.
//
// With a one-hour cooldown that is the board where exactly one probe is
// owed: eligible since an hour ago, and no pulse has looked since.
func judgedBoard(t *testing.T, cooldown string, seedBeforeThePulse func(root string)) string {
	t.Helper()
	root := quietFixture(t)
	seedChoreDef(t, root, "lore-sweep", `{"when":"lore has drifted"`+cooldown+`}`, 3*time.Hour)
	completeChoreAt(t, root, "moe", "lore-sweep", 2*time.Hour)
	if seedBeforeThePulse != nil {
		seedBeforeThePulse(root)
	}
	seedPulseRun(t, root, "moe", "pulse-1", run.StatusClosed, 90*time.Minute)
	return root
}

// TestHeartbeatProbesAJudgedChoreOnceEligible: a judged chore is never
// mechanically due — the survey judges its prose against what landed —
// so on a quiet board the only thing that can change the answer is the
// cooldown expiring. That expiry is the edge this leg fires on.
func TestHeartbeatProbesAJudgedChoreOnceEligible(t *testing.T) {
	root := judgedBoard(t, `,"cooldown":"1h"`, nil)

	decisions := dueDecisions(t, newHeartbeatGate(root), testTick)
	if got := sweepIDs(decisions); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v with a judged chore past its cooldown, want [moe]", got)
	}
	if got, want := reasonFor(decisions, "moe"), "a judged chore is eligible"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// TestHeartbeatProbesAJudgedChoreExactlyOnce is the property that makes
// the leg affordable. Sweeping whenever a judged chore is merely
// eligible burns an agent turn every twenty minutes until the condition
// happens to hold; the probe sweep mints a pulse run of its own, which
// moves lastPulseAt past the expiry and closes the edge.
func TestHeartbeatProbesAJudgedChoreExactlyOnce(t *testing.T) {
	root := judgedBoard(t, `,"cooldown":"1h"`, nil)
	g := newHeartbeatGate(root)
	if got := dueProjects(t, g); len(got) != 1 || got[0] != "moe" {
		t.Fatalf("due = %v on the first look, want [moe]", got)
	}

	// The probe: a pulse run that looked and declined. Declining writes
	// nothing about the chore — a skip marker is the operator's verb, not
	// the agent's — so the chore's own state is unchanged.
	seedPulseRun(t, root, "moe", "pulse-2", run.StatusClosed, 0)
	g.Swept("moe", true)

	for tick := range 3 {
		if got := dueProjects(t, g); len(got) != 0 {
			t.Fatalf("due = %v on tick %d after the probe declined, want none", got, tick+1)
		}
	}
}

// TestHeartbeatIsQuietForAJudgedChoreStillCoolingDown: the cooldown is
// the whole clock here, so a chore inside it has no edge to fire on.
func TestHeartbeatIsQuietForAJudgedChoreStillCoolingDown(t *testing.T) {
	root := judgedBoard(t, `,"cooldown":"168h"`, nil)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v for a judged chore still cooling down, want none", got)
	}
}

// TestHeartbeatIsQuietForAJudgedChoreWithNoCooldown is the abstention.
// A judged chore that carries no cooldown — or has never completed — has
// a zero NextEligible, and "eligible forever" is not an edge: firing on
// it would probe every tick, which is the cost the edge-trigger exists
// to avoid. It stays reachable through the moved leg, because
// registering or editing a chore is itself a journal commit.
func TestHeartbeatIsQuietForAJudgedChoreWithNoCooldown(t *testing.T) {
	root := judgedBoard(t, "", nil)

	if got := dueProjects(t, newHeartbeatGate(root)); len(got) != 0 {
		t.Errorf("due = %v for a judged chore with no cooldown, want none", got)
	}
}

// TestHeartbeatIsQuietForAJudgedChoreWithAnOpenRun: the anti-pile-up
// guard the mechanical family already honours applies here too — a probe
// would nominate a chore that is already being worked.
//
// The gate still sweeps, and should: a chore-rooted run is settled by
// construction, so the parked leg offers it and the sweep's kick rides
// it. What must not happen is the *probe* — asking the survey to judge a
// condition whose answer is already being acted on.
func TestHeartbeatIsQuietForAJudgedChoreWithAnOpenRun(t *testing.T) {
	root := judgedBoard(t, `,"cooldown":"1h"`, func(root string) {
		seedRun(t, root, "moe", "lore-sweep-2026-08-22", "sdlc", run.StatusInProgress, time.Now().Local(), nil)
		journalCommit(t, root, "moe", "open: lore-sweep-2026-08-22",
			"MoE-Consent: dynamic\nMoE-Project: moe\nMoE-Run: lore-sweep-2026-08-22\nMoE-Chore: moe/lore-sweep")
		backdateHead(t, root, 100*time.Minute)
	})

	decisions := dueDecisions(t, newHeartbeatGate(root), testTick)
	if got := reasonFor(decisions, "moe"); got == "a judged chore is eligible" {
		t.Errorf("reason = %q for a judged chore that already has an open run, want anything but a probe", got)
	}
}
