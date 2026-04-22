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
// which lets cli.Register keep a flat top-level table. The workflow
// also tracks stage order and prereq edges so callers (like `moe dash`)
// can compute "what's next" without hard-coding the design→code→push
// pipeline at each callsite (see Next).
type Workflow struct {
	Name    string
	Summary string

	stages  map[string]*Command
	order   []string
	prereqs map[string][]string
}

// NewWorkflow constructs an empty workflow. Callers add stages with
// Register and then hand Command() to cli.Register.
func NewWorkflow(name, summary string) *Workflow {
	return &Workflow{
		Name:    name,
		Summary: summary,
		stages:  map[string]*Command{},
		prereqs: map[string][]string{},
	}
}

// Register adds a stage command to this workflow. Panics on duplicate
// stage names within the workflow — same contract as top-level Register.
// Optional prereq stage names record that c's satisfaction depends on
// those stages' latest work turns; the list is consumed by Next,
// checkStaleness (push), and upstreamChangeBanner (stage session).
func (w *Workflow) Register(c *Command, prereqs ...string) {
	if _, dup := w.stages[c.Name]; dup {
		panic("cli: duplicate stage " + w.Name + " " + c.Name)
	}
	w.stages[c.Name] = c
	w.order = append(w.order, c.Name)
	if len(prereqs) > 0 {
		w.prereqs[c.Name] = append([]string(nil), prereqs...)
	}
}

// Stages returns the registered stage names in registration order.
func (w *Workflow) Stages() []string {
	out := make([]string, len(w.order))
	copy(out, w.order)
	return out
}

// Prereqs returns the prereq doc ids for stage, or nil if stage has
// none (or isn't part of this workflow).
func (w *Workflow) Prereqs(stage string) []string {
	return w.prereqs[stage]
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
