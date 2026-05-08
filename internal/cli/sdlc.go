package cli

import (
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
	sdlc := NewWorkflow("sdlc", "sdlc workflow: new, design, code, push")
	sdlc.RegisterFacade(newRunCommand("sdlc"))
	sdlc.Register(&Command{
		Name:    "design",
		Summary: "open a Claude Code session on the run's design document",
		Run:     runDesign,
	})
	sdlc.Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the run's code document (in a sandbox clone)",
		Run:     runCode,
	}, "design")
	sdlc.Register(pushCmd, "code")
	sdlc.RegisterFacade(&Command{
		Name:    "shell",
		Summary: "drop into a shell rooted at a run's workspace, or at a named workspace directly",
		Run:     runShell,
	})
	sdlc.RegisterFacade(closeCommand("sdlc", "Close sdlc run %s/%s", releaseWorkspaceCleanup))
	sdlc.RegisterFacade(&Command{
		Name:    "resume",
		Summary: "drive any pending stages of an opened run headlessly, then prompt at the merge gate",
		Run:     runResume,
	})
	RegisterWorkflow(sdlc)
	Register(sdlc.Command())
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

// runResume drives an already-opened sdlc run forward through whichever
// of design/code is still pending and hands off to the merge-gate
// prompt. Useful as a first-class operator verb (pick up an opened run
// and ride it to the merge gate without typing two stage commands) and
// as the per-item entry point for `moe queue run`.
//
// Two modes:
//   - default (interactive): invoke the next pending stage interactively;
//     the stage's existing [Y/n/o] / [N/m/p] chain prompt walks through
//     the rest. Operator is in the loop at every Claude session.
//   - --one-shot: drive each pending stage headlessly via `claude -p`,
//     then hand off to the merge gate. Operator is in the loop only at
//     the merge gate.
//
// Both modes refuse missing or terminal runs at the boundary so a
// resume call against a dead run fails fast instead of spawning a session.
func runResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive pending stages headlessly via `claude -p` (default: interactive)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc resume [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Picks up the run at its first pending stage and drives it forward.")
		moePrintln(stderr, "Without --one-shot, opens the stage interactively (operator in the loop).")
		moePrintln(stderr, "With --one-shot, drives the pending stage headlessly and prompts")
		moePrintln(stderr, "[Y/n/o] before chaining to the next — operator can spot-check the")
		moePrintln(stderr, "design before letting code run. The final stage hands off to the")
		moePrintln(stderr, "[N/m/p] merge-gate prompt. Refuses runs that are missing or already")
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
		moePrintf(stderr, "sdlc resume: %s/%s is a %s run, not sdlc\n", projectID, runID, md.Workflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "sdlc resume: %s/%s is %s; nothing to resume\n", projectID, runID, md.Status)
		return 1
	case run.StatusPushed:
		moePrintf(stderr, "sdlc resume: %s/%s already pushed; resume cannot drive a pushed run\n", projectID, runID)
		return 1
	}

	// Decide where to start. Workflow.Next returns the parked stage
	// for any in_progress sdlc run — design, code, or push — under
	// the forward-walking satisfaction rule. NextKindDone is reserved
	// for terminal statuses and runs whose workflow has no stages,
	// neither of which can reach this point (resume refuses terminal
	// above; sdlc has three stages).
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if *oneShot {
		// Headless chain. push (or NextKindDone) skips both stages and
		// hands straight to the merge-gate prompt.
		startStage := ""
		if kind == NextKindStage && next != nil {
			startStage = next.Name
		}
		return runOneShotChain(root, md, startStage, stdout, stderr)
	}

	// Interactive mode: invoke the next stage interactively. Its
	// post-stage [Y/n/o] / [N/m/p] prompt drives the rest of the chain
	// — same behaviour the operator gets today after a stage exits.
	if kind != NextKindStage || next == nil {
		// Defensive: under the forward-walking satisfaction rule,
		// Next() returns the parked stage rather than NextKindDone for
		// any non-terminal in_progress run, and resume already refuses
		// terminal statuses above. In practice this branch isn't
		// reachable from a healthy run today; kept as a no-op safety
		// net so an unforeseen edge case (a bare-metadata run with no
		// turns at all and a workflow with no stages) doesn't panic.
		return promptNextStage(root, md, "", stdout, stderr)
	}
	return next.Run([]string{md.Project, md.ID}, stdout, stderr)
}

// requireDesignCanvas refuses the code stage when the run's design
// canvas is missing or empty. The fail-loud invariant the design twin
// records on the commit side carries into the read side: code can't
// drive against a design that was never opened. Applies to both
// interactive and `--one-shot` paths so an operator skipping straight
// to `sdlc code` on a fresh run gets the same error either way.
func requireDesignCanvas(projectID, runID string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return err
	}
	canvas := filepath.Join(root, run.ContentPath(projectID, runID, "design"))
	info, err := os.Stat(canvas)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("design canvas missing — run `moe sdlc design %s %s` first", projectID, runID)
	}
	return nil
}
