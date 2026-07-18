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

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/wiki"
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
	"file followup entries for work worth doing, and write the canvas report ending in a `## Pull next` section that ranks " +
	"the next things to pull from the existing open backlog. Follow the stage guidance. A quiet pulse — \"nothing new since " +
	"the last pulse\" — is a valid, successful report; never manufacture findings.\n\n" +
	"Close the canvas with the `## Gate` section (a ```json fence). Set \"status\" to a short word (e.g. \"ok\") once the " +
	"survey actually ran and concluded — that is what tells the harness this was a real sweep, not a crashed no-op. " +
	"Flag a twin reflect as due — `\"reflect\": {\"due\": true, \"why\": \"<one line>\"}` — when either the cycle landed a " +
	"significant twin-relevant change (a decision, a new component, a boundary move the twin docs don't yet describe), or " +
	"twin staleness has accumulated (many small changes and/or pending twin observations teed up since the last reflect). " +
	"Do NOT flag reflect due when a twin run is already open, and never manufacture a reflect to justify the turn — the " +
	"default is `\"reflect\": {\"due\": false}`. The `why` is required when due: one line, the operator reads it next to the verdict."

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

## Gate

(agent fills: a fenced json block — set "status" once the survey concluded, and "reflect": {"due": …, "why": …}. This placeholder has no fence, so a no-op turn leaves the gate detectably unfilled.)
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
	runPulse(root, projectID, spawner, stdout, stderr)
}

// runPulse is the whole pulse: the deterministic chore auto-open (which
// opens runs but executes none), then the survey. spawner is the
// triggering run's slug ("" for a manual `moe pulse new`, which threads
// no parent edge).
func runPulse(root, projectID, spawner string, stdout, stderr io.Writer) int {
	autoOpenDueChores(root, projectID, stdout, stderr)
	return runPulseSurvey(root, projectID, spawner, stdout, stderr)
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
var runPulseSurvey func(root, projectID, spawner string, stdout, stderr io.Writer) int

func init() {
	runPulseSurvey = pulseSurvey
}

func pulseSurvey(root, projectID, spawner string, stdout, stderr io.Writer) int {
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
	// open on the dash's ACTIVE list for the operator to inspect and
	// close by hand. It does not block the next survey.
	if code := openPulse(projectID, md.ID, true /*headless*/, "", stdout, stderr); code != 0 {
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
	if gate.Reflect.Due {
		maybeSpawnReflect(root, projectID, md.ID, gate.Reflect.Why, stdout, stderr)
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
func maybeSpawnReflect(root, projectID, pulseSlug, why string, stdout, stderr io.Writer) {
	canonical, err := twinWikiBuilder(root, projectID)
	if err != nil {
		moePrintf(stderr, "pulse: reflect spawn: build twin wiki for %s: %v\n", projectID, err)
		return
	}
	md, err := mintReflectRun(root, projectID, pulseSlug, "" /*agent*/, canonical, stdout, stderr)
	if err != nil {
		var refusal *reflectRefusal
		if errors.As(err, &refusal) {
			if refusal.kind == reflectRefusalUnrecorded {
				moePrintf(stderr, "pulse: reflect not spawned for %s — %v; the operator lands those first\n", projectID, refusal)
			}
			return
		}
		moePrintf(stderr, "pulse: reflect spawn for %s: %v\n", projectID, err)
		return
	}
	moePrintf(stderr, "pulse: drift flagged — opened twin reflect %s/%s (%s)\n", projectID, md.ID, why)
}

// pulseKickoffWithContext appends the twin-reflect context line to the
// static kickoff. Wired as InitialPromptBuilder, so root is the session
// worktree runStageSession hands the builder. Best-effort: a project
// with no twin, or a feedback read that fails, drops the line rather
// than failing the sweep.
func pulseKickoffWithContext(root, projectID string) string {
	line := pendingTwinObservationsLine(root, projectID)
	if line == "" {
		return pulseKickoff
	}
	return pulseKickoff + "\n\n" + line
}

// pendingTwinObservationsLine reports how many twin observations are
// teed up for the next reflect and which runs they came from — the one
// computed input behind the "staleness accumulated" criterion, which the
// agent can't cheaply derive itself (loadTwinFeedback filters against the
// reflect checkpoint's LastIngestAt). Returns "" when the project has no
// twin or the read fails.
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
	return fmt.Sprintf("Twin-reflect context: %d twin observation(s) pending since the last reflect, from %s.",
		len(feedback), strings.Join(runs, ", "))
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
			// Deferred so the twin-reflect context line renders against
			// the session worktree, the read-only copy runStageSession
			// hands the builder — the same deferral the twin stages use to
			// keep a pass off the operator's live checkout.
			InitialPromptBuilder: func(workRoot string, _ *wiki.Config, _ bool) (string, error) {
				return pulseKickoffWithContext(workRoot, projectID), nil
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
		moePrintln(stderr, "followups and writes a report ranking what to pull next.")
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
	return runPulse(root, projectID, "" /*spawner*/, stdout, stderr)
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
