package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
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
	g := NewCommandGroup(pulseWorkflow, "pulse workflow — project sweeps and optional work dispatch")
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
	autoOpenDueChores(root, projectID, pi, stderr)
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
// (offline, no gh, a wedged lock) must not derail the sweep. No lock is
// taken here — the walk takes one short window per transition and holds
// none of it across the `gh` calls, so a sweep over a project with
// nothing pushed touches neither the lock nor the network. Whatever the
// walk commits reaches origin on serve's pusher.
func reconcileAtPulse(root, projectID string, pi *pulseInterrupt, stdout, stderr io.Writer) {
	// Checkpoint: a Ctrl-C during chore auto-open skips the network walk
	// too — the operator asked for the sweep to get out of the way.
	if pi.interrupted() {
		return
	}
	if _, err := reconcilePushedRuns(root, projectID, stdout, stderr); err != nil {
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
func autoOpenDueChores(root, projectID string, pi *pulseInterrupt, stderr io.Writer) {
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
		if _, err := openChoreInProcess(root, projectID, s.Definition.Name, choreOpenNormal); err != nil {
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
// pulseSurvey → closePulseRunStamped → closeRunInProcess and back
// through the registered close, which the var-init dependency analyser
// reads as a cycle.
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
	})
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
		disposePulseRun(root, projectID, md.ID, stderr)
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
		disposePulseRun(root, projectID, md.ID, stderr)
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
	groomed := groomChains(root, projectID, md.ID, groups, survey.chainEdges, stderr)
	groomed.prChecks = survey.prChecks

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
	var kickSection func(body []byte) []byte
	if currentRideMode == rideDynamic {
		section := renderKickSection(planKick(root, groomed))
		kickSection = func(body []byte) []byte {
			return []byte(strings.TrimRight(string(body), "\n") + "\n\n" + section)
		}
	}
	if err := closePulseRunStamped(root, projectID, md.ID, "kick section", kickSection, stderr); err != nil {
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

// closePulseRunStamped closes a pulse run through the registered close —
// the one subject the happy-path auto-close and the interrupt disposal
// both ride, so there's no parallel close path. skipEdit harvests
// followups.md as-is.
//
// rewrite, when non-nil, stamps the canvas inside the close's locked
// closure — after the dirty-tree gate, before the status flip — and
// stages it, so the stamp and the status flip land in the *same* commit.
// name labels the stamp in its error. It rides as the close's cleanup
// argument rather than the registration's own hook because pulse
// registered nil (no workspace, no branch to tear down — see the
// closeCommand registration above), so a nil rewrite is exactly the
// registered close.
//
// The fold is the point. A stamp in its own lock and its own commit could
// fail while the close went on to succeed, and the run would close with
// a decoy canvas — the raw skeleton, or a report silent about its own
// queue — and only an ephemeral warn to say why. Folded, a failed stamp
// fails the close and the run stays open on the dash's ACTIVE list:
// escalation by visibility, the pulse's standard failure mode.
//
// A failed stamped close is put back the way it was: the canvas bytes
// the stamp overwrote, if it got that far, and one reset over the whole
// run dir, which covers the canvas, run.json, and anything the harvest
// staged before the commit failed. The status flip needs no repair here:
// a failed close commit walks its own flip back (commitTerminal), so the
// run comes back already open. Repairing is not optional. The dirty-tree
// gate is repo-wide, so a half-applied stamp left on disk wedges every
// later close in the bureaucracy, not just this run's — and a run.json
// that says closed over a skeleton canvas, with no commit to say why, is
// the exact decoy the fold exists to make impossible. Best-effort: the
// close already failed, and there is no better error to report than the
// one returned. Deliberately unlocked: the close released the repolock
// on its way out, this only rewrites files this process itself
// half-wrote, and the index-lock retry in internal/git covers the
// git-level race.
func closePulseRunStamped(root, projectID, runID, name string, rewrite func(body []byte) []byte, stderr io.Writer) error {
	reg, ok := lookupCloseRegistration(pulseWorkflow)
	if !ok {
		return fmt.Errorf("no close registration for %q", pulseWorkflow)
	}
	canvasRel := run.ContentPath(projectID, runID, pulseDoc)
	canvas := filepath.Join(root, canvasRel)
	var before []byte
	var stamp closeCleanup
	if rewrite != nil {
		stamp = func(root string, _ *run.Metadata) error {
			body, err := os.ReadFile(canvas)
			if err != nil {
				return fmt.Errorf("read pulse canvas: %w", err)
			}
			before = body
			if err := os.WriteFile(canvas, rewrite(body), 0o644); err != nil {
				return fmt.Errorf("stamp %s: %w", name, err)
			}
			// Staging is what carries the stamp into the close's commit:
			// StageAndCommit's `git commit -m` takes the whole index.
			return run.Stage(root, canvasRel)
		}
	}
	err := closeRunInProcess(root, pulseWorkflow, reg.subject, stamp,
		projectID, runID, true /*skipEdit*/, stderr)
	if err != nil && rewrite != nil {
		if before != nil {
			_ = os.WriteFile(canvas, before, 0o644)
		}
		_ = git.Run(root, "reset", "-q", "--", run.Dir(projectID, runID))
	}
	return err
}

// disposePulseRun closes a just-minted pulse run the operator Ctrl-C'd
// before the survey could produce anything worth reviewing, stamping
// pulseSkipNote over the skeleton in the close's own commit. The
// skeleton canvas is non-empty so the close-time canvas gate passes, and
// the canonical root is committed-clean at that point so the dirty-tree
// gate passes. Skeleton-closed is unreachable from any stamp-capable
// binary, which makes it an unambiguous signature of a stale one — the
// diagnosis this run had to reconstruct from shell history.
//
// The trade is deliberate and reverses the stamp's original posture: a
// failed stamp now leaves the run open on the dash's ACTIVE list instead
// of closing it with a decoy canvas. Disposal only fires on the
// operator's own Ctrl-C, and a decoy report has cost three diagnosis
// cycles.
func disposePulseRun(root, projectID, runID string, stderr io.Writer) {
	skipNote := func([]byte) []byte { return []byte(pulseSkipNote) }
	if err := closePulseRunStamped(root, projectID, runID, "skip note", skipNote, stderr); err != nil {
		moePrintf(stderr, "pulse: skip-close %s/%s: %v — leaving run open\n", projectID, runID, err)
		return
	}
	moePrintf(stderr, "pulse: skipped — closed %s/%s\n", projectID, runID)
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
				sc, ok := newPulseScan(workRoot)
				if !ok {
					return "", errors.New("pulse: could not read survey state")
				}
				prompt, edges := pulseKickoffFromScan(sc, projectID, runID, stderr)
				out.prChecks = sc.prChecks(projectID, stderr)
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
	prChecks   []ownedPRChecks
}

func runPulseNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pulse new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dynamic := fs.Bool("dynamic", false, "start eligible parked threads and nominated CI repairs when the sweep finishes")
	// The clock's signature. Serve's heartbeat spawns a command otherwise
	// spelled exactly like the operator's, and the per-project mode caps
	// the clock rather than the operator — so the child has to say which
	// one it is. Nothing else passes it.
	heartbeat := fs.Bool("heartbeat", false, "the clock invoked this sweep; the project's mode binds it")
	// The spawner's channel back: `moe serve` passes a path here and reads
	// the run the sweep opened out of it, so /serve can link a sweep to
	// the pulse run it minted. Nothing else passes it.
	emitRun := fs.String("emit-run", "", "write the run this sweep opens to `path` (one line, bare slug)")
	prChecks := fs.String("pr-checks", "", "heartbeat PR observation (internal)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe pulse new [--dynamic] <project>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Runs the whole pulse for a project: opens every due chore's run")
		moePrintln(stderr, "without executing stages, then runs a headless read-only survey that")
		moePrintln(stderr, "files followups, writes a report, and may spawn and groom parked runs.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Without --dynamic the sweep is pure curation: everything it grooms")
		moePrintln(stderr, "parks. With it, the sweep starts eligible parked threads and nominated")
		moePrintln(stderr, "CI repairs on existing pushed PRs when it finishes — the consent")
		moePrintln(stderr, "`moe serve --dynamic` stands behind.")
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
	if *prChecks != "" {
		var offered []ownedPRChecks
		if !*heartbeat || json.Unmarshal([]byte(*prChecks), &offered) != nil {
			moePrintln(stderr, "pulse: invalid heartbeat PR observation")
			return 2
		}
		prior := heartbeatPRChecks
		heartbeatPRChecks = offered
		defer func() { heartbeatPRChecks = prior }()
	}
	// The pulse is the verb here, so a skip is the verb's own outcome:
	// exit 130.
	code, interrupted := runPulse(root, projectID, *emitRun, stdout, stderr)
	if interrupted {
		return exitInterrupted
	}
	return code
}
