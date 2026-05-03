package cli

import (
	"flag"
	"io"
)

// The quick workflow is a two-stage ladder (code → push) for changes
// small enough that the sdlc design stage is pure friction: rename a
// flag, bump a timeout, fix a typo. It re-uses every piece of code
// sdlc/code relies on — runStageSession, the sandbox, the push command
// — and only authors a workflow-specific prompt fragment
// (workflows/quick/code.md) and kickoff string. The design for this
// workflow lives under projects/moe/runs/fix-workflow/.

func init() {
	quick := NewWorkflow("quick", "quick-fix workflow: new, code, push")
	quick.RegisterFacade(newRunCommand("quick"))
	quick.Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the run's code canvas (in a sandbox clone)",
		Run:     runQuickCode,
	})
	quick.Register(pushCmd, "code")
	RegisterWorkflow(quick)
	Register(quick.Command())
}

func runQuickCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quick code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe quick code <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the code canvas. There is")
		moePrintln(stderr, "no design stage in this workflow — the canvas is the brief. The agent")
		moePrintln(stderr, "works inside a private sandbox clone of the project's submodule, isolated")
		moePrintln(stderr, "from other activity until `moe quick push` ships it.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	const kickoff = "The operator just opened this quick session. " +
		"Read the canvas file before replying — it's usually a short problem " +
		"statement, not implementation notes. In one or two sentences, " +
		"acknowledge what the change is asking for (or that this is a resumed " +
		"session), and ask any single clarifying question whose answer would " +
		"change what you'd do. Otherwise say you're ready and wait."
	return runStageSession(fs.Arg(0), fs.Arg(1), "code",
		stageSessionOpts{NeedsSandbox: true, InitialPrompt: kickoff}, stdout, stderr)
}
