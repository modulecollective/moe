package cli

import (
	"flag"
	"io"
)

func init() {
	Register(&Command{
		Name:    "design",
		Summary: "open a Claude Code session on the request's design document",
		Run:     runDesign,
	})
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("design", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe design <project> <request>")
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
