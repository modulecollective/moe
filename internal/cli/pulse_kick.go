package cli

import (
	"io"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// The harness-owned step that follows grooming: whether the pulse kicks
// anything itself. Where a spawned run *lands* is not here — that is a
// `chain` claim the survey makes, twin reflects included.

// pulseSelfKick is the last step of a pulse: kick the threads whose
// groom group asked for it. This is the only door to machine-rooted
// motion, and two structural guards hold it shut everywhere else.
//
// First, **dynamic consent upstream**. A `!!!` tail pulse — or a manual
// `moe pulse new` — grooms and parks; only a fourth bang the operator
// actually typed licenses the machine to start something. That is what
// makes the surprise ride impossible by construction rather than by
// restraint — "I ran a plain push and my terminal is riding a thread I
// never saw" cannot happen, and a plain push no longer sweeps at all.
//
// Second, **re-entrancy**: a pulse-kick only roots at an unchained
// spawner. If the run whose tail fired this pulse is itself a chain
// member, the ride that is (probably) carrying it already picks up
// growth on its own tail, so nested rides are impossible — again by
// construction, not by flag-threading.
//
// There is deliberately no third bound on *how many* generations this
// can run for. A kicked ride's own tail does fire a pulse, so a survey
// can groom and kick work whose tail grooms and kicks again — the
// machine walks until a survey finds nothing worth chaining. What holds
// that open-ended is the two guards above plus the ladder itself: each
// generation is real shipped work behind review and test, it shows up on
// the dash as it lands, and a Ctrl-C halts the ride. Escalation by
// visibility, not by counting.
//
// Kicks that do fire are themselves dynamic rides: a confident pulse
// rooting bounded-only motion would defeat the point, and an operator
// who wants bounded keeps `!!!`.
//
// And the thread's root must have **a settled design** — a disk fact
// about the work, not about who opened it. Lineage was the wrong proxy
// and said so twice in two days: keying on SpawnedBy alone stranded an
// operator-advanced run on 2026-07-22, and widening to "or advanced"
// stranded a pulse-nominated judged chore the same evening (chore opens
// are the one machine path that mints no SpawnedBy). Both runs were
// ready; neither had the lineage the guard wanted. So the question the
// admit asks is whether the design is settled and whether anyone is
// inside the run — see rootDesignSettled and openSessionStage.
//
// What stays with the operator is a root sitting at its first stage,
// in either of two shapes, and the skip line names which: nothing has
// run there yet — a promoted sketch, a hand-minted chain head composed
// over an afternoon — or the turn closed and nobody advanced it, which
// is the reopened run and the re-edit that out-dated its advance
// marker. Both are held; only one of them means the design never ran.
// Those have `!` and `!!!`. The kick bar still decides *whether to
// ask* — this is only the floor under it.
//
// Every skip is one stderr line, warn-only ethos.
//
// Every fact this step keys on comes out of the groom's final in-memory
// graph (see groomResult) — thread roots, the spawner's chain
// membership, and whether a root is still kickable. Re-reading the
// journal here would answer the same questions a second time against a
// state the sweep had already moved. The one live read is the root's
// session branches, and that is the point: it asks whether the operator
// has a stage open *right now*, which no snapshot can say.
func pulseSelfKick(root string, groomed groomResult, spawnerKey string, stdout, stderr io.Writer) {
	var wanted []groomedThread
	for _, th := range groomed.threads {
		if th.Kick && th.Root != "" {
			wanted = append(wanted, th)
		}
	}
	if len(wanted) == 0 {
		return
	}
	if currentRideMode != rideDynamic {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, this verb carried no dynamic consent (`!!!!` or --dynamic)\n", len(wanted))
		return
	}
	if spawnerKey != "" && groomed.spawnerChained {
		moePrintf(stderr, "pulse: %d thread(s) asked for a kick — skipping, %s is itself chained and its ride picks up growth on its own tail\n",
			len(wanted), spawnerKey)
		return
	}
	for _, th := range wanted {
		proj, runID, err := splitProjectRun(th.Root)
		if err != nil {
			moePrintf(stderr, "pulse: kick: malformed thread root %q: %v\n", th.Root, err)
			continue
		}
		// A group can be groomed onto a thread whose head has already
		// shipped — `onto` admits a settled anchor on purpose, that being
		// the queue-jump case — and the root then walks back to a merged
		// run. Kicking one would ride a finished thread from its finished
		// end. ChainChildLive is the same terminal-or-missing test every
		// other edge reader applies.
		md := groomed.byKey[th.Root]
		if md == nil || !run.ChainChildLive(th.Root, groomed.byKey) {
			moePrintf(stderr, "pulse: kick: %s heads a thread that has already settled — skipping\n", th.Root)
			continue
		}
		if settled, turnClosed := rootDesignSettled(root, md, groomed.idx); !settled {
			held := "only a seed"
			if turnClosed {
				held = "its turn closed but not advanced"
			}
			moePrintf(stderr, "pulse: kick: %s is waiting at its first stage with %s — the operator holds the trigger\n", th.Root, held)
			continue
		}
		if stage := openSessionStage(root, md); stage != "" {
			moePrintf(stderr, "pulse: kick: %s has a live session at %s — skipping\n", th.Root, stage)
			continue
		}
		moePrintf(stderr, "pulse: kicking %s (dynamic)\n", th.Root)
		if code := chainKickRun(root, proj, runID, rideDynamic, stdout, stderr); code != 0 {
			moePrintf(stderr, "pulse: kick %s exited %d\n", th.Root, code)
		}
	}
}

// rootDesignSettled reports whether md's design is settled — the one
// structural readiness fact the kick admits on. "Parked at code, human
// runs are good to go" passes; an operator-promoted sketch still
// waiting at design with only its seed does not. Three legs, any of:
//
//   - **Past its first stage.** Mechanically (stageSatisfied) that
//     needs a first-stage work-turn *and* either an advance marker at
//     least as recent as it, or a downstream stage's turn. So this leg
//     is exactly "the operator advanced it, or real downstream work
//     exists" — and a run whose design merely closed still reads as
//     waiting at design, which preserves AdvancedTo's "a canvas merely
//     complete is not consent to proceed" by mechanism rather than by a
//     kick-side special case. The staleness rule (a marker out-dated by
//     a re-edit) is inherited the same way.
//   - **Machine-minted** (SpawnedBy) — the seed is a design baked by
//     the spawning run.
//   - **Chore-rooted** — the seed is the chore's operator-authored
//     prompt.md, so standing intent is a settled design by
//     construction. Its own leg because openChoreInProcess is the one
//     machine-open path that stamps no SpawnedBy, and it can't be made
//     to: autoOpenDueChores runs before the pulse run is minted, and a
//     nomination landing on an already-open chore run inherits the old
//     open commit.
//
// An empty ladder (the `chain` workflow's placeholder head) is never
// past-first. A root with nothing left to run is past-first by the same
// comparison and reports its own reason downstream — a terminal one has
// already fallen to the settled-thread guard above, and a live one owes
// its ride only its members, which is chainKickRun's nothing-pending
// branch to say.
//
// turnClosed is the caller's reporting bit and only means anything when
// settled is false: it says a first-stage work-turn has landed, so the
// hold is "worked, not advanced" rather than "never worked". It lives
// here because the first stage it asks about is the one the past-first
// comparison just resolved; deriving it at the call site would look the
// workflow up a second time to answer half a question.
func rootDesignSettled(root string, md *run.Metadata, idx *run.JournalIndex) (settled, turnClosed bool) {
	if md.SpawnedBy != "" {
		return true, false
	}
	if idx != nil && idx.ChoreByRun[md.Project+"/"+md.ID] != "" {
		return true, false
	}
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return false, false
	}
	stages := w.Stages()
	if len(stages) == 0 {
		return false, false
	}
	stage, _, err := w.NextWithIndex(root, md, idx)
	if err != nil {
		return false, false
	}
	if stage != stages[0] {
		return true, false
	}
	// Held at the first stage: a turn that landed and wasn't advanced
	// (no marker, a marker a re-edit out-dated, a failed gate) reads
	// differently to the operator than a stage nothing has run at. Same
	// workTurnTime stageSatisfied just consulted.
	when, err := workTurnTime(root, md.Project, md.ID, stages[0], idx)
	if err != nil {
		return false, false
	}
	return false, !when.IsZero()
}

// openSessionStage returns the stage md has a live session branch at,
// or "" for none. This is the occupancy check: a settled design says
// the run is ready, and this says nobody is already inside it.
//
// Branches are the only signal that works. run.json's session id
// reaches main only when the turn commits (commitSessionStart writes it
// on the session branch), so a mid-session run reads as having no
// session at all — the exact window the check exists to close. Refs
// live in the common dir, so HasRef reads true from any worktree, and
// Close/Abandon both delete the branch.
//
// A run that died mid-stage keeps its branch, and this cannot tell that
// leftover from an operator's live session — so such a root is held,
// one skip line per sweep, until the session is resumed or abandoned.
// Conservative on purpose; the line is the recovery signpost.
func openSessionStage(root string, md *run.Metadata) string {
	w, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return ""
	}
	for _, stage := range w.Stages() {
		if git.HasRef(root, "refs/heads/"+session.BranchName(md.Project, md.ID, stage)) {
			return stage
		}
	}
	return ""
}
