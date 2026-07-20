package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/wiki"
)

// The pulse workflow is the level-3 "gather" primitive: a headless,
// read-only sweep of one project that files followup entries (→ ideas
// via the existing harvest) and writes a short report whose gate may
// spawn parked fix runs and groom queued work into lanes. It has no
// push — the artifact is the filed followups plus the canvas report.
//
// A pulse is more than the survey. It fires at the tail of the
// operator-rooted run-traffic verbs (sdlc close, sdlc push, twin close,
// and the cascades' auto-close), and every fire does two things:
//
//   - Always: open every due chore's run for the project (never execute
//     one) via openChoreInProcess. Automation acts on standing intent —
//     a chore the operator authored — but never makes a fresh decision.
//   - Every time: the survey — a blocking, headless stage that opens a
//     run, sweeps, files followups, writes its report, and auto-closes
//     itself on a clean exit. Every run-traffic event fires a fresh
//     survey unconditionally; a failed or abandoned sweep leaves its run
//     open on the dash's ACTIVE list for a human to look at, but nothing
//     blocks the next survey — visible junk over invisible absence.
//
// `moe pulse new <project>` runs the whole pulse by hand; it is also
// the verb an external cron would call. Cron itself stays out of moe —
// the primitives are cron-safe, the cron is not ours.
const (
	pulseWorkflow = "pulse"
	// pulseDoc is the single stage's document id. The survey canvas
	// lives at documents/pulse/content.md.
	pulseDoc = "pulse"
)

// pulseKickoff is the survey's first user turn (the whole `claude -p`
// prompt in headless mode). The steering lives in the stage fragment;
// this just points the agent at the job.
const pulseKickoff = "Run the pulse for this project: a delta-first, read-only sweep. " +
	"Survey what changed since the last pulse — the journal, twin-vs-code drift in the touched areas, the open backlog — " +
	"file followup entries for work worth doing, and write the canvas report. Follow the stage guidance. A quiet pulse — " +
	"\"nothing new since the last pulse\" — is a valid, successful report; never manufacture findings.\n\n" +
	"Close the canvas with the `## Gate` section (a ```json fence). Set \"status\" to a short word (e.g. \"ok\") once the " +
	"survey actually ran and concluded — that is what tells the harness this was a real sweep, not a crashed no-op. " +
	"Flag a twin reflect as due — `\"reflect\": {\"due\": true, \"why\": \"<one line>\"}` — when either the cycle landed a " +
	"significant twin-relevant change (a decision, a new component, a boundary move the twin docs don't yet describe), or " +
	"twin staleness has accumulated (many small changes and/or pending twin observations teed up since the last reflect). " +
	"Do NOT flag reflect due when a twin run is already open, and never manufacture a reflect to justify the turn — the " +
	"default is `\"reflect\": {\"due\": false}`. The `why` is required when due: one line, the operator reads it next to the verdict.\n\n" +
	"The gate may also carry a `\"spawn\"` list: high-confidence fixes the harness should open as parked runs. The bar is " +
	"mechanical, bounded, and verifiable — all three — and the stage guidance holds it. Omitting `spawn` is the normal " +
	"outcome; a followup is the default channel for everything that doesn't clear the bar.\n\n" +
	"And a `\"chain\"` list: groups of run slugs in execution order, each attached after an existing run (`\"onto\"`), under a " +
	"freshly named head (`\"head\"`), or left to land opportunistically. A group may name runs this gate just spawned or any " +
	"parked run in the project — naming one that is chained elsewhere moves it. This is where your ordering judgment goes; " +
	"there is no prose ranking section. The bar is the spawn bar plus ordering conviction: would the operator kick these, in " +
	"this order, unchanged? If the order is a guess, leave the runs loose. A group may add `\"kick\": true` to ask the harness " +
	"to start that thread — highest bar on the canvas, and the harness refuses it unless the operator's own verb licensed " +
	"machine-rooted motion. Omitting `chain` is a perfectly normal outcome. See the stage guidance."

// pulseCanvasSkeleton is the fixed structural shape the survey canvas
// opens with. The agent fills the sections in place. The gate's grammar
// — spawn entries, chain groups, the bars each is held to — is taught
// in the stage fragment, not restated here.
const pulseCanvasSkeleton = `# Pulse

## What landed

(agent fills: 2–3 lines — what changed since the last pulse)

## Surveyed

(agent fills: what was read — the journal slice, twin areas, the backlog)

## New filings

(agent fills: one line per followup filed. "None" is valid.)

## Backlog hygiene

(agent fills: stale/duplicate flags, advisory only. Empty is fine.)

## Gate

(agent fills: a fenced json block — set "status" once the survey concluded, "reflect": {"due": …, "why": …}, and optionally "spawn": [...] and "chain": [...]. This placeholder has no fence, so a no-op turn leaves the gate detectably unfilled.)
`

func init() {
	g := NewCommandGroup(pulseWorkflow, "pulse workflow — read-only project sweep that feeds the backlog")
	// `moe pulse new <project>` is the manual whole-pulse kick (and the
	// external-cron entry point): chore auto-open plus the survey, both
	// headless.
	g.Register(&Command{
		Name:    "new",
		Summary: "run the whole pulse for a project: open due chores, then a headless survey",
		Run:     runPulseNew,
	})
	// The single stage's opener — reopen a sweep to inspect or re-run it
	// by hand. The hook and `moe pulse new` drive it headless.
	g.Register(&Command{
		Name:    pulseDoc,
		Summary: "open an agent session on a pulse run's survey canvas",
		Run:     runPulseStage,
		argKind: argProjectRun,
	})
	// Pulse has no workspace and no moe/<run> branch, so close has
	// nothing workflow-specific to clean up — pass nil and ride the
	// shared harvest / state-guard / status-flip path. The happy-path
	// survey auto-closes through this same registration (skipEdit, so
	// filings promote to ideas unreviewed); the verb itself is the manual
	// ending for interactive sittings and failed sweeps, where the
	// editor-pop prune gate still applies.
	g.Register(closeCommand(pulseWorkflow, "Close pulse run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (pulse cat <project>/<run> <stage>)",
		Run:     runCat(pulseWorkflow, ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (pulse log <project>/<run> <stage>)",
		Run:     runLog(pulseWorkflow, ""),
	})
	RegisterGroup(g)

	w := NewWorkflow(pulseWorkflow)
	w.RegisterStage(pulseDoc)
	// Machine-minted and machine-driven: pulse registers a cascade
	// dispatcher (so its own auto-drive can reach the stage seam) but
	// must stay out of the operator cascade vocabulary. SetMachinePaced
	// is the one declaration that excludes it everywhere operatorCascades
	// keys — stage-verb flags, chain edit, serve chips — the sibling of
	// chat's SetPerpetual.
	w.SetMachinePaced()
	RegisterWorkflow(w)
}

// pulseFiresForWorkflow reports whether a terminal transition of the
// given workflow is run traffic that should tail a pulse. sdlc and twin
// move intent — code and the recorded canon; the rest of the
// close-registered workflows (chat, kb, hooks, chores) and pulse itself
// do not, which is also what makes pulse-on-pulse recursion structurally
// impossible. Used by both the close seam and the push seam (twin has no
// push, so at a push point the workflow is always sdlc — the guard is
// defensive there).
func pulseFiresForWorkflow(workflow string) bool {
	return workflow == "sdlc" || workflow == "twin"
}

// firePulse runs the pulse for a project at the tail of a run-traffic
// verb. spawner is the triggering run's slug, threaded onto the survey
// run's MoE-Spawned-By edge so the dash can nest the pulse under it. It
// is a var so the close/push wiring can be observed in tests without
// running the survey's agent turn.
//
// Warn-only by construction: the transition that triggered the pulse
// has already committed and pushed by the time this runs, so a pulse
// failure must never change the triggering verb's outcome (it mirrors
// closeWithAutoResolve's posture, not the abort-on-fail push
// synthesis). runPulse prints its own warnings; the exit code is
// dropped here. The returned bool is "operator interrupted the sweep" —
// callers thread it to exitInterrupted so a Ctrl-C halts a cascade
// instead of riding on to the next run.
var firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) bool {
	_, interrupted := runPulse(root, projectID, spawner, stdout, stderr)
	return interrupted
}

// runPulse is the whole pulse: the deterministic chore auto-open (which
// opens runs but executes none), then the survey. spawner is the
// triggering run's slug ("" for a manual `moe pulse new`, which threads
// no parent edge).
//
// It owns the pulse's scoped Ctrl-C latch (installPulseInterrupt): the
// "scanning — Ctrl-C to skip" banner prints up front, before the run is
// minted, so the operator knows the skip window is live from the start.
// The second return is whether the operator interrupted the sweep.
func runPulse(root, projectID, spawner string, stdout, stderr io.Writer) (int, bool) {
	pi := installPulseInterrupt()
	defer pi.Close()
	moePrintf(stderr, "pulse: scanning %s — Ctrl-C to skip\n", projectID)
	autoOpenDueChores(root, projectID, pi, stdout, stderr)
	reconcileAtPulse(root, projectID, pi, stdout, stderr)
	code := runPulseSurvey(root, projectID, spawner, pi, stdout, stderr)
	return code, pi.interrupted()
}

// reconcileAtPulse asks GitHub about this project's pushed runs and
// applies whatever landed out of band, so a PR the operator merged from
// their phone is `merged` in the journal *before* the survey reads its
// delta. Same walk `moe sync` does, scoped to one project — sync still
// owns pointer bumps; the pulse takes only the reconcile step.
//
// Warn-only like everything else in the pulse: a reconcile failure
// (offline, no gh, a wedged lock) must not derail the sweep or the verb
// that triggered it. The repolock is acquired here because firePulse
// runs after the triggering verb released it, and it is held only for
// the walk — the survey's own run-open takes its own. The journal push
// is conditional on something actually having moved: the common case is
// a project with nothing pushed, which should stay a disk-only scan.
func reconcileAtPulse(root, projectID string, pi *pulseInterrupt, stdout, stderr io.Writer) {
	// Checkpoint: a Ctrl-C during chore auto-open skips the network walk
	// too — the operator asked for the sweep to get out of the way.
	if pi.interrupted() {
		return
	}
	err := repolock.With(root, repolock.Options{
		Purpose:   "pulse-reconcile",
		Budget:    repolock.CronBudget,
		Heartbeat: true,
	}, func() error {
		moved, err := reconcilePushedRuns(root, projectID, stdout, stderr)
		if err != nil {
			return err
		}
		if moved == 0 {
			return nil
		}
		return sync.AutoPush(root, stdout, stderr)
	})
	if err != nil {
		moePrintf(stderr, "pulse: reconcile pushed runs for %s: %v\n", projectID, err)
	}
}

// autoOpenDueChores opens every due chore's run for the project via the
// shared chore-open pipeline. No stage executes — the operator kicks
// the first stage when ready. The existing open-run refusal is the
// anti-pile-up guard, so a chore that already has an open run is
// skipped silently; any other failure warns and moves on (a chore
// pile-up must not derail the sweep or the verb that triggered it).
func autoOpenDueChores(root, projectID string, pi *pulseInterrupt, stdout, stderr io.Writer) {
	states, err := gatherChoreStates(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: read chore states for %s: %v\n", projectID, err)
		return
	}
	for _, s := range states {
		// Checkpoint: a Ctrl-C stops opening further chores. Already-opened
		// ones are standing intent and stay; the survey below sees the latch
		// and skips too.
		if pi.interrupted() {
			return
		}
		if !s.Due {
			continue
		}
		if _, err := openChoreInProcess(root, projectID, s.Definition.Name, false, stdout, stderr); err != nil {
			var notOpenable *choreNotOpenableError
			if errors.As(err, &notOpenable) {
				// Expected: an open run already holds this chore, or it
				// cooled/undued between the scan and the open.
				continue
			}
			moePrintf(stderr, "pulse: open chore %s: %v\n", s.Definition.Key(), err)
		}
	}
}

// runPulseSurvey is the agent part of the pulse. It is a var so tests
// exercising the deterministic parts (chore auto-open, auto-close) can
// stub the agent turn out.
//
// Every fire runs a fresh survey unconditionally — there is no rate
// limiter. On a clean (exit 0) survey it auto-closes its own run; a
// failed or SIGINT'd sweep leaves the run open on the dash's ACTIVE list
// (escalation by visibility), but does not block the next survey.
// Concurrent and piled-up pulse runs are allowed: run opening mints
// distinct dated slugs under the repolock, so parallel fires don't
// collide.
//
// Body assigned in init() rather than at declaration to break the
// firePulse ↔ runPulseSurvey initialization cycle the auto-close arm
// introduces (auto-close → closeRunInProcess → firePulse) — the same
// init-order dodge openPulseStage uses.
var runPulseSurvey func(root, projectID, spawner string, pi *pulseInterrupt, stdout, stderr io.Writer) int

func init() {
	runPulseSurvey = pulseSurvey
}

func pulseSurvey(root, projectID, spawner string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
	// spawner threads the triggering run onto the survey's MoE-Spawned-By
	// edge (empty on a manual `moe pulse new`, so it renders un-nested).
	// Both the metadata field and the trailer are set, mirroring how
	// `moe sdlc reopen` pairs Options.ReopenOf with Trailers.ReopenOf.
	// Qualify the spawner to "<project>/<slug>" so the lineage edge can name
	// a foreign spawner (cross-project coordination opens a run in one
	// project from another); the empty guard keeps a manual `moe pulse new`
	// from writing a dangling "<project>/".
	if spawner != "" {
		spawner = projectID + "/" + spawner
	}

	// Checkpoint: a Ctrl-C before the run is minted skips with nothing to
	// clean — no run, no lock (runopen.Open's window hasn't opened yet).
	if pi.interrupted() {
		moePrintf(stderr, "pulse: skipped — no run opened for %s\n", projectID)
		return 0
	}

	md, err := runopen.Open(root, projectID, run.Options{
		IDBase:    pulseWorkflow,
		Workflow:  pulseWorkflow,
		SeedDocs:  map[string]string{pulseDoc: pulseCanvasSkeleton},
		SpawnedBy: spawner,
		Trailers:  trailers.Block{SpawnedBy: spawner},
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "pulse: open run for %s: %v\n", projectID, err)
		return 1
	}

	// Checkpoint: a Ctrl-C landed while the run was being minted — dispose
	// the just-minted skeleton run so it doesn't linger on the dash with
	// nothing to review.
	if pi.interrupted() {
		disposePulseRun(root, projectID, md.ID, stdout, stderr)
		return 0
	}

	// A non-zero exit (agent failure or SIGINT) is never propagated —
	// abandoning a sweep is not a verb failure — and (mid-agent) it leaves
	// the run open on the dash's ACTIVE list for the operator to inspect
	// and close by hand. It does not block the next survey.
	code := openPulse(projectID, md.ID, true /*headless*/, "", pi, stdout, stderr)
	switch {
	case code == exitInterrupted:
		// Mid-agent Ctrl-C: the survey was actually running and may hold
		// real findings, so disposing it would harvest half-written
		// followups into ideas unreviewed. Leave the run open for review —
		// but propagate the interrupt (mark the latch, since the Ctrl-C may
		// have been observed only at the agent boundary) so a cascade halts.
		pi.mark()
		return 0
	case pi.interrupted():
		// The latch is set but the agent never ran to a real conclusion: a
		// Ctrl-C in a millisecond gap between setup children tripped the
		// pre-executor belt (openPulse's prompt builder returned
		// errPulseSkipped, exit 1 ≠ 130). Dispose the just-minted run.
		disposePulseRun(root, projectID, md.ID, stdout, stderr)
		return 0
	case code != 0:
		// A failed or abandoned sweep with no interrupt — leave the run open
		// on the dash's ACTIVE list (escalation by visibility).
		return 0
	}

	// Read the survey's `## Gate` verdict. An unfilled or unparsable gate
	// — the skeleton placeholder, or a turn that exited 0 without writing
	// a real conclusion — means the sweep didn't actually conclude:
	// refuse the auto-close so the run lingers on the dash's ACTIVE list
	// (escalation by visibility), and skip the reflect spawn. Any parsed
	// non-empty status passes; a pulse has no ready/blocked vocabulary,
	// only close-or-linger.
	gate, ok := readPulseGate(root, projectID, md.ID)
	if !ok {
		moePrintf(stderr, "pulse: %s/%s left an unfilled gate — leaving the run open for review\n", projectID, md.ID)
		return 0
	}
	// Mint, then groom, then place the reflect, then kick. The order is
	// the design's: a group can only name runs that exist, the reflect
	// belongs at the *post-groom* tail so it reads the settled record
	// after the fixes it follows, and a kick must not start until the
	// thread it names has stopped moving.
	reflectID := ""
	if gate.Reflect.Due {
		reflectID = maybeSpawnReflect(root, projectID, md.ID, gate.Reflect.Why, stdout, stderr)
	}
	minted := maybeSpawnFixRuns(root, projectID, md.ID, gate.Spawn, stdout, stderr)
	threads := groomChains(root, projectID, md.ID, gate.Chain, minted, spawner, stdout, stderr)
	// Appended last on purpose: with no spawner tail to stamp onto, the
	// reflect rides as its own thread, and pulseSelfKick's loop is
	// sequential — so trailing the gate-named fix threads gives it the
	// same read-after-fixes ordering the tail stamp gives, with no extra
	// chain commit.
	if reflectThread := placeReflect(root, projectID, md.ID, reflectID, spawner, stdout, stderr); reflectThread != nil {
		threads = append(threads, *reflectThread)
	}

	// Clean sweep: auto-close the run so the next run-traffic event can
	// fire a fresh survey. Route through the registered close (subject +
	// cleanup) so there's no parallel close path. skipEdit harvests
	// followups.md as-is — the filings promote to ideas unreviewed;
	// review moves to scrapping on the dash. tailPulse=false because
	// pulse never tails pulse (pulseFiresForWorkflow excludes it — the
	// false just says so at the call). A close failure warns and leaves
	// the run open, mirroring firePulse's warn-only posture: the report
	// and filings are already durable on disk, so a failed auto-close is
	// a close-by-hand-later, not a lost sweep.
	if err := closePulseRun(root, projectID, md.ID, stdout, stderr); err != nil {
		moePrintf(stderr, "pulse: auto-close %s/%s: %v\n", projectID, md.ID, err)
	}

	// The kick is last, and deliberately *outside* the pulse's skip
	// window. pi latches the first Ctrl-C and then steps aside, which is
	// exactly right while the sweep is the thing running — but a ride the
	// pulse roots is not the sweep. Left inside, a Ctrl-C aimed at the
	// ride would be swallowed by the latch: the ride would carry on and
	// the finished sweep would be reported as interrupted. Closing the
	// latch first hands SIGINT back to the ride's own handling, the same
	// as an operator-typed kick.
	//
	// Closing the pulse run first is the other half: a ride can run for
	// a long time, and a sweep that has already done all its work should
	// not sit on the dash's ACTIVE list for the duration.
	pi.Close()
	pulseSelfKick(root, threads, spawner, stdout, stderr)
	return 0
}

// closePulseRun closes a pulse run through the registered close — the
// same (subject, cleanup) the happy-path auto-close and the interrupt
// disposal both ride, so there's no parallel close path. skipEdit
// harvests followups.md as-is; tailPulse=false because pulse never
// tails pulse (pulseFiresForWorkflow excludes it).
func closePulseRun(root, projectID, runID string, stdout, stderr io.Writer) error {
	reg, ok := lookupCloseRegistration(pulseWorkflow)
	if !ok {
		return fmt.Errorf("no close registration for %q", pulseWorkflow)
	}
	return closeRunInProcess(root, pulseWorkflow, reg.subject, reg.cleanup,
		projectID, runID, true /*skipEdit*/, false /*tailPulse*/, stdout, stderr)
}

// disposePulseRun closes a just-minted pulse run the operator Ctrl-C'd
// before the survey could produce anything worth reviewing. The
// skeleton canvas is non-empty so the close-time canvas gate passes, and
// the canonical root is committed-clean at that point so the dirty-tree
// gate passes. A disposal failure warns and leaves the run open —
// today's worst-case outcome, minus the lock leak.
func disposePulseRun(root, projectID, runID string, stdout, stderr io.Writer) {
	if err := closePulseRun(root, projectID, runID, stdout, stderr); err != nil {
		moePrintf(stderr, "pulse: skip-close %s/%s: %v — leaving run open\n", projectID, runID, err)
		return
	}
	moePrintf(stderr, "pulse: skipped — closed %s/%s\n", projectID, runID)
}

// pulseGate is the machine-readable verdict the survey agent writes to
// the canvas's `## Gate` section. A non-empty status is all the
// auto-close decision needs — a pulse has no ready/blocked advance
// vocabulary, only close-or-linger. reflect asks the harness to mint a
// parked twin reflect run; why is the operator-facing rationale, carried
// next to the verdict on the pulse canvas.
type pulseGate struct {
	Status  string `json:"status"`
	Reflect struct {
		Due bool   `json:"due"`
		Why string `json:"why"`
	} `json:"reflect"`
	// Spawn carries the survey's high-confidence fix proposals. The
	// agent proposes; the harness executes — so the survey sandbox stays
	// read-only and needs no new tools. Each entry becomes a parked sdlc
	// run, unchained: ordering is a separate claim, made in Chain.
	Spawn []pulseSpawn `json:"spawn"`
	// Chain carries the survey's ordering opinion: groups of run slugs
	// in execution order, each placed after an existing run, under a
	// freshly named head, or self-rooted. See pulse_groom.go. A spawn
	// entry named in no group parks standalone — ordering is a claim,
	// and the lane bar prices it separately from the spawn bar.
	Chain []pulseChainGroup `json:"chain"`
}

// pulseSpawn is one proposed fix run. Slug is the slug base (the harness
// dates it on collision); Title and Why are what the operator reads on
// the chain canvas before kicking; Design seeds the new run's design
// canvas, so the design stage starts from the survey's findings instead
// of re-deriving them.
type pulseSpawn struct {
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Why    string `json:"why"`
	Design string `json:"design"`
}

// readPulseGate reads the survey canvas and parses its `## Gate` JSON
// fence (the shared `stageGateJSON` grammar). ok is false for every
// no-op shape the auto-close refusal keys on: a missing/unreadable
// canvas, an absent or empty fence (the skeleton placeholder),
// unparseable JSON, or an empty status. A read error reads as unfilled —
// the run lingers rather than auto-closing on a canvas we couldn't
// inspect.
func readPulseGate(root, projectID, runID string) (pulseGate, bool) {
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(projectID, runID, pulseDoc)))
	if err != nil {
		return pulseGate{}, false
	}
	payload, ok := stageGateJSON(string(body))
	if !ok {
		return pulseGate{}, false
	}
	var g pulseGate
	if err := json.Unmarshal(payload, &g); err != nil {
		return pulseGate{}, false
	}
	if g.Status == "" {
		return pulseGate{}, false
	}
	return g, true
}

// maybeSpawnReflect mints a parked twin reflect run when the survey's
// gate flagged drift as due. Warn-only throughout — a guard refusal or a
// mint failure never blocks the pulse's auto-close, since the report and
// filings are already durable on disk. The guards live in mintReflectRun
// (shared with `moe twin reflect`): an in-progress twin run, including a
// prior auto-spawned reflect still parked, is a silent skip — no sense
// opening a second; out-of-band twin edits warn and skip, since the
// operator has to clear those before any reflect can run.
// Returns the minted run's id, or "" when nothing was minted — the
// handle placeReflect needs to place it at a chain's tail once
// this sweep's grooming has settled.
func maybeSpawnReflect(root, projectID, pulseSlug, why string, stdout, stderr io.Writer) string {
	canonical, err := twinWikiBuilder(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: reflect spawn: build twin wiki for %s: %v\n", projectID, err)
		return ""
	}
	// Qualify the spawner to "<project>/<slug>" before minting, mirroring
	// pulseSurvey: run.Options.SpawnedBy is the qualified spawner, and the
	// journal index treats these edges as always qualified. pulseSlug is the
	// pulse run's own slug and is never empty here, so no empty-guard is
	// needed.
	md, err := mintReflectRun(root, projectID, projectID+"/"+pulseSlug, "" /*agent*/, canonical, stdout, stderr)
	if err != nil {
		var refusal *reflectRefusal
		if errors.As(err, &refusal) {
			if refusal.kind == reflectRefusalUnrecorded {
				moePrintf(stderr, "pulse: reflect not spawned for %s — %v; the operator lands those first\n", projectID, refusal)
			}
			return ""
		}
		moePrintf(stderr, "pulse: reflect spawn for %s: %v\n", projectID, err)
		return ""
	}
	moePrintf(stderr, "pulse: drift flagged — opened twin reflect %s/%s (%s)\n", projectID, md.ID, why)
	return md.ID
}

// maybeSpawnFixRuns mints a parked sdlc run for each high-confidence fix
// the survey's gate proposed. Minting is all it does: ordering is a
// separate claim the gate makes in its `chain` list, priced against a
// higher bar, and groomChains is what stamps it. A spawn entry named in
// no group parks standalone and unchained — which is the normal outcome
// for work whose order the survey isn't sure of.
//
// Returns proposed-slug → minted-run-id, so a `chain` group can name
// this batch's own runs by the slug the survey wrote even when the
// harness dated it on collision.
//
// No numeric cap. The harness has no basis for judging which proposals
// to trim, and parked is itself the review gate: spawned runs are
// visible on the dash and prunable with `moe chain edit`.
// Over-proposal is visible junk, which the pulse already prefers to
// invisible absence. The bar — mechanical, bounded, verifiable — is
// taught in the stage fragment, where judgment belongs.
//
// The one mechanical exception to dedupe is a live idea carrying a
// harvested follow-up's workflow tag: the pulse promotes that capture
// through the same seam as `--from-idea`. Untagged ideas remain behind a
// structural human-triage fence. Every other live match still skips.
//
// Warn-only throughout, like the reflect spawn beside it: the report and
// filings are already durable, so a failed mint is a spawn-by-hand-later,
// not a lost sweep.
func maybeSpawnFixRuns(root, projectID, pulseSlug string, spawns []pulseSpawn, stdout, stderr io.Writer) map[string]string {
	if len(spawns) == 0 {
		return nil
	}
	live, err := liveSlugs(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: spawn: scan runs for %s: %v\n", projectID, err)
		return nil
	}

	minted := map[string]string{}
	for _, s := range spawns {
		slug := strings.TrimSpace(s.Slug)
		if slug == "" || run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: spawn: skipping entry with unusable slug %q\n", s.Slug)
			continue
		}
		if slugBaseMatches(live, slug) {
			matches, scanErr := matchingLiveRuns(root, projectID, slug)
			if scanErr != nil {
				moePrintf(stderr, "pulse: spawn: scan live match for %s/%s: %v\n", projectID, slug, scanErr)
				continue
			}
			if len(matches) != 1 || matches[0].Workflow != "idea" {
				moePrintf(stderr, "pulse: spawn: %s already has a live run for %q — skipping\n", projectID, slug)
				continue
			}
			idea := matches[0]
			if idea.PromoteTo == "" {
				moePrintf(stderr, "pulse: spawn: idea %s/%s is untagged and requires operator triage — skipping\n", projectID, idea.ID)
				continue
			}
			wf, lookupErr := LookupWorkflow(idea.PromoteTo)
			if lookupErr != nil || !chainableWorkflow(idea.PromoteTo) || len(wf.Stages()) == 0 {
				moePrintf(stderr, "pulse: spawn: idea %s/%s has unusable workflow tag %q — skipping\n", projectID, idea.ID, idea.PromoteTo)
				continue
			}
			if strings.TrimSpace(s.Design) != "" {
				moePrintf(stderr, "pulse: spawn: ignoring design body for tagged idea %s/%s; the idea canvas is the seed\n", projectID, idea.ID)
			}
			promoted, promoteErr := runopen.Promote(root, projectID, idea.ID, runopen.PromoteOptions{
				Workflow:   idea.PromoteTo,
				FirstStage: wf.Stages()[0],
				SpawnedBy:  projectID + "/" + pulseSlug,
			}, stdout, stderr)
			if promoteErr != nil {
				moePrintf(stderr, "pulse: promote tagged idea %s/%s: %v\n", projectID, idea.ID, promoteErr)
				continue
			}
			if promoted.MarkErr != nil {
				moePrintf(stderr, "pulse: warning: promoted %s/%s but could not mark the idea: %v\n", projectID, promoted.Run.ID, promoted.MarkErr)
			}
			live = append(live, promoted.Run.ID)
			minted[slug] = promoted.Run.ID
			moePrintf(stderr, "pulse: promoted tagged idea %s/%s to %s run %s/%s\n", projectID, idea.ID, idea.PromoteTo, projectID, promoted.Run.ID)
			continue
		}
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = slug
		}
		md, err := runopen.Open(root, projectID, run.Options{
			IDBase:    slug,
			Workflow:  "sdlc",
			SeedDocs:  map[string]string{"design": spawnDesignSeed(title, s)},
			SpawnedBy: projectID + "/" + pulseSlug,
			Trailers:  trailers.Block{SpawnedBy: projectID + "/" + pulseSlug},
		}, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "pulse: spawn %q for %s: %v\n", slug, projectID, err)
			continue
		}
		// Claim the base so a second entry in the same batch proposing the
		// same slug hits the dedupe rather than minting a dated sibling.
		live = append(live, md.ID)
		minted[slug] = md.ID
		moePrintf(stderr, "pulse: spawned fix run %s/%s (%s)\n", projectID, md.ID, title)
	}
	return minted
}

// matchingLiveRuns returns every live run derived from base. The pulse
// only promotes when the tagged idea is the sole match: a dated live
// destination beside it means a prior promotion already queued the work,
// even if marking the source idea failed.
func matchingLiveRuns(root, projectID, base string) ([]*run.Metadata, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var matches []*run.Metadata
	for _, md := range mds {
		if md.Project != projectID || (md.Status != run.StatusInProgress && md.Status != run.StatusPushed) {
			continue
		}
		if slugBaseMatches([]string{md.ID}, base) {
			matches = append(matches, md)
		}
	}
	return matches, nil
}

// spawnDesignSeed builds the design canvas body a spawned run opens
// with. The survey's own markdown is the body when it wrote one;
// otherwise the title and why are all there is, which is still a
// better starting point than an empty canvas.
func spawnDesignSeed(title string, s pulseSpawn) string {
	body := strings.TrimSpace(s.Design)
	if body != "" {
		return body + "\n"
	}
	seed := "# " + title + "\n"
	if why := strings.TrimSpace(s.Why); why != "" {
		seed += "\n" + why + "\n"
	}
	return seed
}

// liveSlugs lists the project's live run slugs — the dedupe set the
// spawn guard checks against. Live means in progress or pushed and
// waiting on a human to merge: in both cases the fix is already in
// flight, and whatever it addresses stays broken until it lands.
// Merged, closed and promoted runs are out — a finding that survives a
// merge is a new finding, not a duplicate.
func liveSlugs(root, projectID string) ([]string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, md := range mds {
		if md.Project == projectID && (md.Status == run.StatusInProgress || md.Status == run.StatusPushed) {
			out = append(out, md.ID)
		}
	}
	return out, nil
}

// datedSlugSuffix matches what run.Options.IDBase appends to a slug base
// on collision: `-YYYY-MM-DD`, optionally `-N` for a same-day repeat.
var datedSlugSuffix = regexp.MustCompile(`^-\d{4}-\d{2}-\d{2}(-\d+)?$`)

// slugBaseMatches reports whether any slug in the set was derived from
// base — the bare base, or one of IDBase's dated forms. The spawn guard
// passes the live set; the close-time followup-claim check passes every
// run on record.
//
// Deliberately not a bare prefix match: `fix-ci` and `fix-ci-red-main`
// are different proposals, and a greedy prefix would silently skip the
// second whenever the first is live. Only a date-shaped remainder counts
// as "the harness already dated this base".
func slugBaseMatches(slugs []string, base string) bool {
	for _, slug := range slugs {
		if slug == base {
			return true
		}
		if rest, ok := strings.CutPrefix(slug, base); ok && datedSlugSuffix.MatchString(rest) {
			return true
		}
	}
	return false
}

// pulseKickoffWithContext appends the harness-computed context blocks to
// the static kickoff — the twin-reflect line, the GitHub block, the
// recently-settled-runs block, and the chain-state block. Wired
// as InitialPromptBuilder, so root is the session worktree
// runStageSession hands the builder. Best-effort throughout: a gather
// that fails drops its own block rather than failing the sweep.
func pulseKickoffWithContext(root, projectID, runID string, stderr io.Writer) string {
	blocks := []string{pulseKickoff}
	if line := pendingTwinObservationsLine(root, projectID); line != "" {
		blocks = append(blocks, line)
	}
	if gh := pulseGitHubContext(root, projectID, runID, stderr); gh != "" {
		blocks = append(blocks, gh)
	}
	if settled := settledRunsBlock(root, projectID); settled != "" {
		blocks = append(blocks, settled)
	}
	if chains := chainStateBlock(root, projectID); chains != "" {
		blocks = append(blocks, chains)
	}
	if advanced := advancedRunsBlock(root, projectID); advanced != "" {
		blocks = append(blocks, advanced)
	}
	// Its own block, not a tail on the chain-state one. A tail pulse
	// fires after its spawner merged, so the ridden unit is usually
	// below the two-active-member bar chainStateBlock renders at — and
	// an unchained spawner (the self-kick door) has no chain at all.
	// Nested, the line reached the agent only when some *unrelated*
	// chain happened to be active: absent in both cases it exists for.
	if ride := rideModeContextLine(); ride != "" {
		blocks = append(blocks, ride)
	}
	return strings.Join(blocks, "\n\n")
}

// pendingTwinObservationsLine reports how many twin observations are
// teed up for the next reflect and which runs they came from — the one
// computed input behind the "staleness accumulated" criterion, which the
// agent can't cheaply derive itself (loadTwinFeedback filters against the
// reflect checkpoint's LastIngestAt). Returns "" when the feedback read
// fails; a project with no twin checkpoint reads as a first reflect, so
// with no committed feedback it gets the quiet "none pending" line.
//
// When an open twin run already exists, the line names it. Counting the
// observations without naming their destination is what let a pulse
// read a parked reflect as a finished job: it had the count, it had the
// run, and nothing connected the two to an action it could take. The
// slug turns the count into a thread the agent can groom or kick — the
// vocabulary the fragment teaches beside this.
func pendingTwinObservationsLine(root, projectID string) string {
	cfg, err := twinWikiBuilder(root, projectID)
	if err != nil || cfg == nil {
		return ""
	}
	feedback, err := loadTwinFeedback(root, projectID, *cfg)
	if err != nil {
		return ""
	}
	if len(feedback) == 0 {
		return "Twin-reflect context: no twin observations pending since the last reflect."
	}
	seen := map[string]bool{}
	var runs []string
	for _, fb := range feedback {
		if seen[fb.runID] {
			continue
		}
		seen[fb.runID] = true
		runs = append(runs, fb.runID)
	}
	line := fmt.Sprintf("Twin-reflect context: %d twin observation(s) pending since the last reflect, from %s.",
		len(feedback), strings.Join(runs, ", "))
	// Read failure is silent: the count is the load-bearing half, and a
	// scan that failed is not evidence that no twin run is open.
	if open, err := findInProgressTwinRun(root, projectID); err == nil && open != "" {
		line += fmt.Sprintf(" They are waiting on open twin run `%s/%s`, which stays parked until something rides it.", projectID, open)
	}
	return line
}

// errPulseSkipped is the sentinel openPulse's prompt builder returns
// when the operator's Ctrl-C latched between the post-Open checkpoint
// and the agent executor — a millisecond gap between setup children
// that kills no child and fails no step. The builder runs post-worktree,
// pre-executor, so returning here routes into runStageSession's
// bootstrap-failure path: the worktree is torn down and openPulse
// returns 1, which pulseSurvey (latch set, exit ≠ 130) reads as a
// pre-agent skip and disposes the run.
var errPulseSkipped = errors.New("pulse: skipped before the survey started")

// openPulse is the Go-level seam behind `moe pulse pulse` and the
// survey's headless execution. Read-only both-legs-strict sandbox (the
// design/chat shape): the survey reads the project but never edits it,
// and the boundary guard enforces that. It is a var so runPulseSurvey's
// auto-close can be tested without running the agent turn.
//
// pi is the survey's Ctrl-C latch (nil on the interactive `moe pulse
// pulse` path, which has no skip window). The prompt builder is the
// pre-executor belt: a Ctrl-C that latched during setup returns
// errPulseSkipped here so the agent never starts.
var openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, pulseDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			Agent:                  agentOverride,
			// Deferred so the twin-reflect context line renders against
			// the session worktree, the read-only copy runStageSession
			// hands the builder — the same deferral the twin stages use to
			// keep a pass off the operator's live checkout.
			InitialPromptBuilder: func(workRoot string, _ *wiki.Config, _ bool) (string, error) {
				if pi.interrupted() {
					return "", errPulseSkipped
				}
				return pulseKickoffWithContext(workRoot, projectID, runID, stderr), nil
			},
			CanvasSkeleton: pulseCanvasSkeleton,
		}, stdout, stderr)
}

func runPulseNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe pulse new <project>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the whole pulse for a project: opens every due chore's run")
		moePrintln(stderr, "(never executes one), then a headless read-only survey that files")
		moePrintln(stderr, "followups, writes a report, and may spawn and groom parked fix runs.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "pulse new: %v\n", err)
		return 1
	}
	// `moe pulse new` is the one place the pulse *is* the verb, so a skip
	// is the verb's own outcome: exit 130. (At a run-traffic tail the
	// verb's durable work already succeeded, so those callers keep their
	// own exit code and only thread the interrupt to halt a cascade.)
	code, interrupted := runPulse(root, projectID, "" /*spawner*/, stdout, stderr)
	if interrupted {
		return exitInterrupted
	}
	return code
}

func runPulseStage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse "+pulseDoc, flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe pulse "+pulseDoc+" [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive agent session on a pulse run's survey canvas —")
		moePrintln(stderr, "reopen a sweep to inspect it or re-run it by hand.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "pulse %s: %v\n", pulseDoc, err)
		return 2
	}
	// Interactive reopen: the operator owns this session's Ctrl-C, so no
	// skip latch (nil pi).
	return openPulse(projectID, runID, false, *agentOverride, nil /*pi*/, stdout, stderr)
}
