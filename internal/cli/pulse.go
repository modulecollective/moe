package cli

import (
	"errors"
	"flag"
	"io"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// The pulse workflow is the level-3 "gather" primitive: a headless,
// read-only sweep of one project that files followup entries (→ ideas
// via the existing harvest) and writes a short report ranking what to
// pull next from the existing open backlog. It has no push — the
// artifact is the filed followups plus the canvas report.
//
// A pulse is more than the survey. It fires at the tail of the
// operator-rooted run-traffic verbs (sdlc close, sdlc push, twin close,
// and the cascades' auto-close), and every fire does two things:
//
//   - Always: open every due chore's run for the project (never execute
//     one) via openChoreInProcess. Automation acts on standing intent —
//     a chore the operator authored — but never makes a fresh decision.
//   - Every time it can: the survey — a blocking, headless stage that
//     opens a run, sweeps, files followups, writes its report, and
//     auto-closes itself on a clean exit. The open-run single-flight
//     guard is no longer a rate limiter: on the happy path each survey
//     closes its own run, so a lingering open run means a failed or
//     abandoned sweep, and the guard holds further surveys until a human
//     looks at it.
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
	"file followup entries for work worth doing, and write the canvas report ending in a `## Pull next` section that ranks " +
	"the next things to pull from the existing open backlog. Follow the stage guidance. A quiet pulse — \"nothing new since " +
	"the last pulse\" — is a valid, successful report; never manufacture findings."

// pulseCanvasSkeleton is the fixed structural shape the survey canvas
// opens with. The agent fills the sections in place. The exact Pull
// next grammar (backtick slug, em-dash, why-now) is taught in the stage
// fragment, not restated here.
const pulseCanvasSkeleton = `# Pulse

## What landed

(agent fills: 2–3 lines — what changed since the last pulse)

## Surveyed

(agent fills: what was read — the journal slice, twin areas, the backlog)

## New filings

(agent fills: one line per followup filed. "None" is valid.)

## Backlog hygiene

(agent fills: stale/duplicate flags, advisory only. Empty is fine.)

## Pull next

(agent fills: at most 3 ranked backlog picks. See the stage guidance for the exact grammar. Empty means no highlights.)
`

func init() {
	g := NewCommandGroup(pulseWorkflow, "pulse workflow — read-only project sweep that feeds the backlog")
	// `moe pulse new <project>` is the manual whole-pulse kick (and the
	// external-cron entry point): chore auto-open plus the survey, both
	// headless, the open-run guards never waived.
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
// dropped here.
var firePulse = func(root, projectID, spawner string, stdout, stderr io.Writer) {
	runPulse(root, projectID, spawner, false /*manual*/, stdout, stderr)
}

// runPulse is the whole pulse: the deterministic chore auto-open (which
// always runs and needs no rate limit — it opens runs but executes
// none), then the rate-limited survey. spawner is the triggering run's
// slug ("" for a manual `moe pulse new`, which threads no parent edge).
// manual makes the survey's single-flight refusal loud and non-zero
// (`moe pulse new`); the hook path skips quietly.
func runPulse(root, projectID, spawner string, manual bool, stdout, stderr io.Writer) int {
	autoOpenDueChores(root, projectID, stdout, stderr)
	return runPulseSurvey(root, projectID, spawner, manual, stdout, stderr)
}

// autoOpenDueChores opens every due chore's run for the project via the
// shared chore-open pipeline. No stage executes — the operator kicks
// the first stage when ready. The existing open-run refusal is the
// anti-pile-up guard, so a chore that already has an open run is
// skipped silently; any other failure warns and moves on (a chore
// pile-up must not derail the sweep or the verb that triggered it).
func autoOpenDueChores(root, projectID string, stdout, stderr io.Writer) {
	states, err := gatherChoreStates(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: read chore states for %s: %v\n", projectID, err)
		return
	}
	for _, s := range states {
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
// exercising the deterministic parts (single-flight, chore auto-open,
// auto-close) can stub the agent turn out.
//
// Single-flight is failure escalation, not pacing: a project with an
// open pulse run skips, because on the happy path a survey auto-closes
// its own run (below), so a lingering open run is a failed, SIGINT'd, or
// close-refused sweep and the next survey must wait for a human to look.
// The hook skips quietly; `moe pulse new` names the open run and
// refuses. Otherwise it opens the run and executes its stage headless,
// blocking, behind a loud banner — a SIGINT abandons the sweep and
// leaves the run open. On a clean (exit 0) survey it auto-closes the run
// so the next run-traffic event fires a fresh sweep.
//
// Body assigned in init() rather than at declaration to break the
// firePulse ↔ runPulseSurvey initialization cycle the auto-close arm
// introduces (auto-close → closeRunInProcess → firePulse) — the same
// init-order dodge openPulseStage uses.
var runPulseSurvey func(root, projectID, spawner string, manual bool, stdout, stderr io.Writer) int

func init() {
	runPulseSurvey = pulseSurvey
}

func pulseSurvey(root, projectID, spawner string, manual bool, stdout, stderr io.Writer) int {
	open, err := findInProgressPulseRun(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: scan runs for %s: %v\n", projectID, err)
		return 1
	}
	if open != "" {
		if manual {
			moePrintf(stderr, "pulse: %s already has an open pulse run %s/%s — prune or close it first\n", projectID, projectID, open)
			return 1
		}
		return 0
	}

	// spawner threads the triggering run onto the survey's MoE-Spawned-By
	// edge (empty on a manual `moe pulse new`, so it renders un-nested).
	// Both the metadata field and the trailer are set, mirroring how
	// `moe sdlc reopen` pairs Options.ReopenOf with Trailers.ReopenOf.
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

	moePrintf(stderr, "pulse: scanning %s — Ctrl-C to skip\n", projectID)
	// A non-zero exit (agent failure or SIGINT) is never propagated —
	// abandoning a sweep is not a verb failure — and it leaves the run
	// open: single-flight then blocks further surveys until the operator
	// inspects and closes the broken sweep by hand.
	if code := openPulse(projectID, md.ID, true /*headless*/, "", stdout, stderr); code != 0 {
		return 0
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
	reg, ok := lookupCloseRegistration(pulseWorkflow)
	if !ok {
		moePrintf(stderr, "pulse: no close registration for %q — leaving run %s/%s open\n", pulseWorkflow, projectID, md.ID)
		return 0
	}
	if err := closeRunInProcess(root, pulseWorkflow, reg.subject, reg.cleanup,
		projectID, md.ID, true /*skipEdit*/, false /*tailPulse*/, stdout, stderr); err != nil {
		moePrintf(stderr, "pulse: auto-close %s/%s: %v\n", projectID, md.ID, err)
	}
	return 0
}

// findInProgressPulseRun returns the id of the project's open pulse run,
// or "" if none. Same shape as findInProgressTwinRun. This is the
// survey's single-flight guard.
func findInProgressPulseRun(root, projectID string) (string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return "", err
	}
	for _, md := range mds {
		if md.Project == projectID && md.Workflow == pulseWorkflow && md.Status == run.StatusInProgress {
			return md.ID, nil
		}
	}
	return "", nil
}

// openPulse is the Go-level seam behind `moe pulse pulse` and the
// survey's headless execution. Read-only both-legs-strict sandbox (the
// design/chat shape): the survey reads the project but never edits it,
// and the boundary guard enforces that. It is a var so runPulseSurvey's
// auto-close can be tested without running the agent turn.
var openPulse = func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, pulseDoc,
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			Agent:                  agentOverride,
			InitialPrompt:          pulseKickoff,
			CanvasSkeleton:         pulseCanvasSkeleton,
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
		moePrintln(stderr, "followups and writes a report ranking what to pull next. Refuses the")
		moePrintln(stderr, "survey if a pulse run is already open — prune or close it first.")
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
	return runPulse(root, projectID, "" /*spawner*/, true /*manual*/, stdout, stderr)
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
	return openPulse(projectID, runID, false, *agentOverride, stdout, stderr)
}
