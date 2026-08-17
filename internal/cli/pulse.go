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
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
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
	"bounded, and verifiable, all three, and the stage guidance holds it. An entry may instead set `\"workflow\": \"twin\"` to " +
	"ask for a twin reflect: do that when either the cycle landed a significant twin-relevant change (a decision, a new " +
	"component, a boundary move the twin docs don't yet describe), or twin staleness has accumulated (many small changes " +
	"and/or pending twin observations teed up since the last reflect). Do NOT ask for a reflect when a twin run is already " +
	"open, and never manufacture one to justify the turn. Whatever the workflow, `why` is the one line the operator reads " +
	"next to the verdict.\n\n" +
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
	// The single stage's opener — re-run a failed sweep by hand. A
	// terminal sweep is refused here (guardPulseReentry); reading one is
	// `cat` / `log`. The hook and `moe pulse new` drive it headless.
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
// run-open takes its own. The journal push is conditional on something
// actually having moved: the common case is a project with nothing
// pushed, which should stay a disk-only scan.
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
	survey := openPulse(projectID, md.ID, true /*headless*/, "", pi, stdout, stderr)
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
	// (escalation by visibility), and skip the reflect spawn. Any parsed
	// non-empty status passes; a pulse has no ready/blocked vocabulary,
	// only close-or-linger.
	//
	// It reports out as a failure for the same reason the branch above
	// does. A vendor that hangs up mid-turn does not always exit
	// non-zero, and a sweep that concluded nothing is the shape that
	// arrives when it doesn't — counting it clean would reset the very
	// backoff meant to pace it.
	gate, ok := readPulseGate(root, projectID, md.ID)
	if !ok {
		moePrintf(stderr, "pulse: %s/%s left an unfilled gate — leaving the run open for review\n", projectID, md.ID)
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
// close's WithJournalPush closure — after the dirty-tree gate, before
// the status flip — so the staged canvas lands in the *same* commit that
// marks the run closed. That fold is the point: a stamp that lived in
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
// allowlist is sdlc + twin: the only two workflows a pulse has a reason
// to propose fresh (chat is perpetual, pulse would be recursion). A twin
// spec is a reflect *nomination* — its real slug stays harness-minted
// (reflect-YYYY-MM-DD) and nothing dedupes on Slug there, so it is
// optional and only ever read back in a warn line; Title and Design are
// meaningless and warn-ignored.
//
// Chore names a judged chore instead, and is exclusive with every other
// field: the survey's claim is only "the condition the operator wrote
// holds", and everything about the resulting run — workflow, seed,
// cooldown — comes from the chore's own definition. Why stays the one
// line the operator reads.
type pulseRunSpec struct {
	Slug     string `json:"slug"`
	Workflow string `json:"workflow"`
	Title    string `json:"title"`
	Why      string `json:"why"`
	Design   string `json:"design"`
	Chore    string `json:"chore"`
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
// parked run that already exists, or an object minting one right there.
// The two shapes are the whole reason the alias map could be deleted —
// a run that doesn't exist yet is described where it belongs rather than
// declared elsewhere and referenced by name.
type pulseThreadEntry struct {
	// Existing names a parked run in the project, resolved at apply
	// time. Set only for the string form.
	Existing string
	// Spec describes a run to mint at this position. Set only for the
	// object form.
	Spec *pulseRunSpec
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
		e.Existing, e.Spec = slug, nil
		return nil
	}
	var spec pulseRunSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return fmt.Errorf("thread entry is neither a run slug nor a run spec: %w", err)
	}
	e.Existing, e.Spec = "", &spec
	return nil
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

// maybeSpawnReflect resolves the project's twin reflect for a pulse-side
// ask — a spawn entry asking for workflow "twin", or an idea tagged
// `(twin)`. Every pulse-side ask is a *nomination*, not a create: with no
// reflect open one is minted; with one already open the mint is a noop and
// the nomination maps to the open run's id. Chain grooming then treats a
// mapped nomination like any other member, so a gate writing a twin spec at
// a thread's tail repositions the open run instead of dropping it.
//
// The one ask that resolves to nothing is unrecorded out-of-band twin
// edits with no reflect open: there is nothing to map to and minting is
// refused until the operator lands or reverts them, so it warns and skips.
//
// Warn-only throughout — a guard refusal or a mint failure never blocks
// the pulse's auto-close, since the report and filings are already durable
// on disk.
//
// Returns the resolved run's id, or "" when the ask resolved to nothing.
func maybeSpawnReflect(root, projectID, pulseSlug, why string, stdout, stderr io.Writer) string {
	canonical, err := twinWikiBuilder(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: reflect spawn: build twin wiki for %s: %v\n", projectID, err)
		return ""
	}
	// Qualify the spawner to "<project>/<slug>" before minting: the
	// journal index treats these edges as always qualified. pulseSlug is
	// the pulse run's own slug and is never empty here, so no empty-guard
	// is needed.
	md, err := mintReflectRun(root, projectID, projectID+"/"+pulseSlug, "" /*agent*/, canonical, stdout, stderr)
	if err != nil {
		if refusal, ok := errors.AsType[*reflectRefusal](err); ok {
			if refusal.kind == reflectRefusalInProgress {
				// The nomination resolves to the open pass. Logged rather
				// than silent so the sweep output stays honest about which
				// run the nomination — and the thread position holding it — landed on.
				moePrintf(stderr, "pulse: reflect already open for %s — mapped to %s/%s (%s)\n", projectID, projectID, refusal.slug, why)
				return refusal.slug
			}
			moePrintf(stderr, "pulse: reflect not spawned for %s — %v; the operator lands those first\n", projectID, refusal)
			return ""
		}
		moePrintf(stderr, "pulse: reflect spawn for %s: %v\n", projectID, err)
		return ""
	}
	moePrintf(stderr, "pulse: drift flagged — opened twin reflect %s/%s (%s)\n", projectID, md.ID, why)
	return md.ID
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
				if id, _ := m.promoteIfTaggedIdea(entry.Existing, pulseRunSpec{Why: "named at a thread position"}, stdout, stderr); id != "" {
					grp.Runs = append(grp.Runs, groomMember{mintedID: id})
				} else {
					grp.Runs = append(grp.Runs, groomMember{slug: entry.Existing})
				}
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
// Dispatch is per-workflow. sdlc (the default) is the fix-run path.
// twin routes through maybeSpawnReflect → mintReflectRun, the same core
// `moe twin reflect` uses, so the closed-schema check and the
// unrecorded-edits refusal ride along — but where the verb refuses a
// concurrent pass, the pulse's ask is a nomination that *resolves*: mint
// if no reflect is open, map onto the open one otherwise. Two twin specs
// in one gate therefore land on the same run instead of the second going
// nowhere. Anything else warns and skips.
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
// through the same seam as `--from-idea` — except a `(twin)` tag, which
// is a reflect nomination like any other and resolves through the
// reflect core, taking the promotion edge but no seed. Untagged ideas
// remain behind a structural human-triage fence. Every other live match
// still skips.
func (m *pulseMinter) mint(s pulseRunSpec, stdout, stderr io.Writer) string {
	projectID, pulseSlug := m.projectID, m.pulseSlug
	spawnedBy := projectID + "/" + pulseSlug

	// A chore nomination names a registration, not a slug to mint, so it
	// dispatches ahead of the slug validation the sdlc path applies.
	if choreName := strings.TrimSpace(s.Chore); choreName != "" {
		return m.nominateChore(choreName, s, stdout, stderr)
	}
	slug := strings.TrimSpace(s.Slug)
	// Dispatch on workflow before anything else — including the slug
	// check, the same way a chore entry dispatches ahead of it. The steps
	// below (slug validation, live-slug dedupe, tagged-idea promotion) are
	// all sdlc's: they name the run being minted. A twin spec names
	// nothing — the reflect's slug is harness-minted and its dedupe
	// semantics are "one twin run in flight per project", enforced inside
	// mintReflectRun — so requiring a slug there was a trap that dropped
	// whole threads over a field the fragment itself calls meaningless.
	switch workflow := strings.TrimSpace(s.Workflow); workflow {
	case "", "sdlc":
		if slug == "" || run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: spawn: skipping entry with unusable slug %q\n", s.Slug)
			return ""
		}
	case "twin":
		// Slug is optional here and purely a handle for these warn lines,
		// so they name the entry only when there is something to name it by.
		entry := "twin entry"
		if slug != "" {
			entry = fmt.Sprintf("twin entry %q", slug)
		}
		if t := strings.TrimSpace(s.Title); t != "" {
			moePrintf(stderr, "pulse: spawn: ignoring title on %s; the reflect's slug is harness-minted\n", entry)
		}
		if strings.TrimSpace(s.Design) != "" {
			moePrintf(stderr, "pulse: spawn: ignoring design body on %s; a reflect reads the twin, not a seed\n", entry)
		}
		return maybeSpawnReflect(m.root, projectID, pulseSlug, s.Why, stdout, stderr)
	default:
		moePrintf(stderr, "pulse: spawn: entry %q asks for workflow %q — only sdlc and twin are spawnable; skipping\n", slug, workflow)
		return ""
	}
	if !m.ensureLive(stderr) {
		return ""
	}
	if slugBaseMatches(m.live, slug) {
		return m.promoteOrSkip(slug, s, stdout, stderr)
	}
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = slug
	}
	md, err := runopen.Open(m.root, projectID, run.Options{
		IDBase:    slug,
		Workflow:  "sdlc",
		SeedDocs:  map[string]string{"design": spawnDesignSeed(title, s)},
		SpawnedBy: spawnedBy,
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
	moePrintf(stderr, "pulse: spawned fix run %s/%s (%s)\n", projectID, md.ID, title)
	return md.ID
}

// nominateChore opens a judged chore's run for a gate entry that says
// the chore's condition holds. It is a nomination, not a create, the
// same shape maybeSpawnReflect has: with the chore's run already open
// the nomination maps onto it, so a `chore` spec written at a thread
// position places the existing run instead of dropping.
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
	id, dup := m.promoteIfTaggedIdea(slug, s, stdout, stderr)
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
// unusable tag, a failed promotion) has already warned by the time this
// returns "".
func (m *pulseMinter) promoteIfTaggedIdea(slug string, s pulseRunSpec, stdout, stderr io.Writer) (id string, dup bool) {
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
	if idea.PromoteTo == "twin" {
		// A `(twin)` tag nominates a reflect; it does not name a
		// destination to mint. Route it through the same resolve the
		// gate's twin specs take — mint if no pass is open, map onto the
		// open one otherwise — and record the promotion edge onto
		// whichever run that was. No seed doc: a reflect reads the
		// managed docs, `feedback/twin.md` and the journal, never a
		// promoted idea's canvas, which stays on the idea and is
		// reachable through the MoE-Promoted-To edge.
		id := maybeSpawnReflect(m.root, projectID, pulseSlug, s.Why, stdout, stderr)
		if id == "" {
			return "", false
		}
		if markErr := runopen.MarkPromoted(m.root, projectID, idea.ID, projectID, id, walkConsent(), stdout, stderr); markErr != nil {
			moePrintf(stderr, "pulse: warning: resolved twin-tagged idea %s/%s to %s/%s but could not mark the idea: %v\n", projectID, idea.ID, projectID, id, markErr)
		}
		moePrintf(stderr, "pulse: promoted twin-tagged idea %s/%s to reflect %s/%s\n", projectID, idea.ID, projectID, id)
		return id, false
	}
	wf, lookupErr := LookupWorkflow(idea.PromoteTo)
	if lookupErr != nil || !chainableWorkflow(idea.PromoteTo) || len(wf.Stages()) == 0 {
		moePrintf(stderr, "pulse: spawn: idea %s/%s has unusable workflow tag %q — skipping\n", projectID, idea.ID, idea.PromoteTo)
		return "", false
	}
	if strings.TrimSpace(s.Design) != "" {
		moePrintf(stderr, "pulse: spawn: ignoring design body for tagged idea %s/%s; the idea canvas is the seed\n", projectID, idea.ID)
	}
	promoted, promoteErr := runopen.Promote(m.root, projectID, idea.ID, runopen.PromoteOptions{
		Workflow:   idea.PromoteTo,
		FirstStage: wf.Stages()[0],
		SpawnedBy:  spawnedBy,
		Consent:    spawnConsent(spawnedBy),
	}, stdout, stderr)
	if promoteErr != nil {
		moePrintf(stderr, "pulse: promote tagged idea %s/%s: %v\n", projectID, idea.ID, promoteErr)
		return "", false
	}
	if promoted.MarkErr != nil {
		moePrintf(stderr, "pulse: warning: promoted %s/%s but could not mark the idea: %v\n", projectID, promoted.Run.ID, promoted.MarkErr)
	}
	m.live = append(m.live, promoted.Run.ID)
	moePrintf(stderr, "pulse: promoted tagged idea %s/%s to %s run %s/%s\n", projectID, idea.ID, idea.PromoteTo, projectID, promoted.Run.ID)
	return promoted.Run.ID, false
}

// ensureLive loads the dedupe set on first use, so a gate whose specs
// are all twin nominations (or which has none at all) pays for no scan.
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

// pulseKickoffWithContext appends the harness-computed context blocks to
// the static kickoff — the twin-reflect line, the GitHub block, the
// recently-settled-runs block, and the chain-state block. Wired
// as InitialPromptBuilder, so root is the session worktree
// runStageSession hands the builder. Best-effort throughout: a gather
// that fails drops its own block rather than failing the sweep.
func pulseKickoffWithContext(root, projectID, runID string, stderr io.Writer) (string, map[string]string) {
	blocks := []string{pulseKickoff}
	if line := pendingTwinObservationsLine(root, projectID); line != "" {
		blocks = append(blocks, line)
	}
	// Four of the five blocks want the same two reads. Doing them once
	// here is not just cheaper — it means the blocks describe one
	// consistent moment rather than four successive ones.
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
	// Its own block, not a tail on the chain-state one: the line is about
	// what this sweep may start, which is true whether or not the board
	// happens to hold an active chain of two or more. Nested under the
	// chain-state block it reached the agent only by coincidence.
	if ride := rideModeContextLine(); ride != "" {
		blocks = append(blocks, ride)
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
//
// It reports a surveyOutcome rather than a bare exit code: the two extra
// facts are the ones the apply step can't recover afterwards.
var openPulse = func(projectID, runID string, headless bool, agentOverride string, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
	out := surveyOutcome{}
	out.code = runStageSession(projectID, runID, pulseDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			Agent:                  agentOverride,
			OnAgentStart:           func() { out.agentStarted = true },
			// Deferred so the twin-reflect context line renders against
			// the session worktree, the read-only copy runStageSession
			// hands the builder — the same deferral the twin stages use to
			// keep a pass off the operator's live checkout.
			InitialPromptBuilder: func(workRoot string, _ *wiki.Config, _ bool) (string, error) {
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
	// The spawner's channel back: `moe serve` passes a path here and reads
	// the run the sweep opened out of it, so /serve can link a sweep to
	// the pulse run it minted. Nothing else passes it.
	emitRun := fs.String("emit-run", "", "write the run this sweep opens to `path` (one line, bare slug)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe pulse new [--dynamic] [--emit-run <path>] <project>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the whole pulse for a project: opens every due chore's run")
		moePrintln(stderr, "(never executes one), then a headless read-only survey that files")
		moePrintln(stderr, "followups, writes a report, and may spawn and groom parked fix runs.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Without --dynamic the sweep is pure curation: everything it grooms")
		moePrintln(stderr, "parks. With it, the sweep starts every kickable parked thread on")
		moePrintln(stderr, "the board when it finishes — the consent `moe serve --dynamic`")
		moePrintln(stderr, "stands behind, said at the door a clock can knock on.")
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
	// The pulse is the verb here, so a skip is the verb's own outcome:
	// exit 130.
	code, interrupted := runPulse(root, projectID, *emitRun, stdout, stderr)
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
		moePrintln(stderr, "re-run a failed sweep by hand. A finished sweep is read with")
		moePrintln(stderr, "`moe pulse cat` / `moe pulse log`, not reopened.")
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
	if code := guardPulseReentry(projectID, runID, stderr); code != 0 {
		return code
	}
	// Interactive reopen: the operator owns this session's Ctrl-C, so no
	// skip latch (nil pi).
	survey := openPulse(projectID, runID, false, *agentOverride, nil /*pi*/, stdout, stderr)
	return survey.code
}

// guardPulseReentry fences the operator door (`moe pulse pulse
// <p>/<run>`) against a terminal run — the same class of bug the sdlc
// stage verbs grew resolveSDLCReentry for, and sharper here: a sweep's
// only durable output is its filings, filings promote through the close
// harvest, and closeRunInProcess refuses an already-terminal run. So
// everything typed into a closed sweep is silently lost. Happy-path
// sweeps auto-close, which puts *most* pulse runs in exactly this state.
//
// Refusal, not chat's soft reopen: reopen-then-close would re-arm the
// harvest on a run that already promoted its filings, and the inspect
// use case is served read-only by `cat` / `log`. What the door is still
// for is re-running a *failed* sweep, which is still in-progress and
// passes through here.
//
// A load error refuses loud rather than passing through — unlike twin
// there is no downstream require* that owns the message, so a
// pass-through would surface a raw error from deep inside
// runStageSession.
//
// A non-pulse run typed here refuses on workflow before status is
// considered: a closed sdlc run should hear that it is an sdlc run, not
// that "a sweep is not reopened". Without the check the non-terminal
// case passed straight through and wrote a survey skeleton into the
// other run's document tree.
//
// Scoped to the operator door. openPulse stays unguarded: the headless
// survey path opens a run minted moments earlier, and openPulseStage
// (the cascade dispatcher) is only reachable via chain rides.
func guardPulseReentry(projectID, runID string, stderr io.Writer) int {
	verb := "pulse " + pulseDoc
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "%s: run not found: %s/%s\n", verb, projectID, runID)
		} else {
			moePrintf(stderr, "%s: %v\n", verb, err)
		}
		return 1
	}
	if md.Workflow != pulseWorkflow {
		moePrintf(stderr, "%s: %s/%s is a %s run, not %s\n", verb, projectID, runID, md.Workflow, pulseWorkflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "%s: %s/%s is %s; a sweep is not reopened\n", verb, projectID, runID, md.Status)
		moePrintf(stderr, "hint: moe pulse log %s/%s %s   (read the sweep)\n", projectID, runID, pulseDoc)
		moePrintf(stderr, "hint: moe pulse new %s   (run a fresh one)\n", projectID)
		return 1
	}
	return 0
}

// pulseScan is the one disk read a sweep's kickoff makes: the run scan
// and the journal index, plus the by-key map and chain graph derived
// from them.
//
// Four of the five context blocks want some of this, and each used to
// take its own copy — five scans and four index builds per sweep, each
// describing a slightly different moment. One read is cheaper and, more
// to the point, coherent: the blocks the agent reads all describe the
// same instant.
type pulseScan struct {
	root  string
	mds   []*run.Metadata
	idx   *run.JournalIndex
	byKey map[string]*run.Metadata
	graph *run.ChainGraph
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
