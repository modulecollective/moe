package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

// The SDLC workflow owns the design→code→push lifecycle. Stages are
// nested under `moe sdlc` so kb (and future workflows) can pick their
// own short stage names without collision. `moe sdlc new` is the entry
// point that creates a run in this workflow.

func init() {
	g := NewCommandGroup("sdlc", "sdlc workflow: new, design, code, test, push")
	g.Register(newRunCommand("sdlc"))
	g.Register(&Command{
		Name:    "design",
		Summary: "open a Claude Code session on the run's design document",
		Run:     runDesign,
	})
	g.Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the run's code document (in a sandbox clone)",
		Run:     runCode,
	})
	g.Register(&Command{
		Name:    "test",
		Summary: "open a Claude Code session on the run's test document — verify the code stage's work",
		Run:     runTest,
	})
	g.Register(pushCommand("sdlc"))
	g.Register(&Command{
		Name:    "shell",
		Summary: "drop into a shell rooted at a run's workspace, or at a named workspace directly",
		Run:     runShell,
	})
	g.Register(closeCommand("sdlc", "Close sdlc run %s %s", releaseWorkspaceCleanup))
	g.Register(&Command{
		Name:    "reopen",
		Summary: "open a fresh sdlc run seeded with the design canvas of a terminal prior run",
		Run:     runSDLCReopen,
	})
	g.Register(&Command{
		Name:    "resume",
		Summary: "drive any pending stages of an opened run headlessly, then prompt at the merge gate",
		Run:     runResume,
	})
	RegisterGroup(g)

	w := NewWorkflow("sdlc")
	w.RegisterStage("design")
	w.RegisterStage("code", "design")
	w.RegisterStage("test", "code")
	w.RegisterStage("push", "test")
	// Test stage's anti-theater check: the work-turn commit alone
	// doesn't tell us whether the agent actually filled the canvas
	// or just committed the placeholder skeleton. The gate reads the
	// canvas and refuses to advance until "What was verified" and
	// "What wasn't verified" both have substantive content.
	w.RegisterStageGate("test", testStageGate)
	RegisterWorkflow(w)
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc design", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive this stage headlessly via `claude -p`; the run title is the user prompt")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc design [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the design canvas.")
		moePrintln(stderr, "First use on a run creates the document; re-runs resume the session.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	if code := requireRun("sdlc design", fs.Arg(0), fs.Arg(1), stderr); code != 0 {
		return code
	}
	if *oneShot {
		return runStageSession(fs.Arg(0), fs.Arg(1), "design",
			stageSessionOpts{Headless: true}, stdout, stderr)
	}
	// The agent produces the user-facing cue itself: Claude Code has no
	// way to pre-seed the input box with editable text, so instead of a
	// printed banner (which the TUI would cover on launch) we ask the
	// agent to greet the operator and prompt for input.
	const kickoff = "The operator just opened this design session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"design stands (fresh start vs. resumed) and ask what they'd like to " +
		"work on next. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "design",
		stageSessionOpts{InitialPrompt: kickoff}, stdout, stderr)
}

func runCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive this stage headlessly via `claude -p`; the run title is the user prompt")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc code [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the code canvas. The agent")
		moePrintln(stderr, "works inside a private sandbox clone of the project's submodule, isolated")
		moePrintln(stderr, "from other activity until `moe sdlc push` opens a PR.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	// Validate the run before requireDesignCanvas, so a wrong-project
	// typo surfaces as "run not found" instead of "design canvas
	// missing" (which would send the operator off to run a design
	// stage that's also going to fail). Mirrors runResume's shape.
	if code := requireRun("sdlc code", fs.Arg(0), fs.Arg(1), stderr); code != 0 {
		return code
	}
	if err := requireDesignCanvas(fs.Arg(0), fs.Arg(1)); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if *oneShot {
		return runStageSession(fs.Arg(0), fs.Arg(1), "code",
			stageSessionOpts{NeedsSandbox: true, Headless: true}, stdout, stderr)
	}
	const kickoff = "The operator just opened this code session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"implementation stands (fresh start vs. resumed) and ask what they'd " +
		"like to work on next. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "code",
		stageSessionOpts{NeedsSandbox: true, InitialPrompt: kickoff}, stdout, stderr)
}

func runTest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive this stage headlessly via `claude -p`; the run title is the user prompt")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc test [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the test canvas. The agent")
		moePrintln(stderr, "verifies the code stage's work — running the project's checks, driving")
		moePrintln(stderr, "the change end-to-end, applying small in-place fixes, and narrating what")
		moePrintln(stderr, "was and wasn't verified on the canvas. Pre-push hooks still gate ship.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	if code := requireRun("sdlc test", fs.Arg(0), fs.Arg(1), stderr); code != 0 {
		return code
	}
	// test depends on code's output (the diff to verify) the same way code
	// depends on design's. Refuse if the code canvas is missing so a
	// skipped-ahead `sdlc test` fails fast instead of opening a session
	// against nothing.
	if err := requireCodeCanvas(fs.Arg(0), fs.Arg(1)); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if *oneShot {
		return runStageSession(fs.Arg(0), fs.Arg(1), "test",
			stageSessionOpts{
				NeedsSandbox:   true,
				Headless:       true,
				CanvasSkeleton: testCanvasSkeleton,
			}, stdout, stderr)
	}
	const kickoff = "The operator just opened this test session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where " +
		"verification stands (fresh start vs. resumed) and ask what they'd " +
		"like to verify or spot-check next. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "test",
		stageSessionOpts{
			NeedsSandbox:   true,
			InitialPrompt:  kickoff,
			CanvasSkeleton: testCanvasSkeleton,
		}, stdout, stderr)
}

// testCanvasSkeleton is the fixed structural shape every test canvas
// opens with. The Next.satisfied check (see workflow.go) enforces
// non-empty "What was verified" and "What wasn't verified" sections;
// the stage fragment instructs the agent on the anti-theater rules.
const testCanvasSkeleton = `# Test

## What was verified

(agent fills: commands run, end-to-end paths driven, what passed — cite and quote)

## What wasn't verified

(agent fills: skipped surfaces + why — needs human eye, needs prod-shaped data, out of scope. "Nothing — automated tests cover the change" is acceptable for pure-backend work.)

## Fixes applied during this stage

(agent fills: one row per in-place fix; empty if none)

## Operator spot-check

(optional; the operator may fill if they drove the change manually)
`

// runResume drives an already-opened sdlc run forward through whichever
// of design/code/test is still pending and hands off to the next chain
// prompt. Useful as a first-class operator verb (pick up an opened run
// and ride it to the next gate without typing two stage commands) and
// as the per-item entry point for `moe queue run`.
//
// Always interactive: invokes the next pending stage interactively;
// the stage's existing chain prompt (`[Y/n/o…]` / `[N/m/p…]`) walks
// the rest. Headless cascade is no longer a `resume` flag — the
// operator types `!<stage>` or `!!` at the chain prompt once they've
// seen the canvas, the same vocabulary every other one-shot decision
// uses.
//
// Refuses missing or terminal runs at the boundary so a resume call
// against a dead run fails fast instead of spawning a session.
func runResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc resume <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Picks up the run at its first pending stage and opens it")
		moePrintln(stderr, "interactively. The stage's post-turn chain prompt drives the rest:")
		moePrintln(stderr, "`o` runs the next stage headless, `!<stage>` cascades to a named gate,")
		moePrintln(stderr, "`!!` cascades and ships. Refuses runs that are missing or already")
		moePrintln(stderr, "terminal.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, runID := fs.Arg(0), fs.Arg(1)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "sdlc resume: %v\n", err)
		return 1
	}
	if md.Workflow != "sdlc" {
		moePrintf(stderr, "sdlc resume: %s %s is a %s run, not sdlc\n", projectID, runID, md.Workflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "sdlc resume: %s %s is %s; nothing to resume\n", projectID, runID, md.Status)
		return 1
	case run.StatusPushed:
		moePrintf(stderr, "sdlc resume: %s %s already pushed; resume cannot drive a pushed run\n", projectID, runID)
		return 1
	}

	// Decide where to start. Workflow.Next returns the parked stage
	// for any in_progress sdlc run — design, code, test, or push —
	// under the forward-walking satisfaction rule. NextKindDone is
	// reserved for terminal statuses and runs whose workflow has no
	// stages, neither of which can reach this point (resume refuses
	// terminal above; sdlc has four stages).
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	nextStage, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Interactive mode: invoke the next stage interactively. Its
	// post-stage chain prompt drives the rest — same behaviour the
	// operator gets today after a stage exits.
	if kind != NextKindStage || nextStage == "" {
		// Defensive: under the forward-walking satisfaction rule,
		// Next() returns the parked stage rather than NextKindDone for
		// any non-terminal in_progress run, and resume already refuses
		// terminal statuses above. In practice this branch isn't
		// reachable from a healthy run today; kept as a no-op safety
		// net so an unforeseen edge case (a bare-metadata run with no
		// turns at all and a workflow with no stages) doesn't panic.
		return promptNextStage(root, md, "", stdout, stderr)
	}
	g, err := LookupGroup(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	cmd := g.Lookup(nextStage)
	if cmd == nil {
		moePrintf(stderr, "sdlc resume: workflow %s has no command for stage %q\n", md.Workflow, nextStage)
		return 1
	}
	return cmd.Run([]string{md.Project, md.ID}, stdout, stderr)
}

// requireRun fails the stage entry point fast when the run doesn't
// exist, before any per-turn worktree is materialised. Without this
// check, a wrong-project typo produces an empty worktree per attempt
// plus a confusing downstream error (a missing design canvas, or a
// raw filesystem read error from inside the worktree). Returns the
// process exit code: 0 means proceed, non-zero means the caller
// already wrote the error and should bail.
func requireRun(verb, projectID, runID string, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if _, err := run.Load(root, projectID, runID); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "%s: run not found: %s %s\n", verb, projectID, runID)
			return 1
		}
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	return 0
}

// requireDesignCanvas refuses the code stage when the run's design
// canvas is missing or empty. The fail-loud invariant the design twin
// records on the commit side carries into the read side: code can't
// drive against a design that was never opened. Applies to both
// interactive and `--one-shot` paths so an operator skipping straight
// to `sdlc code` on a fresh run gets the same error either way.
func requireDesignCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "design", "code")
}

// requireCodeCanvas is the analogue for test stage: refuse to open a
// test session when there's no code canvas to verify. Same fail-loud
// invariant as requireDesignCanvas, one stage downstream.
func requireCodeCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "code", "test")
}

// requirePriorCanvas is the shared shape behind requireDesignCanvas and
// requireCodeCanvas: stat the prior stage's canvas and bail with a
// pointer at the verb the operator needs to run first.
func requirePriorCanvas(projectID, runID, priorStage, currentStage string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return err
	}
	canvas := filepath.Join(root, run.ContentPath(projectID, runID, priorStage))
	info, err := os.Stat(canvas)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("%s canvas missing — run `moe sdlc %s %s %s` before `moe sdlc %s`",
			priorStage, priorStage, projectID, runID, currentStage)
	}
	return nil
}
