package cli

import (
	"fmt"
	"io"
	"sort"
)

// CommandGroup is a top-level verb that dispatches to nested
// subcommands, e.g. `moe sdlc design` or `moe project new`. It owns
// the per-verb usage line and the subcommand dispatch table; nothing
// more. The DAG of stages a workflow walks lives separately in
// Workflow — CommandGroup carries no stage order, no prereqs, no
// dash presence.
//
// Workflows pair a CommandGroup (for dispatch) with a Workflow (for
// the stage ladder); non-workflow verbs like `project` only have a
// CommandGroup. See projects/moe/runs/arch-2-workflow-overload-*
// for the design that split these.
type CommandGroup struct {
	Name     string
	Summary  string
	commands map[string]*Command
}

// NewCommandGroup constructs an empty group. Callers add subcommands
// with Register and then hand the group to RegisterGroup.
func NewCommandGroup(name, summary string) *CommandGroup {
	return &CommandGroup{
		Name:     name,
		Summary:  summary,
		commands: map[string]*Command{},
	}
}

// Register adds a subcommand to the group's dispatch table. Panics on
// duplicate names so conflicts surface at process start.
func (g *CommandGroup) Register(c *Command) {
	if _, dup := g.commands[c.Name]; dup {
		panic("cli: duplicate subcommand " + g.Name + " " + c.Name)
	}
	g.commands[c.Name] = c
}

// Lookup returns the registered subcommand named sub, or nil if no
// such name was registered. Used by callers (chain prompt, resume)
// that need to invoke a stage by name without going through the
// argv dispatcher.
func (g *CommandGroup) Lookup(sub string) *Command {
	return g.commands[sub]
}

// Command returns the group as a top-level Command — same shape as any
// other entry in the cli.commands table. The returned Command's Run
// handler expects args positioned after `moe <Name>`.
func (g *CommandGroup) Command() *Command {
	return &Command{
		Name:    g.Name,
		Summary: g.Summary,
		Run:     g.run,
	}
}

func (g *CommandGroup) run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		g.printUsage(stdout)
		return 0
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		g.printUsage(stdout)
		return 0
	}
	cmd, ok := g.commands[name]
	if !ok {
		moePrintf(stderr, "unknown %s subcommand %q\n", g.Name, name)
		g.printUsage(stderr)
		return 1
	}
	return cmd.Run(args[1:], stdout, stderr)
}

func (g *CommandGroup) printUsage(out io.Writer) {
	moePrintf(out, "usage: moe %s <subcommand> [args...]\n", g.Name)
	moePrintln(out, "")
	moePrintln(out, "subcommands:")
	names := make([]string, 0, len(g.commands))
	for n, c := range g.commands {
		if c.Hidden {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		moePrintf(out, "  %-14s  %s\n", n, g.commands[n].Summary)
	}
}

var groups = map[string]*CommandGroup{}

// RegisterGroup adds g to the group registry and also adds g.Command()
// to the top-level command table — one call covers both. Panics on
// duplicate names. Symmetric with RegisterWorkflow.
func RegisterGroup(g *CommandGroup) {
	if _, dup := groups[g.Name]; dup {
		panic("cli: duplicate group " + g.Name)
	}
	groups[g.Name] = g
	Register(g.Command())
}

// LookupGroup returns the registered group named name. An unknown or
// empty name returns an error listing the known groups so a typo at a
// dispatch site (chain prompt, resume) surfaces loudly.
func LookupGroup(name string) (*CommandGroup, error) {
	g, ok := groups[name]
	if !ok {
		return nil, fmt.Errorf("group %q not registered (known: %v)", name, groupNames())
	}
	return g, nil
}

func groupNames() []string {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
