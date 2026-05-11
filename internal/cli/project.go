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
	g := NewCommandGroup("project", "manage projects (subcommands: add, remove)")
	g.Register(&Command{
		Name:    "add",
		Summary: "register a project from a remote git URL",
		Run:     runProjectAdd,
	})
	g.Register(&Command{
		Name:    "remove",
		Summary: "unregister a project by id",
		Run:     runProjectRemove,
	})
	RegisterGroup(g)
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
