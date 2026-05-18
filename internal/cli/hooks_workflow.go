package cli

import (
	"flag"
	"io"
	"path/filepath"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// The hooks workflow journals edits to projects/<p>/hooks/<event>.d/*
// scripts. NeedsSandbox: false — edits land in the bureaucracy itself,
// so the per-turn commit IS the landing (same shape as meta-moe; no
// push verb). Pair with `moe hook fire` (hook_cli.go) for the cheap
// iteration loop; this workflow is where edits get a design canvas,
// a per-pass record, and trailers in the journal.

const hooksWorkflow = "hooks"
const hooksCodeDoc = "code"

func init() {
	g := NewCommandGroup(hooksWorkflow, "hooks workflow: new, code, close")
	g.Register(newRunCommand(hooksWorkflow))
	g.Register(&Command{
		Name:    hooksCodeDoc,
		Summary: "open a Claude Code session on the run's code canvas; edits land in projects/<p>/hooks/<event>.d/*",
		Run:     runHooksCode,
	})
	// Bureaucracy-side workflow: no sandbox, no workspace, no
	// moe/<run> branch — pass nil cleanup and ride the standard
	// state-guard / harvest / status-flip path.
	g.Register(closeCommand(hooksWorkflow, "Close hooks run %s %s", nil))
	RegisterGroup(g)

	w := NewWorkflow(hooksWorkflow)
	w.RegisterStage(hooksCodeDoc)
	RegisterWorkflow(w)
}

func runHooksCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hooks code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe hooks code <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the run's code canvas.")
		moePrintln(stderr, "The agent edits scripts under projects/<project>/hooks/<event>.d/* and")
		moePrintln(stderr, "iterates via `moe hook fire <project> <event>`. Edits commit alongside")
		moePrintln(stderr, "the canvas on session close.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	return openHooksCode(fs.Arg(0), fs.Arg(1), false, false, stdout, stderr)
}

// openHooksCode is the Go-level seam behind `moe hooks code`. Mirrors
// the equivalent seams in sdlc / twin / kb / meta-moe: the typed
// Command.Run parses args; this helper does the requireRun guard and
// hands to runStageSession. The chain prompt's cascade driver reaches
// it through openHooksStage in hooks_stages.go.
func openHooksCode(projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
	if code := requireRun("hooks code", projectID, runID, stderr); code != 0 {
		return code
	}
	const kickoff = "The operator just opened this hooks code session. " +
		"Read the canvas before replying so your acknowledgement reflects what's " +
		"actually on it; skim the project's hooks/ directory to see what scripts " +
		"already exist. In one or two sentences, acknowledge where the work stands " +
		"(fresh start vs. resumed) and ask what they'd like to change next. Then " +
		"wait for their reply."
	return runStageSession(projectID, runID, hooksCodeDoc, stageSessionOpts{
		NeedsSandbox:    false,
		InitialPrompt:   kickoff,
		Headless:        headless,
		SkipNextStage:   suppressNextStage,
		ExtraStagePaths: hooksStageHooksDir,
	}, stdout, stderr)
}

// hooksStageHooksDir tells commitTurn to also stage the project's
// hooks/ directory. The agent's edits live there, not in the canvas
// dir, so without this they'd silently drop on commit — commitTurn
// only stages docDir + runJSON by default.
func hooksStageHooksDir(workRoot string, md *run.Metadata) ([]string, error) {
	hooksRel := filepath.Join(project.Dir(md.Project), "hooks")
	return []string{hooksRel}, nil
}
