package cli

import (
	"io"
)

const choresWorkflow = "chores"
const choresCodeDoc = "code"

func init() {
	g := NewCommandGroup(choresWorkflow, "chores workflow")
	g.Register(newRunCommand(choresWorkflow))
	g.Register(&Command{
		Name:    choresCodeDoc,
		Summary: "open an agent session on the chore-definition canvas; edits land in projects/<p>/chores/*",
		Run:     runChoresCode,
	})
	g.Register(closeCommand(choresWorkflow, "Close chores run %s/%s", nil))
	g.Register(harvestCommand(choresWorkflow))
	g.Register(&Command{Name: "cat", Summary: "dump the run's code canvas to stdout", Run: runCat(choresWorkflow, choresCodeDoc)})
	g.Register(&Command{Name: "log", Summary: "render the run's code-stage agent transcript", Run: runLog(choresWorkflow, choresCodeDoc)})
	RegisterGroup(g)

	w := NewWorkflow(choresWorkflow)
	w.RegisterStage(choresCodeDoc)
	RegisterWorkflow(w)

	// Serve declaration: render the cascade trio plus the close chip on
	// chores run pages. Cascade is derived from operatorCascades; newRun
	// stays false.
	registerServeWorkflow(choresWorkflow, serveWorkflowDecl{})
}

func runChoresCode(args []string, stdout, stderr io.Writer) int {
	return runStageVerb(stageVerbCfg{
		workflow: choresWorkflow,
		verb:     "chores code",
		stage:    choresCodeDoc,
		usage: []string{
			"Opens an interactive agent session on the chore-definition canvas.",
			"Edits land in projects/<project>/chores/*.",
		},
		open:        openChoresCode,
		resolveSlug: plainRunSlug,
	}, args, stdout, stderr)
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
		ExtraStagePaths: stageProjectDirs,
	}, stdout, stderr)
}
