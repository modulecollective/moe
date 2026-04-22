package cli

import (
	"flag"
	"io"
)

func init() {
	Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the request's code document (in a sandbox clone)",
		Run:     runCode,
	})
}

func runCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe code <project> <request>")
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
