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
// nested under `moe workflow sdlc` so kb (and future workflows) can pick
// their own short stage names without collision. `moe workflow sdlc new`
// is the entry point that creates a run in this workflow.

func init() {
	sdlc := NewWorkflow("sdlc", "SDLC workflow: new, design, code, push")
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
	sdlc.RegisterFacade(closeCommand("sdlc", "Close sdlc run %s/%s", sdlcCloseCleanup))
	RegisterWorkflow(sdlc)
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc design", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive this stage headlessly via `claude -p`; the run title is the user prompt")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe workflow sdlc design [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the design canvas.")
		moePrintln(stderr, "First use on a run creates the document; re-runs resume the session.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
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
		moePrintln(stderr, "usage: moe workflow sdlc code [--one-shot] <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the code canvas. The agent")
		moePrintln(stderr, "works inside a private sandbox clone of the project's submodule, isolated")
		moePrintln(stderr, "from other activity until `moe workflow sdlc push` opens a PR.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
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
		return fmt.Errorf("design canvas missing — run `moe workflow sdlc design %s %s` first", projectID, runID)
	}
	return nil
}
