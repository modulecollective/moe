package cli

import (
	"flag"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/workspace"
)

func init() {
	g := NewCommandGroup("project", "manage projects")
	g.Register(&Command{
		Name:    "add",
		Summary: "register a project from a remote git URL",
		Run:     runProjectAdd,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "list registered projects",
		Run:     runProjectList,
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
	err = sync.WithJournalPush(root, repolock.Options{Purpose: "project-add"}, stdout, stderr, func() error {
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

func runProjectList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe project list") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	mds, warnings, err := project.List(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	for _, w := range warnings {
		moePrintf(stderr, "project list: skipping %s: %v\n", w.ID, w.Err)
	}
	if len(mds) == 0 {
		moePrintln(stdout, "(no projects registered)")
		return 0
	}
	for _, md := range mds {
		moePrintf(stdout, "%s\t%s\t%s\n", md.ID, md.DefaultBranch, md.Remote)
	}
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
	// Refuse if any named workspace still exists under .moe/named/<id>/*.
	// Removing the project would orphan those dirs from the CLI surface
	// — the operator has to clear them first via `moe workspace remove`.
	// Cross-package guard lives in the CLI wrapper so internal/project
	// doesn't grow a dep on internal/workspace just for this check.
	infos, err := workspace.List(root, id)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if len(infos) > 0 {
		names := make([]string, 0, len(infos))
		for _, info := range infos {
			names = append(names, info.Name)
		}
		moePrintf(stderr, "project %s has %d named workspace(s): %s\n",
			id, len(infos), strings.Join(names, ", "))
		moePrintf(stderr, "       remove each with `moe workspace remove %s/<name>` first\n", id)
		return 1
	}
	err = sync.WithJournalPush(root, repolock.Options{Purpose: "project-remove"}, stdout, stderr, func() error {
		return project.Unregister(root, id)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "unregistered %s\n", id)
	return 0
}
