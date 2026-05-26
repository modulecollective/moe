package cli

import (
	"flag"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/dash"
)

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (backlog / runs)",
		Run:     runDash,
	})
}

// runDash is the cli/handler. Loads the inputs the dash package
// needs (run scan, journal index, open-session list, per-run
// next-stage decisions, per-project twin configs) and hands them to
// dash for assembly + render.
func runDash(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "show everything (no dormancy filter, no completed-run cap)")
	project := fs.String("project", "", "show only rows whose run belongs to this project")
	workflow := fs.String("workflow", "", "show only rows whose run uses this workflow")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe dash [--all] [--project <id>] [--workflow <name>]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

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

	now := time.Now().UTC()
	snap, err := GatherDashSnapshot(root, now, DashFilter{
		All:            *all,
		ProjectFilter:  *project,
		WorkflowFilter: *workflow,
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	state := dash.FactoryStateFromRows(snap.Rows)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Mark the dash render with the same one-line gradient bar every
	// stage session opens with, suffixed with the render timestamp so
	// the operator can tell a stale tab from a fresh one. Dash refreshes
	// are frequent, so we keep it to one line instead of a multi-line
	// block.
	banner.Dash(stdout, now)
	dash.Render(stdout, now, snap.Rows, snap.ProjectCount, snap.ActiveProjects, *all, state, r)
	return 0
}
