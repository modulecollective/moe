package cli

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/queue"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/wiki"
)

func init() {
	Register(&Command{
		Name:    "dash",
		Summary: "show the home-screen dashboard (backlog / runs)",
		Run:     runDash,
	})
}

// runDash is the cli/handler. Loads the inputs the dash package
// needs (run scan, journal index, queue membership, open-session
// list, per-run next-stage decisions, per-project twin configs) and
// hands them to dash for assembly + render.
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

	// Queue membership is best-effort decoration: a corrupt or missing
	// queue.json silently yields no markers and the dash still renders.
	// Loud errors on bad queue state belong in queue add/list/run.
	queuedSet := make(map[dash.QueueKey]struct{})
	if items, err := queue.Load(root); err == nil {
		for _, it := range items {
			queuedSet[dash.QueueKey{Workflow: it.Workflow, Project: it.Project, Run: it.Run}] = struct{}{}
		}
	}
	// Open-session liveness is best-effort the same way: a `git
	// worktree list` failure silently yields no markers.
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
		next, kind, err := wf.Next(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		dec := dash.NextDecision{Done: kind == NextKindDone}
		if next != nil {
			dec.Stage = next.Name
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
		QueuedSet:        queuedSet,
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

	twinConfigs, err := loadTwinConfigs(root, *project)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	twinRows, err := dash.BuildTwinRows(root, mds, idx, *project, twinConfigs)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Count active from buckets rather than md.Status so the footer
	// matches what the ACTIVE section actually shows. A KB run past
	// its terminal stage is Status=in_progress on disk but lives in
	// COMPLETED here — counting it as active would mislead.
	activeCount := 0
	for _, r := range rows {
		if r.Bucket == dash.BucketActiveRuns {
			activeCount++
		}
	}

	state := dash.FactoryStateFromRows(rows)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	dash.Render(stdout, now, rows, twinRows, projectCount, activeCount, *all, state, r)
	return 0
}

// loadTwinConfigs builds the per-project wiki.Config slice that
// dash.BuildTwinRows consumes. Filters out projects whose
// twinWikiBuilder errors or returns nil (no twin yet).
func loadTwinConfigs(root, projectFilter string) ([]wiki.Config, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "project.json"))
	if err != nil {
		return nil, fmt.Errorf("dash: glob projects: %w", err)
	}
	var configs []wiki.Config
	for _, m := range matches {
		projectID := filepath.Base(filepath.Dir(m))
		if projectFilter != "" && projectID != projectFilter {
			continue
		}
		cfg, err := twinWikiBuilder(root, projectID)
		if err != nil || cfg == nil {
			continue
		}
		configs = append(configs, *cfg)
	}
	return configs, nil
}
