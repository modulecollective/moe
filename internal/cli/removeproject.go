package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/project"
)

func init() {
	Register(&Command{
		Name:    "remove-project",
		Summary: "unregister a project and remove its submodule",
		Run:     runRemoveProject,
	})
}

func runRemoveProject(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remove-project", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: moe remove-project <id>")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	if err := project.Unregister(root, id); err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "unregistered %s\n", id)
	return 0
}
