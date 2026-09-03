package cli

import (
	"flag"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
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
		Name:    "mode",
		Summary: "read or set a project's mode (paused/safe/auto)",
		Run:     runProjectMode,
	})
	g.Register(&Command{
		Name:    "ship",
		Summary: "read or set how a project's finished runs land (pr/merge)",
		Run:     runProjectShip,
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
	err = repolock.With(root, repolock.Options{Purpose: "project-add"}, func() error {
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
		moePrintf(stdout, "%s\t%s\t%s\t%s\t%s\n", md.ID, project.ModeOf(md), project.ShipOf(md), md.DefaultBranch, md.Remote)
	}
	return 0
}

// runProjectMode reads with no argument and sets with one. The
// no-argument read is why this isn't write-only: "what is this project
// allowed to do on its own" is a question the operator asks more often
// than they change the answer.
func runProjectMode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project mode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe project mode <id> [paused|safe|auto]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Caps what the heartbeat may start in this project. Without a mode")
		moePrintln(stderr, "word it reports the current one.")
		moePrintln(stderr, "")
		moePrintln(stderr, "  paused  the heartbeat never sweeps the project")
		moePrintln(stderr, "  safe    it sweeps and grooms, but starts only threads the")
		moePrintln(stderr, "          operator marked (an advance, a workflow tag, a chore)")
		moePrintln(stderr, "  auto    it starts every kickable parked thread (the default)")
		moePrintln(stderr, "")
		moePrintln(stderr, "The mode binds the clock, not you: bangs, stage verbs, chain kicks")
		moePrintln(stderr, "and a hand-typed `moe pulse new --dynamic` run in every mode.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := project.Load(root, id)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if fs.NArg() == 1 {
		moePrintf(stdout, "%s: %s\n", id, project.ModeOf(md))
		return 0
	}
	mode, err := project.ParseMode(fs.Arg(1))
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 2
	}
	// Not the defence — SetMode re-checks under the lock, and that is
	// the copy serve leans on. This one buys skipping the pull-push
	// round-trip for a no-op, and the friendlier line; deleting it
	// costs a wasted sync, not correctness.
	if project.ModeOf(md) == mode {
		moePrintf(stdout, "%s: %s (unchanged)\n", id, mode)
		return 0
	}
	err = repolock.With(root, repolock.Options{Purpose: "project-mode"}, func() error {
		return project.SetMode(root, id, mode)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "%s: %s\n", id, mode)
	return 0
}

// runProjectShip reads with no argument and sets with one, exactly as
// runProjectMode does. Same reason for the read: "how does work land
// here" is asked far more often than it is changed — and every bang,
// kick and heartbeat ride now answers to it.
func runProjectShip(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("project ship", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe project ship <id> [pr|merge]")
		moePrintln(stderr, "")
		moePrintln(stderr, "How a finished run lands in this project. Without a route word it")
		moePrintln(stderr, "reports the current one.")
		moePrintln(stderr, "")
		moePrintln(stderr, "  pr     push the branch and open a PR; the run stays `pushed` and")
		moePrintln(stderr, "         keeps its sandbox until the PR merges (the default)")
		moePrintln(stderr, "  merge  fast-forward the default branch, delete the remote branch,")
		moePrintln(stderr, "         drop the sandbox")
		moePrintln(stderr, "")
		moePrintln(stderr, "Binds every unflagged ship — bare `moe sdlc push`, `!!` / `!!!`,")
		moePrintln(stderr, "`moe chain kick`, and the heartbeat's rides. `--pr` / `--merge` on")
		moePrintln(stderr, "push, and m/p at the chain prompt, override it per ship.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := project.Load(root, id)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if fs.NArg() == 1 {
		moePrintf(stdout, "%s: %s\n", id, project.ShipOf(md))
		return 0
	}
	ship, err := project.ParseShip(fs.Arg(1))
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 2
	}
	// Not the defence either, and the same purchase as the mode copy
	// above: SetShip re-checks under the lock, this one only skips the
	// pull-push for a no-op.
	if project.ShipOf(md) == ship {
		moePrintf(stdout, "%s: %s (unchanged)\n", id, ship)
		return 0
	}
	err = repolock.With(root, repolock.Options{Purpose: "project-ship"}, func() error {
		return project.SetShip(root, id, ship)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "%s: %s\n", id, ship)
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
	err = repolock.With(root, repolock.Options{Purpose: "project-remove"}, func() error {
		return project.Unregister(root, id)
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "unregistered %s\n", id)
	return 0
}
