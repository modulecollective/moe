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
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
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

	mds, err := run.Scan(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	now := time.Now().UTC()

	// Open-session liveness is best-effort: a `git worktree list`
	// failure silently yields no markers.
	sessionDocsByRun := make(map[string][]string)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionDocsByRun[s.Run] = append(sessionDocsByRun[s.Run], s.Doc)
		}
	}

	// Pre-compute the per-run next-stage decision for every in-progress
	// non-idea run. classify in dash needs the decision but doesn't
	// know about the workflow registry — keeping internal/dash free
	// of cli's per-workflow plumbing.
	nextByRun := make(map[string]dash.NextDecision, len(mds))
	for _, md := range mds {
		if md.Workflow == dash.IdeaWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		wf, err := LookupWorkflow(md.Workflow)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		next, kind, err := wf.NextWithIndex(root, md, idx)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		dec := dash.NextDecision{Done: kind == NextKindDone}
		if next != "" {
			dec.Stage = next
		}
		nextByRun[md.ID] = dec
	}

	rows, err := dash.BuildRows(dash.Inputs{
		Now:              now,
		All:              *all,
		ProjectFilter:    *project,
		WorkflowFilter:   *workflow,
		Runs:             mds,
		Index:            idx,
		SessionDocsByRun: sessionDocsByRun,
		NextByRun:        nextByRun,
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	projectCount, err := dash.CountProjects(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Count *projects with at least one active run*, not active rows.
	// The footer sentence is "N project(s) registered · M with active
	// runs" — both numbers count projects. Bucketing (not md.Status)
	// is what defines "active" so the footer matches the ACTIVE
	// section: a KB run past its terminal stage is in_progress on
	// disk but lives in COMPLETED here, so its project doesn't count.
	activeProjects := map[string]struct{}{}
	for _, r := range rows {
		if r.Bucket == dash.BucketActiveRuns {
			activeProjects[r.Project] = struct{}{}
		}
	}
	activeCount := len(activeProjects)

	state := dash.FactoryStateFromRows(rows)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Mark the dash render with the same one-line gradient bar every
	// stage session opens with, suffixed with the render timestamp so
	// the operator can tell a stale tab from a fresh one. Dash refreshes
	// are frequent, so we keep it to one line instead of a multi-line
	// block.
	banner.Dash(stdout, now)
	dash.Render(stdout, now, rows, projectCount, activeCount, *all, state, r)
	return 0
}
