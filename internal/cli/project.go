package cli

import (
	"flag"
	"io"
	"os"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
)

func init() {
	Register(&Command{
		Name:    "project",
		Summary: "manage projects (subcommands: add, remove)",
		Run:     runProject,
	})
}

func runProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		moePrintln(stdout, "usage: moe project <subcommand> [args...]")
		moePrintln(stdout, "subcommands: add, remove")
		return 0
	}
	switch args[0] {
	case "add":
		return runProjectAdd(args[1:], stdout, stderr)
	case "remove":
		return runProjectRemove(args[1:], stdout, stderr)
	default:
		moePrintf(stderr, "unknown project subcommand %q\n", args[0])
		return 2
	}
}

func runProjectAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe project add <repo-url>") }
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
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	var md *project.Metadata
	err = withRepoLock(root, repolock.Options{Purpose: "project-add"}, func() error {
		m, err := project.Register(root, url, project.Options{})
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "registered %s (branch %s) at %s\n", md.ID, md.DefaultBranch, md.Submodule)
	return 0
}

func runProjectRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe project remove <id>") }
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
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	err = withRepoLock(root, repolock.Options{Purpose: "project-remove"}, func() error {
		return project.Unregister(root, id)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "unregistered %s\n", id)
	return 0
}
