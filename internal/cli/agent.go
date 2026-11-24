package cli

import (
	"flag"
	"io"
	"time"

	"github.com/modulecollective/moe/internal/agentdefault"
)

func init() {
	Register(&Command{Name: "agent", Summary: "show or set the global default agent for 24 hours", Run: runAgent})
}

func runAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe agent [codex|claude|reset]")
		moePrintln(stderr, "Global default for 24 hours; flags, run bindings and MOE_FORCE_AGENT still win.")
		moePrintln(stderr, "Same active choice does not extend expiry. reset returns to the current stylesheet.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	var s agentdefault.State
	if fs.NArg() == 0 {
		s, err = agentdefault.Read(root)
	} else {
		desired := fs.Arg(0)
		if desired == "reset" {
			desired = ""
		}
		s, err = agentdefault.Set(root, desired, nil, time.Now)
	}
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintln(stdout, s.Summary(time.Now()))
	return 0
}
