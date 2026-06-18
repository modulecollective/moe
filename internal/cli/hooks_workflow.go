package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// The hooks workflow journals edits to projects/<p>/hooks/<event>.d/*
// scripts. NeedsSandbox: false — edits land in the bureaucracy itself,
// so the per-turn commit IS the landing (no push verb). Pair with
// `moe hook fire` (hook_cli.go) for the cheap
// iteration loop; this workflow is where edits get a design canvas,
// a per-pass record, and trailers in the journal.

const hooksWorkflow = "hooks"
const hooksCodeDoc = "code"

func init() {
	g := NewCommandGroup(hooksWorkflow, "hooks workflow")
	g.Register(newRunCommand(hooksWorkflow))
	g.Register(&Command{
		Name:    hooksCodeDoc,
		Summary: "open an agent session on the run's code canvas; edits land in projects/<p>/hooks/<event>.d/*",
		Run:     runHooksCode,
	})
	// Bureaucracy-side workflow: no sandbox, no workspace, no
	// moe/<run> branch — pass nil cleanup and ride the standard
	// state-guard / harvest / status-flip path.
	g.Register(closeCommand(hooksWorkflow, "Close hooks run %s/%s", nil))
	g.Register(harvestCommand(hooksWorkflow))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump the run's code canvas to stdout",
		Run:     runCat(hooksWorkflow, hooksCodeDoc),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render the run's code-stage agent transcript",
		Run:     runLog(hooksWorkflow, hooksCodeDoc),
	})
	RegisterGroup(g)

	w := NewWorkflow(hooksWorkflow)
	w.RegisterStage(hooksCodeDoc)
	RegisterWorkflow(w)
}

func runHooksCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hooks code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe hooks code [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive agent session on the run's code canvas.")
		moePrintln(stderr, "The agent edits scripts under projects/<project>/hooks/<event>.d/* and")
		moePrintln(stderr, "iterates via `moe hook fire <project> <event>`. Edits commit alongside")
		moePrintln(stderr, "the canvas on session close.")
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
		moePrintf(stderr, "hooks code: %v\n", err)
		return 2
	}
	return openHooksCode(projectID, runID, false, *agentOverride, stdout, stderr)
}

// openHooksCode is the Go-level seam behind `moe hooks code`. Mirrors
// the equivalent seams in sdlc / twin / kb: the typed
// Command.Run parses args; this helper does the requireRun guard and
// hands to runStageSession. The chain prompt's cascade driver reaches
// it through openHooksStage in hooks_stages.go.
func openHooksCode(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	if code := requireRun("hooks code", projectID, runID, stderr); code != 0 {
		return code
	}
	kickoff, err := buildHooksCodeKickoff(projectID, runID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return runStageSession(projectID, runID, hooksCodeDoc, stageSessionOpts{
		NeedsSandbox:    false,
		InitialPrompt:   kickoff,
		Headless:        headless,
		Agent:           agentOverride,
		ExtraStagePaths: hooksStageHooksDir,
	}, stdout, stderr)
}

// buildHooksCodeKickoff is the first-turn prompt for `moe hooks code`.
// When the run was opened with --workspace, the kickoff names the
// workspace path so the agent can answer "where should I cd to fire
// hooks?" without the operator having to remember the layout. Once
// proposal #1 of hook-dev-cleanup shipped, firing from inside the
// workspace dir Just Works.
func buildHooksCodeKickoff(projectID, runID string) (string, error) {
	const base = "The operator just opened this hooks code session. " +
		"Read the canvas before replying so your acknowledgement reflects what's " +
		"actually on it; skim the project's hooks/ directory to see what scripts " +
		"already exist. In one or two sentences, acknowledge where the work stands " +
		"(fresh start vs. resumed) and ask what they'd like to change next. Then " +
		"wait for their reply."
	root, err := findRoot(io.Discard)
	if err != nil {
		return base, nil
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		return base, nil
	}
	if md.Workspace == "" {
		return base, nil
	}
	wsPath := workspace.Path(root, md.Project, md.Workspace)
	return base + fmt.Sprintf(
		" This run is bound to the named workspace %q (at %s) — that's the cwd "+
			"the operator wants `moe hook fire` to read edits from. If they ask "+
			"where to fire from, point them there.",
		md.Workspace, wsPath), nil
}

// hooksStageHooksDir tells commitTurn to also stage the project's
// hooks/ directory. The agent's edits live there, not in the canvas
// dir, so without this they'd silently drop on commit — commitTurn
// only stages docDir + runJSON by default.
func hooksStageHooksDir(workRoot string, md *run.Metadata) ([]string, error) {
	hooksRel := filepath.Join(project.Dir(md.Project), "hooks")
	return []string{hooksRel}, nil
}
