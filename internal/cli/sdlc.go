package cli

import (
	"flag"
	"io"
)

// The SDLC workflow owns the design→code→(push) lifecycle. Stages are
// nested under `moe sdlc` so kb (and future workflows) can pick their
// own short stage names without collision. `push` stays top-level:
// it's a cross-workflow shipping verb, not a stage.

func init() {
	sdlc := NewWorkflow("sdlc", "SDLC workflow stages: design and code a request")
	sdlc.Register(&Command{
		Name:    "design",
		Summary: "open a Claude Code session on the request's design document",
		Run:     runDesign,
	})
	sdlc.Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the request's code document (in a sandbox clone)",
		Run:     runCode,
	})
	Register(sdlc.Command())
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc design", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc design <project> <request>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the design canvas.")
		moePrintln(stderr, "First use on a request creates the document; re-runs resume the session.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	return runStageSession(fs.Arg(0), fs.Arg(1), "design", false, stdout, stderr)
}

func runCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc code <project> <request>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the code canvas. The agent")
		moePrintln(stderr, "works inside a private sandbox clone of the project's submodule, isolated")
		moePrintln(stderr, "from other activity until `moe push` opens a PR.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	return runStageSession(fs.Arg(0), fs.Arg(1), "code", true, stdout, stderr)
}
