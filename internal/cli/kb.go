package cli

import (
	"flag"
	"io"
)

// The kb workflow owns the research→summarize lifecycle for knowledge
// base runs. There is no push — the artifact is the markdown in the
// bureaucracy itself, and signing `summarize` is publication. See
// designs for the shape and rationale.
//
// Neither stage needs a sandbox clone: research edits a source list in
// place; summarize edits its synthesized article. Both live under
// projects/<project>/runs/<id>/documents/ alongside any sdlc runs the
// project hosts.

func init() {
	kb := NewWorkflow("kb", "Knowledge base workflow: new, research, summarize")
	kb.RegisterFacade(newRunCommand("kb"))
	kb.Register(&Command{
		Name:    "research",
		Summary: "open a Claude Code session on the run's research bibliography",
		Run:     runResearch,
	})
	kb.Register(&Command{
		Name:    "summarize",
		Summary: "open a Claude Code session on the run's synthesized article",
		Run:     runSummarize,
	}, "research")
	kb.RegisterFacade(closeCommand("kb", "Close kb run %s/%s", nil))
	Register(kb.Command())
	RegisterWorkflow(kb)
}

func runResearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb research", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe kb research <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the research bibliography.")
		moePrintln(stderr, "The agent extends the source list with web searches rather than replacing it.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	const kickoff = "The operator just opened this research session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"source list stands (fresh start vs. resumed) and ask what topic or " +
		"angle they'd like you to search for. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "research", false, kickoff, stdout, stderr)
}

func runSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe kb summarize <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the synthesized article.")
		moePrintln(stderr, "The agent writes prose from the research doc; signing this stage is")
		moePrintln(stderr, "publication — there is no push stage.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	const kickoff = "The operator just opened this summarize session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"article stands (fresh start vs. resumed) and ask what they'd like to " +
		"work on next. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "summarize", false, kickoff, stdout, stderr)
}
