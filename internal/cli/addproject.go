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
		Name:    "add-project",
		Summary: "register a target repo as a submodule",
		Run:     runAddProject,
	})
}

func runAddProject(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("add-project", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: moe add-project <repo-url>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	url := fs.Arg(0)

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

	md, err := project.Register(root, url, project.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "moe: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "registered %s (branch %s) at %s\n", md.ID, md.DefaultBranch, md.Submodule)
	return 0
}
