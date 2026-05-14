// Package cli dispatches moe subcommands.
//
// Commands register themselves into a table in init(). Run looks up the first
// argument and hands the rest to the command. Keeping the dispatcher as a
// library function (rather than inlined in main) makes it testable with
// in-memory io.Writers — see cli_test.go.
package cli

import (
	"io"
	"sort"
)

// Command is a single moe subcommand.
type Command struct {
	Name    string
	Summary string
	Run     func(args []string, stdout, stderr io.Writer) int
	// Hidden omits the command from usage listings while keeping it
	// reachable by exact name. Used for stub stages (e.g. the idea
	// workflow's single stage is load-bearing for Workflow.Next but
	// isn't an operator-facing verb).
	Hidden bool
}

var commands = map[string]*Command{}

// Register adds a command to the dispatch table. Panics on duplicate names so
// conflicts surface at process start rather than on first invocation.
func Register(c *Command) {
	if _, dup := commands[c.Name]; dup {
		panic("cli: duplicate command " + c.Name)
	}
	commands[c.Name] = c
}

// Run dispatches one invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	installGitTrace()
	if len(args) == 0 {
		PrintUsage(stdout)
		moePrintln(stdout, "try 'moe dash'")
		return 0
	}
	name := args[0]
	cmd, ok := commands[name]
	if !ok {
		moePrintf(stderr, "unknown command %q\n", name)
		PrintUsage(stderr)
		return 1
	}
	return cmd.Run(args[1:], stdout, stderr)
}

// PrintUsage writes the top-level help message.
func PrintUsage(w io.Writer) {
	moePrintln(w, "usage: moe <command> [args...]")
	moePrintln(w, "")
	moePrintln(w, "commands:")
	names := make([]string, 0, len(commands))
	for n, c := range commands {
		if c.Hidden {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		moePrintf(w, "  %-14s  %s\n", n, commands[n].Summary)
	}
}
