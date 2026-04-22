package cli

import (
	"io"
	"sort"
)

// Workflow groups a set of stage Commands under one top-level verb,
// e.g. `moe sdlc design ...`. It implements the "nested dispatcher"
// shape chosen in the consider-workflow-subcommands design: each
// workflow owns its own stage namespace so SDLC and (future) kb can
// pick short stage names without colliding.
//
// A Workflow is itself exposed as a top-level Command via Command(),
// which lets cli.Register keep a flat top-level table.
type Workflow struct {
	Name    string
	Summary string
	stages  map[string]*Command
}

// NewWorkflow constructs an empty workflow. Callers add stages with
// Register and then hand Command() to cli.Register.
func NewWorkflow(name, summary string) *Workflow {
	return &Workflow{
		Name:    name,
		Summary: summary,
		stages:  map[string]*Command{},
	}
}

// Register adds a stage command to this workflow. Panics on duplicate
// stage names within the workflow — same contract as top-level Register.
func (w *Workflow) Register(c *Command) {
	if _, dup := w.stages[c.Name]; dup {
		panic("cli: duplicate stage " + w.Name + " " + c.Name)
	}
	w.stages[c.Name] = c
}

// Command returns the workflow as a top-level Command suitable for
// passing to the package-level Register.
func (w *Workflow) Command() *Command {
	return &Command{
		Name:    w.Name,
		Summary: w.Summary,
		Run:     w.run,
	}
}

func (w *Workflow) run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		w.printUsage(stdout)
		return 0
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		w.printUsage(stdout)
		return 0
	}
	cmd, ok := w.stages[name]
	if !ok {
		moePrintf(stderr, "unknown %s stage %q\n", w.Name, name)
		w.printUsage(stderr)
		return 1
	}
	return cmd.Run(args[1:], stdout, stderr)
}

func (w *Workflow) printUsage(out io.Writer) {
	moePrintf(out, "usage: moe %s <stage> [args...]\n", w.Name)
	moePrintln(out, "")
	moePrintln(out, "stages:")
	names := make([]string, 0, len(w.stages))
	for n := range w.stages {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		moePrintf(out, "  %-14s  %s\n", n, w.stages[n].Summary)
	}
}
