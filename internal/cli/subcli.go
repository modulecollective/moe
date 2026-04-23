package cli

import (
	"io"
	"sort"
)

// Workflow groups a set of Commands under one top-level verb,
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
//
// Workflow subcommands come in two shapes: stages (Register), which
// participate in the stage ladder — and facades (RegisterFacade) for
// workflow entry points that dispatch alongside the stages but aren't
// themselves stages to satisfy. `moe sdlc new` is a facade: it opens a
// run, it doesn't advance one.
type Workflow struct {
	Name    string
	Summary string

	// ExposedViaCLI controls whether `moe workflow <Name>` dispatches
	// to this workflow. Default true; idea sets it false so its runs
	// are still registered for LookupWorkflow / dash while the verbs
	// remain driven by the top-level `moe idea` command.
	ExposedViaCLI bool

	// commands is the full dispatch table — stages and facades —
	// keyed by subcommand name. All entries are addressable as
	// `moe workflow <Name> <sub>`.
	commands map[string]*Command
	// stageOrder is the subset of commands that make up the stage
	// ladder, in registration order. Stages() returns a copy; Next
	// walks it to compute the next incomplete stage.
	stageOrder []string
	prereqs    map[string][]string
}

// NewWorkflow constructs an empty workflow. Callers add stages with
// Register and then hand the workflow to RegisterWorkflow. ExposedViaCLI
// defaults to true.
func NewWorkflow(name, summary string) *Workflow {
	return &Workflow{
		Name:          name,
		Summary:       summary,
		ExposedViaCLI: true,
		commands:      map[string]*Command{},
		prereqs:       map[string][]string{},
	}
}

// Register adds a stage command to this workflow's ladder. Panics on
// duplicate subcommand names — same contract as top-level Register.
// Optional prereq stage names record that c's satisfaction depends on
// those stages' latest work turns; the list is consumed by Next,
// checkStaleness (push), and upstreamChangeBanner (stage session).
func (w *Workflow) Register(c *Command, prereqs ...string) {
	if _, dup := w.commands[c.Name]; dup {
		panic("cli: duplicate subcommand " + w.Name + " " + c.Name)
	}
	w.commands[c.Name] = c
	w.stageOrder = append(w.stageOrder, c.Name)
	if len(prereqs) > 0 {
		w.prereqs[c.Name] = append([]string(nil), prereqs...)
	}
}

// RegisterFacade adds a non-stage subcommand to the workflow's
// dispatch table — a command accessible as `moe <workflow> <name>` but
// not part of the stage ladder. Used for workflow entry points like
// `new` whose job is to create or manipulate runs rather than advance
// them through stages.
func (w *Workflow) RegisterFacade(c *Command) {
	if _, dup := w.commands[c.Name]; dup {
		panic("cli: duplicate subcommand " + w.Name + " " + c.Name)
	}
	w.commands[c.Name] = c
}

// Stages returns the registered stage names in registration order.
// Facades (e.g., `new`) are not included.
func (w *Workflow) Stages() []string {
	out := make([]string, len(w.stageOrder))
	copy(out, w.stageOrder)
	return out
}

// Prereqs returns the prereq doc ids for stage, or nil if stage has
// none (or isn't part of this workflow).
func (w *Workflow) Prereqs(stage string) []string {
	return w.prereqs[stage]
}

// Command returns the workflow as a Command suitable for nesting under
// the top-level `workflow` dispatcher. The returned Command's Run
// handler expects args positioned after `moe workflow <Name>`.
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
	cmd, ok := w.commands[name]
	if !ok {
		moePrintf(stderr, "unknown %s subcommand %q\n", w.Name, name)
		w.printUsage(stderr)
		return 1
	}
	return cmd.Run(args[1:], stdout, stderr)
}

func (w *Workflow) printUsage(out io.Writer) {
	moePrintf(out, "usage: moe workflow %s <subcommand> [args...]\n", w.Name)
	moePrintln(out, "")
	moePrintln(out, "subcommands:")
	names := make([]string, 0, len(w.commands))
	for n, c := range w.commands {
		if c.Hidden {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		moePrintf(out, "  %-14s  %s\n", n, w.commands[n].Summary)
	}
}
