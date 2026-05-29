package cli

import (
	"flag"
	"io"
	"path/filepath"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

const choresWorkflow = "chores"
const choresCodeDoc = "code"

func init() {
	g := NewCommandGroup(choresWorkflow, "chores workflow: new, code, close")
	g.Register(newRunCommand(choresWorkflow))
	g.Register(&Command{
		Name:    choresCodeDoc,
		Summary: "open an agent session on the chore-definition canvas; edits land in projects/<p>/chores/*",
		Run:     runChoresCode,
	})
	g.Register(closeCommand(choresWorkflow, "Close chores run %s/%s", nil))
	g.Register(&Command{Name: "cat", Summary: "dump the run's code canvas to stdout", Run: runCat(choresWorkflow, choresCodeDoc)})
	g.Register(&Command{Name: "log", Summary: "render the run's code-stage agent transcript", Run: runLog(choresWorkflow, choresCodeDoc)})
	RegisterGroup(g)

	w := NewWorkflow(choresWorkflow)
	w.RegisterStage(choresCodeDoc)
	RegisterWorkflow(w)
}

func runChoresCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chores code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chores code [--agent <name>] <project>/<run>")
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
		moePrintf(stderr, "chores code: %v\n", err)
		return 2
	}
	return openChoresCode(projectID, runID, false, *agentOverride, stdout, stderr)
}

func openChoresCode(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	if code := requireRun("chores code", projectID, runID, stderr); code != 0 {
		return code
	}
	return runStageSession(projectID, runID, choresCodeDoc, stageSessionOpts{
		NeedsSandbox:    false,
		InitialPrompt:   "Read the canvas and the project's chores/ directory. Edit definitions under projects/<project>/chores/<name>/ and use `moe chore check` as the dry-run loop.",
		Headless:        headless,
		Agent:           agentOverride,
		ExtraStagePaths: choresStageDir,
	}, stdout, stderr)
}

func choresStageDir(workRoot string, md *run.Metadata) ([]string, error) {
	return []string{filepath.Join(project.Dir(md.Project), "chores")}, nil
}
