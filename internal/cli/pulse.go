package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// The pulse workflow is the level-3 "gather" primitive: a headless,
// read-only sweep of one project that files followup entries (→ ideas
// via the existing harvest) and writes a short report whose gate may
// spawn parked fix runs and groom queued work into lanes. It has no
// push — the artifact is the filed followups plus the canvas report.
//
// A pulse is more than the survey. Every invocation does three things:
//
//   - Always: open every due chore's run for the project (never execute
//     one) via openChoreInProcess. Automation acts on standing intent —
//     a chore the operator authored — but never makes a fresh decision.
//   - Always: reconcile the project's pushed runs against GitHub
//     (reconcileAtPulse), so a PR merged out of band reads as `merged`
//     before the survey looks at its delta.
//   - Every time: the survey — a blocking, headless stage that opens a
//     run, sweeps, files followups, writes its report, and auto-closes
//     itself on a clean exit. A failed or abandoned sweep leaves its run
//     open on the dash's ACTIVE list for a human to look at, but nothing
//     blocks the next survey — visible junk over invisible absence.
//
// Nothing fires a pulse in-process. Run-traffic verbs used to tail one
// at their chain's momentary tail, which meant a kicked ride's own tail
// re-entered the sweep and re-walked a board the outer kick loop was
// already walking. The pulse is now started only by a verb: `moe pulse
// new <project>` by hand, and `--dynamic` is the consent rung that makes
// it the verb a clock can call — grooming may kick what it grooms. `moe
// serve --dynamic` hosts the clock (see internal/serve/heartbeat.go),
// and an external cron can call the same verb.
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
	"The gate opens runs and orders them in one grammar: you write each run where it goes.\n\n" +
	"A `\"loose\"` list holds runs to open with no ordering opinion — they park standalone. Each entry is an object: " +
	"`{\"slug\": ..., \"title\": ..., \"why\": ..., \"design\": ...}`, defaulting to a `sdlc` fix run — the bar is mechanical, " +
	"bounded, and verifiable, all three, and the stage guidance holds it. A `loose` entry may instead set " +
	"`\"design_only\": true`, which opens the run, rides it one headless design turn and parks it for the operator — a " +
	"lower bar for a shorter ride, and the one place a finding that needs judgment rather than a fix may go; the stage " +
	"guidance holds that bar too. Either way, `why` is the one line the operator reads next to the verdict.\n\n" +
	"A `\"threads\"` list holds runs in execution order, each thread attached after an existing run (`\"onto\"`), under a " +
	"freshly named head (`\"head\"`), or self-rooted as its own thread (neither key). A thread's `\"runs\"` entry is either a **string** " +
	"naming any parked run in the project — naming one chained elsewhere moves it — or an **object** in the same shape as a " +
	"`loose` entry, which opens that run right at that position. This is where your ordering judgment goes; there is no prose " +
	"ranking section. The bar is the spawn bar plus ordering conviction: would the operator kick these, in this order, " +
	"unchanged? If the order is a guess, put the runs in `loose`. Under a dynamic sweep every kickable parked thread starts " +
	"when the sweep finishes — the ones you groom and the ones already in order — so the field you write is `\"park\"`: one " +
	"line naming why the operator should look at this thread first (an ordering you wouldn't defend, a speculative member, " +
	"an irreversible or outward-facing surface). No park means it runs, and that includes a run you wrote to `loose`, which " +
	"is a thread of one. `loose` says you have no ordering opinion; it does not say the work waits.\n\n" +
	"Omitting both lists is the normal outcome; a followup is the default channel for everything that doesn't clear the bar. " +
	"See the stage guidance."

// pulseCanvasSkeleton is the fixed structural shape the survey canvas
// opens with. The agent fills the sections in place. The gate's grammar
// — loose runs, threads, the bars each is held to — is taught
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

(agent fills: a fenced json block — set "status" once the survey concluded, and optionally "loose": [...] and "threads": [...]. This placeholder has no fence, so a no-op turn leaves the gate detectably unfilled.)
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
	// The pulse has no stage-opener verb. A sweep is machine-paced:
	// `moe pulse new` runs one, `cat` / `log` read it, `close` ends it.
	// There was a `moe pulse pulse <p>/<run>` door once, sold as "re-run
	// a failed sweep by hand", but it opened the session without applying
	// the resulting gate — a corrected gate typed into the canvas sealed
	// and did nothing. The retry is a fresh sweep, which is what the
	// heartbeat does on its own next tick anyway.
	//
	// Pulse has no workspace and no moe/<run> branch, so close has
	// nothing workflow-specific to clean up — pass nil and ride the
	// shared harvest / state-guard / status-flip path. The happy-path
	// survey auto-closes through this same registration (skipEdit, so
	// filings promote to ideas unreviewed); the verb itself is the manual
	// ending for a refused or failed sweep, where the editor-pop prune
	// gate still applies.
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
	// keys — stage-verb flags, chain edit — the sibling of chat's
	// SetPerpetual.
	w.SetMachinePaced()
	RegisterWorkflow(w)
}

// runPulse is the whole pulse: the deterministic chore auto-open (which
// opens runs but executes none), then the survey. emitRun is the file
// the survey names its run in, "" for every caller but a spawning serve
// (see pulseSurvey).
//
// It owns the pulse's scoped Ctrl-C latch (installPulseInterrupt): the
// "scanning — Ctrl-C to skip" banner prints up front, before the run is
// minted, so the operator knows the skip window is live from the start.
// The second return is whether the operator interrupted the sweep.
func runPulse(root, projectID, emitRun string, stdout, stderr io.Writer) (int, bool) {
	pi := installPulseInterrupt()
	defer pi.Close()
	moePrintf(stderr, "pulse: scanning %s — Ctrl-C to skip\n", projectID)
	autoOpenDueChores(root, projectID, pi, stdout, stderr)
	reconcileAtPulse(root, projectID, pi, stdout, stderr)
	code := runPulseSurvey(root, projectID, emitRun, pi, stdout, stderr)
	return code, pi.interrupted()
}

// reconcileAtPulse asks GitHub about this project's pushed runs and
// applies whatever landed out of band, so a PR the operator merged from
// their phone is `merged` in the journal *before* the survey reads its
// delta. Same walk `moe sync` does, scoped to one project — sync still
// owns pointer bumps; the pulse takes only the reconcile step.
//
// Warn-only like everything else in the pulse: a reconcile failure
// (offline, no gh, a wedged lock) must not derail the sweep. The
// repolock is taken here and held only for the walk — the survey's own
// run-open takes its own. Whatever the walk commits reaches origin on
// serve's pusher, so the common case (a project with nothing pushed)
// stays a disk-only scan with no network leg of its own.
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
		_, err := reconcilePushedRuns(root, projectID, stdout, stderr)
		return err
	})
	if err != nil {
		moePrintf(stderr, "pulse: reconcile pushed runs for %s: %v\n", projectID, err)
	}
}

// autoOpenDueChores opens every due chore's run for the project via the
// shared chore-open pipeline. No stage executes here — the run is parked
// at its first stage, and this sweep's own kick is what rides it: a
// chore-rooted run is settled by construction and operator-marked, so it
// clears the kick's admit bar without a click. The existing open-run
// refusal is the anti-pile-up guard, so a chore that already has an open
// run is skipped silently; any other failure warns and moves on (a chore
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
		if _, err := openChoreInProcess(root, projectID, s.Definition.Name, choreOpenNormal, stdout, stderr); err != nil {
			if _, ok := errors.AsType[*choreNotOpenableError](err); ok {
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
// Every invocation runs a fresh survey unconditionally — there is no
// rate limiter. On a clean survey it auto-closes its own run; a failed
// or SIGINT'd sweep leaves the run open on the dash's ACTIVE list
// (escalation by visibility), but does not block the next survey.
// Concurrent and piled-up pulse runs are allowed: run opening mints
// distinct dated slugs under the repolock, so parallel sweeps don't
// collide.
//
// The return describes the whole invocation, including a dynamic ride
// the completed survey started. Zero means the survey and every kicked
// thread finished cleanly (or the operator interrupted the survey, which
// the latch reports separately); non-zero means the survey died,
// concluded nothing, or a kicked ride stalled. The heartbeat's
// per-project backoff and the notify payload both read it through the
// child's exit status, so it is the only channel a failed unattended
// invocation has.
//
// Body assigned in init() rather than at declaration for the same
// init-order reason openPulseStage uses one: the auto-close arm traces
// pulseSurvey → closePulseRun → closeRunInProcess and back through the
// registered close, which the var-init dependency analyser reads as a
// cycle.
var runPulseSurvey func(root, projectID, emitRun string, pi *pulseInterrupt, stdout, stderr io.Writer) int

func init() {
	runPulseSurvey = pulseSurvey
}

func pulseSurvey(root, projectID, emitRun string, pi *pulseInterrupt, stdout, stderr io.Writer) int {
	// The survey run itself is top-level: no MoE-Spawned-By edge back to
	// anything. A pulse closes one generation and roots the next, so it
	// reads as the fencepost between two chains rather than a member of
	// either. Runs the pulse *mints* still carry SpawnedBy = <this pulse>
	// and fold under it — that edge is load-bearing for pulseSelfKick's
	// settled-design admit.

	// Checkpoint: a Ctrl-C before the run is minted skips with nothing to
	// clean — no run, no lock (runopen.Open's window hasn't opened yet).
	if pi.interrupted() {
		moePrintf(stderr, "pulse: skipped — no run opened for %s\n", projectID)
		return 0
	}

	md, err := runopen.Open(root, projectID, run.Options{
		IDBase:   pulseWorkflow,
		Workflow: pulseWorkflow,
		SeedDocs: map[string]string{pulseDoc: pulseCanvasSkeleton},
		// No MoE-Spawned-By (see above), so this is the one mint whose
		// consent stands alone rather than decorating a spawn edge —
		// and it is the first commit of the range a sweep's own exit
		// walks. Unstamped, the sweep's opening act read as the
		// operator's.
		Trailers: trailers.Block{Consent: walkConsent()},
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "pulse: open run for %s: %v\n", projectID, err)
		return 1
	}

	// The slug is minted here, inside the child, and a serve that spawned
	// this sweep has no other way to learn it — the exit code is the only
	// thing that crosses back on its own. emitRun is that channel: a
	// parameter rather than process state or an env var, so a concurrent
	// sweep in the same process could never name its run in a file serve
	// reads as this sweep's. Warn-only: an unwritable path costs a link,
	// not a sweep.
	if emitRun != "" {
		if err := os.WriteFile(emitRun, []byte(md.ID+"\n"), 0o644); err != nil {
			moePrintf(stderr, "pulse: emit run for %s: %v\n", projectID, err)
		}
	}

	// Checkpoint: a Ctrl-C landed while the run was being minted — dispose
	// the just-minted skeleton run so it doesn't linger on the dash with
	// nothing to review.
	if pi.interrupted() {
		disposePulseRun(root, projectID, md.ID, stdout, stderr)
		return 0
	}

	// An interrupt is never propagated as a failure — abandoning a sweep
	// is the operator's own act, not a broken one — but a survey that
	// *died* exits non-zero, because the sweep's outcome is the only
	// thing that crosses out of this process. The resident heartbeat runs
	// the sweep as `moe pulse new --dynamic <project>`, and its child's
	// exit status is what drives the per-project failure backoff and the
	// notify payload's ok bit. Reporting a dead sweep as a clean one
	// resets the backoff on the exact failure it was written for (a night
	// of exhausted plan limits) and tells a phone glance the sweep
	// succeeded. Either way the run stays open on the dash's ACTIVE list
	// for the operator, and either way the next survey is unblocked.
	survey := openPulse(projectID, md.ID, true /*headless*/, pi, stdout, stderr)
	if survey.code == exitInterrupted {
		// The Ctrl-C may have been observed only at the agent boundary, so
		// mark the latch to propagate the skip out as the verb's own exit.
		// Whether the run is disposed is agentStarted's call, below.
		pi.mark()
	}
	switch {
	case pi.interrupted() && !survey.agentStarted:
		// The latch is set and the agent never started: a Ctrl-C in a
		// millisecond gap between setup children tripped the pre-executor
		// belt (openPulse's prompt builder returned errPulseSkipped).
		// Nothing was surveyed, so dispose the just-minted run.
		disposePulseRun(root, projectID, md.ID, stdout, stderr)
		return 0
	case pi.interrupted():
		// The agent ran and the operator asked for the sweep to get out of
		// the way. The canvas may hold real findings, so it is *not*
		// disposed — but nor is it auto-closed, which would harvest
		// half-reviewed followups into ideas, and no gate action fires: an
		// interrupted sweep spawns, grooms and kicks nothing. The run
		// lingers on the dash's ACTIVE list for a human to look at.
		//
		// This is deliberately keyed on the agent having started rather
		// than on the exit code. A Ctrl-C that lands as the agent finishes
		// cleanly still exits 0 — inferring "the agent never ran" from
		// "exit ≠ 130" threw away a completed sweep.
		moePrintf(stderr, "pulse: interrupted — leaving %s/%s open for review\n", projectID, md.ID)
		return 0
	case survey.code != 0:
		// A failed sweep with no interrupt — the vendor died, the box lost
		// the network, the turn crashed. Leave the run open on the dash's
		// ACTIVE list (escalation by visibility) and report the failure
		// out, so a heartbeat sweep cools off instead of hot-looping into
		// the same wall every tick.
		return 1
	}

	// Read the survey's `## Gate` verdict. An unfilled or unparsable gate
	// — the skeleton placeholder, or a turn that exited 0 without writing
	// a real conclusion — means the sweep didn't actually conclude:
	// refuse the auto-close so the run lingers on the dash's ACTIVE list
	// (escalation by visibility), and skip the gate's spawns. Any parsed
	// non-empty status passes; a pulse has no ready/blocked vocabulary,
	// only close-or-linger.
	//
	// It reports out as a failure for the same reason the branch above
	// does. A vendor that hangs up mid-turn does not always exit
	// non-zero, and a sweep that concluded nothing is the shape that
	// arrives when it doesn't — counting it clean would reset the very
	// backoff meant to pace it.
	gate, err := readPulseGate(root, projectID, md.ID)
	if err != nil {
		moePrintf(stderr, "pulse: %s/%s left an unfilled gate (%v) — leaving the run open for review\n", projectID, md.ID, err)
		// Name the route out. A gate refusal is the common shape of a
		// failed sweep now that an unknown key is a strict-decode refusal
		// — any typo lands here — and there is no reopen-and-fix door:
		// the recovery is to end this run and run another. Harmless when
		// the heartbeat spawned the sweep (stderr goes to serve's logs),
		// and the whole point when the operator typed `moe pulse new`.
		moePrintf(stderr, "hint: moe pulse close %s/%s   (end this sweep; filings still harvest)\n", projectID, md.ID)
		moePrintf(stderr, "hint: moe pulse new %s   (run a fresh one)\n", projectID)
		return 1
	}
	// Mint, then groom, then kick. The order is the design's: the graph
	// can only be stamped once every run in it exists, and a kick must
	// not start until the thread it names has stopped moving.
	groups := applyPulseGate(root, projectID, md.ID, gate, stdout, stderr)
	groomed := groomChains(root, projectID, md.ID, groups, survey.chainEdges, stdout, stderr)

	// Clean sweep: auto-close the run so the next sweep starts from a
	// clean board. Route through the registered close (subject +
	// cleanup) so there's no parallel close path. skipEdit harvests
	// followups.md as-is — the filings promote to ideas unreviewed;
	// review moves to scrapping on the dash. A close failure warns and
	// leaves the run open, mirroring the pulse's warn-only posture
	// throughout: the report and filings are already durable on disk, so
	// a failed auto-close is a close-by-hand-later, not a lost sweep.
	//
	// A dynamic sweep also stamps the kick order it is about to walk onto
	// its own canvas, riding the close's cleanup so the section and the
	// status flip are one commit — the same fold disposePulseRun's skip
	// note proved out, and for the same reason: a stamp in its own commit
	// could fail while the close succeeded, leaving the closed report
	// silent about the queue. That silence is what three runs in four
	// days had to reconstruct by hand.
	canvasRel := run.ContentPath(projectID, md.ID, pulseDoc)
	canvas := filepath.Join(root, canvasRel)
	var before []byte
	var stamp closeCleanup
	if currentRideMode == rideDynamic {
		section := renderKickSection(planKick(root, groomed))
		stamp = func(root string, _ *run.Metadata, _, _ io.Writer) error {
			body, err := os.ReadFile(canvas)
			if err != nil {
				return fmt.Errorf("read pulse canvas: %w", err)
			}
			before = body
			appended := strings.TrimRight(string(body), "\n") + "\n\n" + section
			if err := os.WriteFile(canvas, []byte(appended), 0o644); err != nil {
				return fmt.Errorf("stamp kick section: %w", err)
			}
			return run.Stage(root, canvasRel)
		}
	}
	if err := closePulseRun(root, projectID, md.ID, stamp, stdout, stderr); err != nil {
		if before != nil {
			// The stamp got as far as rewriting the canvas. Put it back:
			// the dirty-tree gate is repo-wide, so a half-applied stamp
			// left on disk wedges every later close in the bureaucracy,
			// not just this run's.
			restorePulseRun(root, projectID, md.ID, canvas, before)
		}
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
	return pulseSelfKick(root, groomed, stdout, stderr)
}

// closePulseRun closes a pulse run through the registered close — the
// same subject the happy-path auto-close and the interrupt disposal both
// ride, so there's no parallel close path. skipEdit harvests
// followups.md as-is.
//
// cleanup is a parameter rather than the registration's own hook because
// pulse registered nil (pulse has no workspace and no branch to tear
// down — see the closeCommand registration above): the happy path passes
// nil and gets exactly the registered behavior, while the disposal
// passes the skip-note stamp so it rides the close's single lock and
// single commit instead of taking its own.
func closePulseRun(root, projectID, runID string, cleanup closeCleanup, stdout, stderr io.Writer) error {
	reg, ok := lookupCloseRegistration(pulseWorkflow)
	if !ok {
		return fmt.Errorf("no close registration for %q", pulseWorkflow)
	}
	return closeRunInProcess(root, pulseWorkflow, reg.subject, cleanup,
		projectID, runID, true /*skipEdit*/, stdout, stderr)
}

// disposePulseRun closes a just-minted pulse run the operator Ctrl-C'd
// before the survey could produce anything worth reviewing. The skeleton
// canvas is non-empty so the close-time canvas gate passes, and the
// canonical root is committed-clean at that point so the dirty-tree gate
// passes.
//
// The skip note rides in as the close's cleanup, which runs inside the
// close's locked closure — after the dirty-tree gate, before the status
// flip — so the staged canvas lands in the *same* commit that marks the
// run closed. That fold is the point: a stamp that lived in
// its own lock and its own commit could fail while the close went on to
// succeed, and the run would close with the raw skeleton and only an
// ephemeral warn to say why. Skeleton-closed is now unreachable from any
// stamp-capable binary, which makes it an unambiguous signature of a
// stale one — the diagnosis this run had to reconstruct from shell
// history.
//
// The trade is deliberate and reverses the stamp's original posture: a
// failed stamp now leaves the run open on the dash's ACTIVE list instead
// of closing it with a decoy canvas. Escalation by visibility is the
// pulse's standard failure mode, disposal only fires on the operator's
// own Ctrl-C, and a decoy report has cost three diagnosis cycles.
func disposePulseRun(root, projectID, runID string, stdout, stderr io.Writer) {
	canvasRel := run.ContentPath(projectID, runID, pulseDoc)
	canvas := filepath.Join(root, canvasRel)
	var before []byte
	stamp := func(root string, md *run.Metadata, stdout, stderr io.Writer) error {
		body, err := os.ReadFile(canvas)
		if err != nil {
			return fmt.Errorf("read pulse canvas: %w", err)
		}
		before = body
		if err := os.WriteFile(canvas, []byte(pulseSkipNote), 0o644); err != nil {
			return fmt.Errorf("stamp skip note: %w", err)
		}
		// Staging is what carries the note into the close's commit:
		// StageAndCommit's `git commit -m` takes the whole index, so the
		// note and the status flip are one commit.
		return run.Stage(root, canvasRel)
	}
	if err := closePulseRun(root, projectID, runID, stamp, stdout, stderr); err != nil {
		restorePulseRun(root, projectID, runID, canvas, before)
		moePrintf(stderr, "pulse: skip-close %s/%s: %v — leaving run open\n", projectID, runID, err)
		return
	}
	moePrintf(stderr, "pulse: skipped — closed %s/%s\n", projectID, runID)
}

// restorePulseRun puts a failed close's run back the way it was: the
// canvas bytes a stamp overwrote, and whatever the stamp or the close
// left in the index. Both canvas stamps ride it — the disposal's skip
// note and the happy path's kick section — because the repair is a
// property of stamping inside a close that can fail, not of either
// stamp's content. before is the caller's evidence that a stamp got as
// far as writing. The status flip needs no repair here: a failed close
// commit walks its own flip back (commitTerminal), so the run comes back
// from closePulseRun already open.
//
// Repairing is not optional. The dirty-tree gate is repo-wide, so a
// half-applied stamp left on disk wedges every later close in the
// bureaucracy, not just this run's — and a run.json that says closed
// over a skeleton canvas, with no commit to say why, is the exact decoy
// the fold exists to make impossible. Best-effort throughout: the close
// already failed, and there is no better error to report than the one
// the caller is about to print.
//
// Deliberately unlocked. The close released the repolock on its way out,
// and this only rewrites files this process itself half-wrote; the
// index-lock retry in internal/git covers the git-level race.
func restorePulseRun(root, projectID, runID, canvas string, before []byte) {
	if before != nil {
		_ = os.WriteFile(canvas, before, 0o644)
	}
	// One reset over the whole run dir rather than per-path: it covers
	// the canvas, run.json, and anything the harvest staged before the
	// commit failed.
	_ = git.Run(root, "reset", "-q", "--", run.Dir(projectID, runID))
}

// pulseSkipNote replaces the untouched skeleton on a disposed run's
// canvas. Closed with the skeleton, the run is indistinguishable on disk
// from a crashed no-op sweep — which is what the `## Gate` check exists
// to leave open — so the next survey reads it as a live bug and files
// against it. (That misread has already cost an idea, a promotion and a
// design turn.) The note says what happened, so the artifact stops being
// a decoy and the "read the previous pulse report" baseline knows to
// fall back a report.
const pulseSkipNote = `# Pulse

Sweep skipped — the operator interrupted the pulse before the survey
started. No report; the previous pulse's report remains the baseline.
`

// pulseGate is the machine-readable verdict the survey agent writes to
// the canvas's `## Gate` section. A non-empty status is all the
// auto-close decision needs — a pulse has no ready/blocked advance
// vocabulary, only close-or-linger.
//
// Spawning and ordering are one grammar. The gate used to split them
// into two lists that named each other through a shared slug namespace,
// which is why it needed aliases — and why a twin entry's agent-chosen
// alias could silently shadow another entry, or a real parked run, and
// order the wrong thing. A run is now written *where it goes*: inline in
// a thread's `runs` at its position, or in `loose` when the survey has
// no ordering opinion. Placement is positional, so nothing has to name
// anything.
type pulseGate struct {
	Status string `json:"status"`
	// Loose carries runs to open with no ordering claim attached. They
	// park standalone and unchained — the normal outcome for work whose
	// order the survey isn't sure of. It stays a separate key rather
	// than folding into a self-rooted thread because "park it, I have no
	// opinion" and "root a new thread with this" are different
	// judgements.
	Loose []pulseRunSpec `json:"loose"`
	// Threads carries the survey's ordering opinion: runs in execution
	// order, each group placed after an existing run, under a freshly
	// named head, or self-rooted. See pulse_groom.go. The lane bar
	// prices this separately from the spawn bar.
	Threads []pulseThread `json:"threads"`
}

// pulseRunSpec is one run the survey asks the harness to open. Slug is
// the slug base (the harness dates it on collision); Title and Why are
// what the operator reads on the chain canvas before kicking; Design
// seeds the new run's design canvas, so the design stage starts from
// the survey's findings instead of re-deriving them.
//
// Workflow picks the mint path and defaults to "sdlc" when empty. The
// allowlist is sdlc alone: the only workflow a pulse has a reason to
// propose fresh (chat is perpetual, pulse would be recursion). The
// field outlives the twin reflect it once also named so a survey that
// still writes `"workflow": "twin"` is told it isn't spawnable rather
// than having the key silently ignored.
//
// Chore names a judged chore instead, and is exclusive with every other
// field: the survey's claim is only "the condition the operator wrote
// holds", and everything about the resulting run — workflow, seed,
// cooldown — comes from the chore's own definition. Why stays the one
// line the operator reads.
//
// DesignOnly lowers the bar and shortens the ride in one move: the
// Design body is a *brief*, the harness rides the run exactly one
// stage — a headless design turn — and then parks it for the operator
// the way an operator-minted run whose design closed is parked. It is a
// field on the grammar that already carried the seed rather than a
// fifth gate key, so slug validation and the live-slug dedupe come
// along for free. Four shapes warn and skip it (see mint and
// applyPulseGate): a chore entry, a slug matching a live idea,
// a thread position, and — on a fresh slug only — a spec with no
// Design body. A slug matching a design-only *tagged* idea is the one
// live match that promotes: the idea's canvas is the brief, so the
// missing body costs nothing.
type pulseRunSpec struct {
	Slug       string `json:"slug"`
	Workflow   string `json:"workflow"`
	Title      string `json:"title"`
	Why        string `json:"why"`
	Design     string `json:"design"`
	Chore      string `json:"chore"`
	DesignOnly bool   `json:"design_only"`
}

// pulseThread is one entry in the gate's `threads` list: runs in
// execution order, plus where the thread goes.
//
// Onto attaches the group after that run, wherever it sits. Head mints
// a chain placeholder with that slug base and chains the group under it.
// Neither self-roots the group as its own headless thread — a dynamic
// sweep's kick loop starts it, anything else parks it. Onto and Head
// together is a warn-and-skip: they are two different answers to the
// same question.
//
// Park is the survey's one-line reason the operator should look at this
// thread before it runs. Non-empty parks the thread; absent lets it kick
// once grooming is done, which is the default under a dynamic sweep and
// impossible without one (see pulseSelfKick). Park is the marked case
// because the error ledger only ever ran one way — strandings, never a
// runaway — so the field the survey has to spend a sentence on is the
// one that stops motion, not the one that causes it.
type pulseThread struct {
	Onto string             `json:"onto"`
	Head string             `json:"head"`
	Runs []pulseThreadEntry `json:"runs"`
	Park string             `json:"park"`
}

// pulseThreadEntry is one position in a thread: a bare string naming a
// parked run that already exists, an object minting one right there, or
// an existing run carrying a question for the operator. The shapes are
// the whole reason the alias map could be deleted — a run that doesn't
// exist yet is described where it belongs rather than declared elsewhere
// and referenced by name, and a question is asked *at* the run whose
// future agent needs the answer rather than through a second gate field
// that names it.
type pulseThreadEntry struct {
	// Existing names a parked run in the project, resolved at apply
	// time. Set for the bare-string form and for the ask form.
	Existing string
	// Spec describes a run to mint at this position. Set only for the
	// mint form.
	Spec *pulseRunSpec
	// Ask is one prose question to open on Existing:
	//
	//	{"run": "change-auth-defaults", "ask": "Which policy — …?"}
	//
	// The `run` key is the discriminator: an inline mint spec has `slug`,
	// never `run`, so the two object forms can't be confused.
	// Deliberately positional — nothing here names a run in another gate
	// field.
	//
	// Asking holds nothing. A survey that needs the answer before the
	// work also writes the thread's `park`, naming the question; the two
	// are separate acts because a question the work need not wait for is
	// a question worth asking.
	Ask string
}

// specs lists every run the gate asked the harness to open, in document
// order: the loose ones, then each thread's inline mints. The reader for
// anything that wants "what did this sweep propose" without caring where
// it landed.
func (g pulseGate) specs() []pulseRunSpec {
	out := append([]pulseRunSpec(nil), g.Loose...)
	for _, th := range g.Threads {
		for _, entry := range th.Runs {
			if entry.Spec != nil {
				out = append(out, *entry.Spec)
			}
		}
	}
	return out
}

func (e *pulseThreadEntry) UnmarshalJSON(b []byte) error {
	var slug string
	if err := json.Unmarshal(b, &slug); err == nil {
		e.Existing, e.Spec, e.Ask = slug, nil, ""
		return nil
	}
	// Probe for the `run` discriminator before committing to a branch,
	// rather than trying each shape in sequence. Try-in-sequence let an
	// ask form with a typo'd key drift into the spec branch and out the
	// other side as an empty spec: the question the survey wrote for the
	// operator vanished, and nothing said so. An object carrying `run` is
	// an ask form or an error — never a spec.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return fmt.Errorf("thread entry is neither a run slug nor a run spec: %w", err)
	}
	if _, isAsk := keys["run"]; isAsk {
		var ask struct {
			Run string `json:"run"`
			Ask string `json:"ask"`
		}
		if err := decodeStrict(b, &ask); err != nil {
			return fmt.Errorf("%s: %w", threadEntryLabel(keys, "run"), err)
		}
		if ask.Run == "" {
			return errors.New(`thread entry has an empty "run"`)
		}
		// `{"run": "x"}` with no ask is the string form written long-hand
		// and means exactly that: a position, no question.
		e.Existing, e.Spec, e.Ask = ask.Run, nil, ask.Ask
		return nil
	}
	var spec pulseRunSpec
	if err := decodeStrict(b, &spec); err != nil {
		return fmt.Errorf("%s: %w", threadEntryLabel(keys, "slug"), err)
	}
	e.Existing, e.Spec, e.Ask = "", &spec, ""
	return nil
}

// threadEntryLabel names the offending entry in a decode error using
// whichever identifying key survived. A long thread's parse error is
// useless if the operator can't tell which position it came from.
func threadEntryLabel(keys map[string]json.RawMessage, key string) string {
	var s string
	if err := json.Unmarshal(keys[key], &s); err != nil || s == "" {
		return "thread entry"
	}
	return fmt.Sprintf("thread entry %q", s)
}

// decodeStrict unmarshals one JSON value with unknown keys rejected. The
// gate grammar is closed: a key nobody defined means the survey wrote
// something the harness can't see, and the keys it does recognise are no
// basis for guessing what the rest meant.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

// readPulseGate reads the survey canvas and parses its `## Gate` JSON
// fence (the shared `stageGateJSON` grammar). The error is non-nil for
// every no-op shape the auto-close refusal keys on: a missing/unreadable
// canvas, an absent or empty fence (the skeleton placeholder),
// unparseable JSON, or an empty status. A read error reads as unfilled —
// the run lingers rather than auto-closing on a canvas we couldn't
// inspect. The reason comes back as an error rather than flattening to a
// bool so the refusal can name the shape it hit; the operator would
// otherwise diff the fence by eye.
//
// The decode is strict, so an unknown key anywhere in the grammar
// refuses the whole gate too. Grammar failures refuse everything —
// nothing in the fence said what the writer meant, so there is no
// trustworthy remainder. Semantic failures stay warn-and-continue at
// apply time (a slug that collides, a named run that doesn't exist):
// those are per-entry judgements the rest of the gate survives.
func readPulseGate(root, projectID, runID string) (pulseGate, error) {
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(projectID, runID, pulseDoc)))
	if err != nil {
		return pulseGate{}, err
	}
	payload, ok := stageGateJSON(string(body))
	if !ok {
		return pulseGate{}, errors.New("no `## Gate` json fence")
	}
	var g pulseGate
	if err := decodeStrict(payload, &g); err != nil {
		return pulseGate{}, err
	}
	if g.Status == "" {
		return pulseGate{}, errors.New("gate has no status")
	}
	return g, nil
}

// applyPulseGate opens every run the gate's specs describe and hands
// back the groom groups its threads imply. Two walks over one minter, in
// document order — `loose` first, then each thread's `runs` — so a slug
// proposed twice in one gate hits the dedupe rather than minting a dated
// sibling.
//
// Both spellings of a thread entry reach the minter: an object spec
// mints, and a string names a parked run — which may be a tagged idea,
// and then promotion is what "opens" it. Only the string that resolves
// to nothing promotable travels on as a slug for the groom.
//
// A minted run travels onward as its own run id rather than as a name in
// a shared namespace. That is the whole of the alias map's replacement:
// the gate's two lists used to name each other by slug, so the mapping
// had to exist, and every write into it was last-write-wins.
//
// Warn-only throughout: a spec that can't be minted drops with a stderr
// line and the rest of the sweep carries on. The report and filings are
// already durable, so a failed mint is a spawn-by-hand-later, not a lost
// sweep.
func applyPulseGate(root, projectID, pulseSlug string, gate pulseGate, stdout, stderr io.Writer) []groomGroup {
	m := &pulseMinter{root: root, projectID: projectID, pulseSlug: pulseSlug}
	for _, spec := range gate.Loose {
		m.mint(spec, stdout, stderr)
	}
	groups := make([]groomGroup, 0, len(gate.Threads))
	for _, th := range gate.Threads {
		grp := groomGroup{Onto: th.Onto, Head: th.Head, Park: th.Park}
		for _, entry := range th.Runs {
			if entry.Spec == nil {
				// A string entry names "any parked run in the project", and a
				// tagged idea is one — the survey that nominated all three
				// parked ideas on 2026-08-13 spelled every entry this way and
				// the groom dropped all three, because resolveMember admits
				// only chainable runs. So promotion gets its turn here too:
				// the grammar offers two spellings and both have to work.
				// Anything that isn't a promotable idea falls through to the
				// slug it always was — an ordinary parked run is the common
				// case and stays the groom's to resolve.
				if id, _ := m.promoteIfTaggedIdea(entry.Existing, pulseRunSpec{Why: "named at a thread position"}, true /*atThread*/, stdout, stderr); id != "" {
					grp.Runs = append(grp.Runs, groomMember{mintedID: id})
					// A ping lands only on a run that already existed when the
					// survey wrote the gate. A promotion mints a different
					// run than the one the question was written against, so
					// the ask drops with a line rather than landing somewhere
					// the survey never looked.
					if entry.Ask != "" {
						moePrintf(stderr, "pulse: input: %s promoted to %s — question dropped\n", entry.Existing, id)
					}
					continue
				}
				grp.Runs = append(grp.Runs, groomMember{slug: entry.Existing})
				if entry.Ask != "" {
					askThreadEntry(root, projectID, pulseSlug, entry.Existing, entry.Ask, stderr)
				}
				continue
			}
			// Design-only is loose-only. Such a root is unsettled by
			// definition until the operator advances it, so heading a
			// thread with one would strand every member behind an
			// unwritten design — the exact shape the held-head groom
			// exists to undo, proposed on purpose. Skipped here rather
			// than in mint because only the caller knows the position.
			if entry.Spec.DesignOnly {
				moePrintf(stderr, "pulse: spawn: entry %q asks for design_only at a thread position — a design-only root strands the runs behind it; put it in loose. Skipping\n", entry.Spec.Slug)
				continue
			}
			// Mint in place. A spec that fails leaves a hole in the order
			// rather than shifting the rest — the warn line names it, and
			// the runs around it keep the positions the survey gave them.
			if id := m.mint(*entry.Spec, stdout, stderr); id != "" {
				grp.Runs = append(grp.Runs, groomMember{mintedID: id})
			}
		}
		groups = append(groups, grp)
	}
	return groups
}

// askThreadEntry opens the survey's ping on the run sitting at this
// thread position. Warn-only, like everything else in applyPulseGate: a
// refused ask costs the operator a stderr line and the next sweep's
// attention, nothing more.
//
// It holds nothing, deliberately. A survey that needs the answer before
// the work writes the thread's `park` too — the two are separate acts,
// so a question the work need not wait for is still askable.
//
// The refusals are the design's scope fence made mechanical. A chain
// placeholder has no agent turn to deliver the answer to, so it is not
// somewhere a question can usefully live; input.Ask covers the rest —
// a terminal run, an empty question, a run that already has one open.
func askThreadEntry(root, projectID, pulseSlug, slug, question string, stderr io.Writer) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "pulse: input: %s/%s: %v\n", projectID, slug, err)
		return
	}
	if md.Workflow == chainWorkflow {
		moePrintf(stderr, "pulse: input: %s/%s is a chain head — no stage to deliver an answer to; question dropped\n", projectID, slug)
		return
	}
	e, err := input.Ask(root, projectID, slug, projectID+"/"+pulseSlug,
		question, walkConsent(), io.Discard, stderr)
	if err != nil {
		moePrintf(stderr, "pulse: input: %s/%s: %v\n", projectID, slug, err)
		return
	}
	moePrintf(stderr, "pulse: input: asked %s/%s#%d — %s\n", projectID, slug, e.ID, e.Question)
}

// pulseMinter opens the runs one sweep's gate asks for. It holds the
// live-slug dedupe set across the whole gate, scanning once on first
// use, and claims each base it mints so a repeat proposal in the same
// gate is a skip rather than a dated sibling.
type pulseMinter struct {
	root      string
	projectID string
	pulseSlug string
	live      []string
	scanned   bool
	broken    bool
}

// mint opens one run for one spec and returns its id, or "" when the
// spec was skipped.
//
// A `chore` spec takes its own path (nominateChore) — it opens no fresh
// run of the survey's design, only the one the operator already
// registered. Everything below is the fresh-run path.
//
// Dispatch is per-workflow, and sdlc (the default) is the only one that
// spawns. Anything else warns and skips.
//
// No numeric cap. The harness has no basis for judging which proposals
// to trim, and parked is itself the review gate: spawned runs are
// visible on the dash and prunable with `moe chain edit`. Over-proposal
// is visible junk, which the pulse already prefers to invisible absence.
// The bar — mechanical, bounded, verifiable — is taught in the stage
// fragment, where judgment belongs.
//
// The one mechanical exception to dedupe is a live idea carrying a
// harvested follow-up's workflow tag: the pulse promotes that capture
// through the same seam as `--from-idea`. Untagged ideas remain behind a
// structural human-triage fence. Every other live match still skips.
func (m *pulseMinter) mint(s pulseRunSpec, stdout, stderr io.Writer) string {
	projectID, pulseSlug := m.projectID, m.pulseSlug
	spawnedBy := projectID + "/" + pulseSlug

	// A chore nomination names a registration, not a slug to mint, so it
	// dispatches ahead of the slug validation the sdlc path applies.
	if choreName := strings.TrimSpace(s.Chore); choreName != "" {
		return m.nominateChore(choreName, s, stdout, stderr)
	}
	slug := strings.TrimSpace(s.Slug)
	// Dispatch on workflow before the slug check, the same way a chore
	// entry dispatches ahead of it.
	switch workflow := strings.TrimSpace(s.Workflow); workflow {
	case "", "sdlc":
		if slug == "" || run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: spawn: skipping entry with unusable slug %q\n", s.Slug)
			return ""
		}
	default:
		moePrintf(stderr, "pulse: spawn: entry %q asks for workflow %q — only sdlc is spawnable; skipping\n", slug, workflow)
		return ""
	}
	if !m.ensureLive(stderr) {
		return ""
	}
	if slugBaseMatches(m.live, slug) {
		// A live match is either an ordinary run already in flight, which
		// promoteOrSkip calls a duplicate, or a tagged idea, which it
		// promotes. What `design_only` means there depends on the idea's
		// own licence, so the refusal lives in promoteIfTaggedIdea, where
		// the idea has been identified.
		return m.promoteOrSkip(slug, s, stdout, stderr)
	}
	// A design-only spec buys a design turn with the brief it carries.
	// Without one the seed is a title and a why — the one-line idea
	// this rung exists to replace — and the turn would spend itself
	// re-deriving what the survey already knew. Checked below the
	// live-slug dispatch because only a fresh mint makes the spec the
	// seed: a tagged idea supplies its own brief on its canvas, and
	// every other live match is a duplicate skip.
	if s.DesignOnly && strings.TrimSpace(s.Design) == "" {
		moePrintf(stderr, "pulse: spawn: entry %q asks for design_only with no design body — the brief is the point; skipping\n", slug)
		return ""
	}
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = slug
	}
	md, err := runopen.Open(m.root, projectID, run.Options{
		IDBase:     slug,
		Workflow:   "sdlc",
		SeedDocs:   map[string]string{"design": spawnDesignSeed(title, s)},
		SpawnedBy:  spawnedBy,
		DesignOnly: s.DesignOnly,
		Trailers: trailers.Block{
			SpawnedBy: spawnedBy,
			Consent:   spawnConsent(spawnedBy),
		},
	}, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "pulse: spawn %q for %s: %v\n", slug, projectID, err)
		return ""
	}
	m.live = append(m.live, md.ID)
	kind := "fix run"
	if s.DesignOnly {
		kind = "design-only run"
	}
	moePrintf(stderr, "pulse: spawned %s %s/%s (%s)\n", kind, projectID, md.ID, title)
	return md.ID
}

// nominateChore opens a judged chore's run for a gate entry that says
// the chore's condition holds. It is a nomination, not a create: with
// the chore's run already open, openChoreInProcess refuses with the
// open run's id and the nomination maps onto it, so a `chore` spec
// written at a thread position places the existing run instead of
// dropping.
//
// The opened run is an ordinary chore run — the chore's own workflow,
// its `prompt.md` seed, the MoE-Chore trailer — so completion, cooldown
// and the open-run guard all keep working exactly as they do for a
// mechanical chore. What the survey replaced is only the due check;
// choreOpenNominated keeps the cooldown and refuses a mechanical chore
// outright.
//
// Warn-only throughout, like the rest of the gate: a refusal or a
// failure drops this entry and the sweep carries on.
func (m *pulseMinter) nominateChore(name string, s pulseRunSpec, stdout, stderr io.Writer) string {
	for _, extra := range []struct{ field, value string }{
		{"slug", s.Slug}, {"workflow", s.Workflow}, {"title", s.Title}, {"design", s.Design},
	} {
		if strings.TrimSpace(extra.value) != "" {
			moePrintf(stderr, "pulse: chore: ignoring %s on chore entry %q; a chore run's shape comes from its own definition\n", extra.field, name)
		}
	}
	if s.DesignOnly {
		moePrintf(stderr, "pulse: chore: ignoring design_only on chore entry %q; a chore run's shape comes from its own definition\n", name)
	}
	res, err := openChoreInProcess(m.root, m.projectID, name, choreOpenNominated, stdout, stderr)
	if err != nil {
		var notOpenable *choreNotOpenableError
		if errors.As(err, &notOpenable) && notOpenable.OpenRun != "" {
			// Logged rather than silent so the sweep output stays honest
			// about which run the nomination — and the thread position
			// holding it — landed on.
			moePrintf(stderr, "pulse: chore %s/%s already open as %s/%s — mapped (%s)\n",
				m.projectID, name, m.projectID, notOpenable.OpenRun, s.Why)
			return notOpenable.OpenRun
		}
		moePrintf(stderr, "pulse: chore %s/%s not opened — %v\n", m.projectID, name, err)
		return ""
	}
	moePrintf(stderr, "pulse: judged chore met — opened %s/%s (%s)\n", m.projectID, res.Metadata.ID, s.Why)
	return res.Metadata.ID
}

// promoteOrSkip handles the one live-slug match that isn't a duplicate:
// a tagged idea, which the pulse promotes through the same seam
// `--from-idea` uses. Everything else already has this work in flight
// and skips.
func (m *pulseMinter) promoteOrSkip(slug string, s pulseRunSpec, stdout, stderr io.Writer) string {
	id, dup := m.promoteIfTaggedIdea(slug, s, false /*atThread*/, stdout, stderr)
	if dup {
		moePrintf(stderr, "pulse: spawn: %s already has a live run for %q — skipping\n", m.projectID, slug)
	}
	return id
}

// promoteIfTaggedIdea promotes the tagged idea named by slug and returns
// the destination run's id, or "" when slug doesn't name one. It is the
// promotion seam both entries into the gate share: a spec whose slug
// collided with a live run (promoteOrSkip, above) and a thread's string
// entry naming a parked run (applyPulseGate) are the same question asked
// from two grammars, and a tagged idea is a promotable answer to both.
//
// dup reports the one case a caller has to speak to itself: slug names
// live work that isn't a lone idea. What that means depends on the
// grammar — a duplicate proposal to the spec path, an ordinary parked
// run to the thread path — so it's returned rather than warned about
// here. Every other refusal (an unreadable scan, an untagged idea, an
// unusable tag, a design-only idea at a thread position, a failed
// promotion) has already warned by the time this returns "".
//
// atThread says the caller is applyPulseGate's string-entry branch,
// which is a position a design-only root can't hold.
func (m *pulseMinter) promoteIfTaggedIdea(slug string, s pulseRunSpec, atThread bool, stdout, stderr io.Writer) (id string, dup bool) {
	projectID, pulseSlug := m.projectID, m.pulseSlug
	spawnedBy := projectID + "/" + pulseSlug

	matches, err := matchingLiveRuns(m.root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "pulse: spawn: scan live match for %s/%s: %v\n", projectID, slug, err)
		return "", false
	}
	if len(matches) != 1 || matches[0].Workflow != dash.IdeaWorkflow {
		return "", true
	}
	idea := matches[0]
	if idea.PromoteTo == "" {
		moePrintf(stderr, "pulse: spawn: idea %s/%s is untagged and requires operator triage — skipping\n", projectID, idea.ID)
		return "", false
	}
	wf, lookupErr := LookupWorkflow(idea.PromoteTo)
	if lookupErr != nil || !chainableWorkflow(idea.PromoteTo) || len(wf.Stages()) == 0 {
		moePrintf(stderr, "pulse: spawn: idea %s/%s has unusable workflow tag %q — skipping\n", projectID, idea.ID, idea.PromoteTo)
		return "", false
	}
	// The same rule a `design_only` spec gets at a thread position, on
	// the tag that carries the bit instead: the promoted root is
	// unsettled by definition until the operator advances it, so every
	// member behind it would strand. The next sweep may propose the idea
	// loose, which is where design-only work belongs.
	if idea.DesignOnly && atThread {
		moePrintf(stderr, "pulse: spawn: idea %s/%s is tagged design-only and was named at a thread position — a design-only root strands the runs behind it; put it in loose. Skipping\n", projectID, idea.ID)
		return "", false
	}
	// A spec that asks for design_only on a plain-tagged idea is trying
	// to narrow the operator's ship licence to a design turn; refuse it,
	// same as design_only on any other live slug. On an idea that
	// already carries the bit the two agree, and refusing would make the
	// operator's own licence unusable the moment a survey quotes the
	// board it reads "design only" from.
	if s.DesignOnly {
		if !idea.DesignOnly {
			moePrintf(stderr, "pulse: spawn: entry %q asks for design_only but the slug names tagged idea %s/%s — the tag is the operator's licence to spend; skipping\n", slug, projectID, idea.ID)
			return "", false
		}
		moePrintf(stderr, "pulse: spawn: ignoring design_only for tagged idea %s/%s; the tag carries it\n", projectID, idea.ID)
	}
	if strings.TrimSpace(s.Design) != "" {
		moePrintf(stderr, "pulse: spawn: ignoring design body for tagged idea %s/%s; the idea canvas is the seed\n", projectID, idea.ID)
	}
	promoted, promoteErr := runopen.Promote(m.root, projectID, idea.ID, runopen.PromoteOptions{
		Workflow:   idea.PromoteTo,
		FirstStage: wf.Stages()[0],
		SpawnedBy:  spawnedBy,
		Consent:    spawnConsent(spawnedBy),
		DesignOnly: idea.DesignOnly,
	}, stdout, stderr)
	if promoteErr != nil {
		moePrintf(stderr, "pulse: promote tagged idea %s/%s: %v\n", projectID, idea.ID, promoteErr)
		return "", false
	}
	if promoted.MarkErr != nil {
		moePrintf(stderr, "pulse: warning: promoted %s/%s but could not mark the idea: %v\n", projectID, promoted.Run.ID, promoted.MarkErr)
	}
	m.live = append(m.live, promoted.Run.ID)
	kind := idea.PromoteTo
	if idea.DesignOnly {
		kind += ", design only"
	}
	moePrintf(stderr, "pulse: promoted tagged idea %s/%s to %s run %s/%s\n", projectID, idea.ID, kind, projectID, promoted.Run.ID)
	return promoted.Run.ID, false
}

// ensureLive loads the dedupe set on first use, so a gate whose specs
// are all chore nominations or non-sdlc skips (or which has none at
// all) pays for no scan.
// A failed scan is remembered: without the dedupe set every sdlc mint
// would be unguarded, so the whole gate's sdlc half is refused rather
// than re-scanning per spec.
func (m *pulseMinter) ensureLive(stderr io.Writer) bool {
	if m.broken {
		return false
	}
	if m.scanned {
		return true
	}
	live, err := liveSlugs(m.root, m.projectID)
	if err != nil {
		moePrintf(stderr, "pulse: spawn: scan runs for %s: %v\n", m.projectID, err)
		m.broken = true
		return false
	}
	m.live, m.scanned = live, true
	return true
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
func spawnDesignSeed(title string, s pulseRunSpec) string {
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

// pulseKickoffWithContext appends the harness-computed context to the
// static kickoff: six blocks (GitHub, recently-settled runs, chain
// state, advanced-and-left runs, openable judged chores, the input
// channel) and two trailing lines (what this invocation licensed, and
// what the project's standing mode does with that licence). Wired as
// InitialPromptBuilder, so root is the session worktree
// runStageSession hands the builder. Best-effort throughout: a gather
// that fails drops its own block rather than failing the sweep.
func pulseKickoffWithContext(root, projectID, runID string, stderr io.Writer) (string, map[string]string) {
	blocks := []string{pulseKickoff}
	// Every block wants the same two reads. Doing them once here is not
	// just cheaper — it means the blocks describe one consistent moment
	// rather than six successive ones.
	sc, ok := newPulseScan(root)
	if !ok {
		// Best-effort like each block was individually: a sweep with no
		// context blocks is a worse sweep, not a failed one.
		moePrintf(stderr, "pulse: kickoff: could not read runs for %s — context blocks dropped\n", projectID)
		return strings.Join(blocks, "\n\n"), nil
	}
	if gh := pulseGitHubContext(sc, projectID, runID, stderr); gh != "" {
		blocks = append(blocks, gh)
	}
	if settled := settledRunsBlock(sc, projectID); settled != "" {
		blocks = append(blocks, settled)
	}
	if chains := chainStateBlock(sc, projectID); chains != "" {
		blocks = append(blocks, chains)
	}
	if advanced := advancedRunsBlock(sc, projectID); advanced != "" {
		blocks = append(blocks, advanced)
	}
	if judged := judgedChoresBlock(sc, projectID); judged != "" {
		blocks = append(blocks, judged)
	}
	// The input record: what the operator has pushed at the board, what
	// the board has asked them, and the bar for asking. Beside the chore
	// block rather than after the ride line — both are "standing operator
	// state this sweep has to account for", and neither depends on what
	// this invocation licensed.
	if inputs := pendingInputBlock(sc, projectID); inputs != "" {
		blocks = append(blocks, inputs)
	}
	// Its own block, not a tail on the chain-state one: the line is about
	// what this sweep may start, which is true whether or not the board
	// happens to hold an active chain of two or more. Nested under the
	// chain-state block it reached the agent only by coincidence.
	if ride := rideModeContextLine(); ride != "" {
		blocks = append(blocks, ride)
	}
	// Beside it rather than folded into it: the ride line says what this
	// invocation licensed, and this says what the project's standing mode
	// does with that licence.
	if mode := projectModeContextLine(sc.root, projectID); mode != "" {
		blocks = append(blocks, mode)
	}
	// The chain edge set as the agent saw it. The sweep's ordering
	// opinion is formed against exactly this picture, so it is what the
	// apply step checks itself against before restructuring anything.
	return strings.Join(blocks, "\n\n"), sc.graph.Edges()
}

// judgedChoresBlock lists the project's judged chores the survey could
// act on this turn — the operator's `when` criterion, and when the chore
// was last completed. This is the whole judged-chore seam on the read
// side: the criterion is prose the agent evaluates against the delta,
// and the gate's `chore` spec is where the verdict goes.
//
// Chores that are cooling down or already have an open run are omitted:
// the survey can't act on them, so naming them is noise. No openable
// judged chores → no block, same as the sibling blocks. A chore-load
// failure drops the block rather than failing the sweep.
func judgedChoresBlock(sc *pulseScan, projectID string) string {
	defs, err := chore.LoadAll(sc.root)
	if err != nil {
		return ""
	}
	var lines []string
	for _, st := range chore.EvaluateAll(defs, sc.mds, sc.idx, time.Now()) {
		d := st.Definition
		if d.Project != projectID || !d.Judged() || st.OpenRun != "" || st.CooldownBlocking {
			continue
		}
		last := "never"
		if !st.LastCompleted.IsZero() {
			last = st.LastCompleted.UTC().Format("2006-01-02 15:04Z")
		}
		lines = append(lines, fmt.Sprintf("- `%s` — due when: %s (last completed: %s)", d.Name, d.When, last))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Judged chores: registrations the operator authored whose due-ness is a judgment, not a glob or a clock. " +
		"For each, ask only whether what landed since the last pulse meets the condition as written — the work itself is " +
		"already authored, so this is not the spawn bar. When it does, nominate it in the gate with " +
		"`{\"chore\": \"<name>\", \"why\": \"<the landed change that met it>\"}`, in `loose` or at a thread position. " +
		"Quiet is normal.\n" + strings.Join(lines, "\n")
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

// openPulse is the Go-level seam behind the survey's execution — the
// sweep's own turn, and the cascade dispatcher. Read-only
// both-legs-strict sandbox (the design/chat shape): the survey reads the
// project but never edits it, and the boundary guard enforces that. It
// is a var so runPulseSurvey's auto-close can be tested without running
// the agent turn.
//
// pi is the sweep's Ctrl-C latch (nil on the dispatcher seam, which has
// no skip window). The prompt builder is the pre-executor belt: a
// Ctrl-C that latched during setup returns errPulseSkipped here so the
// agent never starts.
//
// It reports a surveyOutcome rather than a bare exit code: the two extra
// facts are the ones the apply step can't recover afterwards.
var openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
	out := surveyOutcome{}
	out.code = runStageSession(projectID, runID, pulseDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			OnAgentStart:           func() { out.agentStarted = true },
			// Deferred so the context blocks render against the session
			// worktree, the copy runStageSession hands the builder.
			InitialPromptBuilder: func(workRoot string) (string, error) {
				if pi.interrupted() {
					return "", errPulseSkipped
				}
				prompt, edges := pulseKickoffWithContext(workRoot, projectID, runID, stderr)
				out.chainEdges = edges
				return prompt, nil
			},
			CanvasSkeleton: pulseCanvasSkeleton,
		}, stdout, stderr)
	return out
}

// surveyOutcome is what one survey turn reports back to the sweep.
type surveyOutcome struct {
	code int
	// agentStarted says whether the agent turn actually began. That is
	// the disposal decision: nothing started means nothing was surveyed
	// and the just-minted run is disposed, while a started turn leaves a
	// canvas worth a human's eyes no matter how it ended.
	agentStarted bool
	// chainEdges is the live chain edge set as the agent saw it at
	// kickoff — the picture its ordering opinion was formed against. nil
	// when the kickoff couldn't read it, which reads as "no snapshot" and
	// skips the drift check rather than refusing to groom.
	chainEdges map[string]string
}

func runPulseNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dynamic := fs.Bool("dynamic", false, "start the parked threads on the board when the sweep finishes")
	// The clock's signature. Serve's heartbeat spawns a command otherwise
	// spelled exactly like the operator's, and the per-project mode caps
	// the clock rather than the operator — so the child has to say which
	// one it is. Nothing else passes it.
	heartbeat := fs.Bool("heartbeat", false, "the clock invoked this sweep; the project's mode binds it")
	// The spawner's channel back: `moe serve` passes a path here and reads
	// the run the sweep opened out of it, so /serve can link a sweep to
	// the pulse run it minted. Nothing else passes it.
	emitRun := fs.String("emit-run", "", "write the run this sweep opens to `path` (one line, bare slug)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe pulse new [--dynamic] [--heartbeat] [--emit-run <path>] <project>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the whole pulse for a project: opens every due chore's run")
		moePrintln(stderr, "(never executes one), then a headless read-only survey that files")
		moePrintln(stderr, "followups, writes a report, and may spawn and groom parked fix runs.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Without --dynamic the sweep is pure curation: everything it grooms")
		moePrintln(stderr, "parks. With it, the sweep starts every kickable parked thread on")
		moePrintln(stderr, "the board when it finishes — the consent `moe serve --dynamic`")
		moePrintln(stderr, "stands behind, said at the door a clock can knock on.")
		moePrintln(stderr, "")
		moePrintln(stderr, "--heartbeat is serve's own marker on the sweeps its clock spawns:")
		moePrintln(stderr, "those obey the project's mode (`moe project mode`). A sweep you")
		moePrintln(stderr, "type is your consent and ignores the mode — don't pass it by hand.")
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
	// --dynamic is the whole flag: it hands this invocation to the machine
	// at the dynamic rung. Everything downstream — the self-kick gate, the
	// consent trailers, the survey's context line — reads process ride
	// mode, so nothing else has to change. Without the flag no
	// withRideMode call happens at all, which is what keeps the unflagged
	// verb pure curation: rideWalkActive stays false, so a bare sweep
	// marks itself as operator-typed.
	if *dynamic {
		defer withRideMode(rideDynamic)()
	}
	// Separate from the rung above because it answers a different
	// question: --dynamic is what the invocation licensed, --heartbeat is
	// who made it. Only the second is what a per-project mode caps.
	if *heartbeat {
		defer withClockInvoked()()
	}
	// The pulse is the verb here, so a skip is the verb's own outcome:
	// exit 130.
	code, interrupted := runPulse(root, projectID, *emitRun, stdout, stderr)
	if interrupted {
		return exitInterrupted
	}
	return code
}

// pulseScan is the one disk read a sweep's kickoff makes: the run scan
// and the journal index, plus the by-key map and chain graph derived
// from them.
//
// Every context block wants some of this, and each used to take its own
// copy — a scan and an index build per block per sweep, each describing
// a slightly different moment. One read is cheaper and, more to the
// point, coherent: the blocks the agent reads all describe the same
// instant.
type pulseScan struct {
	root  string
	mds   []*run.Metadata
	idx   *run.JournalIndex
	byKey map[string]*run.Metadata
	graph *run.ChainGraph

	// chores memoizes the whole register's chore states, evaluated
	// against this scan's runs and index. Loaded on the first ask rather
	// than in newPulseScan: loading forks git once per definition, and
	// most scans never ask.
	chores       []chore.State
	choresLoaded bool
}

// choreStates evaluates every registered chore against this scan's runs
// and journal index — once per scan, shared by every caller. The
// heartbeat gate asks once per project per tick, so the memo is what
// keeps the chore legs inside the gate's cheap-read budget.
//
// Not safe for concurrent use, like the rest of pulseScan: a scan
// belongs to one sweep or one tick, and both walk their projects in
// sequence.
//
// A chore-load failure yields no states, which reads as "nothing is
// due". Same warn-by-silence posture the sweep's own chore blocks take
// — a broken definition must not decide a tick.
func (sc *pulseScan) choreStates(now time.Time) []chore.State {
	if sc.choresLoaded {
		return sc.chores
	}
	sc.choresLoaded = true
	defs, err := chore.LoadAll(sc.root)
	if err != nil {
		return nil
	}
	sc.chores = chore.EvaluateAll(defs, sc.mds, sc.idx, now)
	return sc.chores
}

// newPulseScan reads the runs and the journal. ok is false when either
// read fails; the caller drops its blocks rather than failing the sweep,
// which is what each block did on its own.
func newPulseScan(root string) (*pulseScan, bool) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, false
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, false
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	return &pulseScan{root: root, mds: mds, idx: idx, byKey: byKey, graph: run.NewChainGraph(idx, byKey)}, true
}
