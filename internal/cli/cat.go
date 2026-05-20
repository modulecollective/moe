package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// `moe <workflow> cat` is the canvas-dump verb shared across every
// run-bearing workflow. Six per-workflow wrappers (in idea/sdlc/kb/
// metamoe/hooks_workflow/twin) parse positional args and delegate
// here; this file owns the resolver (worktree-vs-checkout, @latest,
// stage validation, error shapes) and the io.Copy to stdout.
//
// The surface lives at the workflow group, not as a top-level `moe
// cat`, so each verb is namespaced like every other workflow verb
// (`moe sdlc design`, `moe idea edit`, `moe sdlc cat`). The slug ↔
// run-id equivalence in the idea workflow is preserved by passing
// the workflow's only stage as a non-empty defaultStage.

// latestRunSentinel is the magic <run> token that picks the most
// recently active run in <project>+<workflow>. Resolved before any
// downstream validation; see pickLatestRun.
const latestRunSentinel = "@latest"

// runCat returns the typed Command.Run for `moe <workflow> cat`.
// defaultStage, when non-empty, is the stage used when the operator
// omits a stage argument — picked up automatically by single-stage
// workflows (idea, meta-moe, hooks). Pass "" to force the operator
// to name a stage.
func runCat(workflow, defaultStage string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet(workflow+" cat", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			if defaultStage == "" {
				moePrintf(stderr, "usage: moe %s cat <project> <run> <stage>\n", workflow)
			} else {
				moePrintf(stderr, "usage: moe %s cat <project> <run> [<stage>]\n", workflow)
			}
		}
		if err := fs.Parse(reorderFlags(fs, args)); err != nil {
			return 2
		}
		n := fs.NArg()
		// Two args is always valid (stage defaults when allowed); three
		// args is valid for either flavour. One arg or four+ is a usage
		// error; two args with no defaultStage is also a usage error.
		if n < 2 || n > 3 {
			fs.Usage()
			return 2
		}
		if n == 2 && defaultStage == "" {
			fs.Usage()
			return 2
		}
		projectID := fs.Arg(0)
		runID := fs.Arg(1)
		stage := defaultStage
		if n == 3 {
			stage = fs.Arg(2)
		}

		root, err := findRoot(stderr)
		if err != nil {
			return 1
		}
		abs, err := resolveCanvasPath(root, workflow, projectID, runID, stage)
		if err != nil {
			moePrintf(stderr, "moe %s cat: %v\n", workflow, err)
			return 1
		}
		f, err := os.Open(abs)
		if err != nil {
			if os.IsNotExist(err) {
				moePrintf(stderr, "moe %s cat: no canvas yet at %s\n", workflow, abs)
				return 1
			}
			moePrintf(stderr, "moe %s cat: %v\n", workflow, err)
			return 1
		}
		defer f.Close()
		if _, err := io.Copy(stdout, f); err != nil {
			moePrintf(stderr, "moe %s cat: %v\n", workflow, err)
			return 1
		}
		return 0
	}
}

// resolveCanvasPath returns the absolute path the cat handler should
// read for (workflow, project, run, stage). Validation order:
// project exists → workflow registered → @latest resolved → run
// exists → run belongs to workflow → stage in workflow's ladder.
// Then routes between the live session's worktree (if any) and the
// operator's bureaucracy checkout.
func resolveCanvasPath(root, workflow, projectID, runID, stage string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		return "", fmt.Errorf("no such project: %s", projectID)
	}
	wf, err := LookupWorkflow(workflow)
	if err != nil {
		return "", err
	}
	if runID == latestRunSentinel {
		resolved, err := pickLatestRun(root, workflow, projectID)
		if err != nil {
			return "", err
		}
		runID = resolved
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return "", fmt.Errorf("no such run: %s/%s", projectID, runID)
		}
		return "", err
	}
	if md.Workflow != workflow {
		return "", fmt.Errorf("%s is a %s run, use 'moe %s cat'", runID, md.Workflow, md.Workflow)
	}
	stages := wf.Stages()
	if !stageRegistered(stages, stage) {
		return "", fmt.Errorf("no such stage: %s (have: %v)", stage, stages)
	}

	canvasRel := run.ContentPath(projectID, runID, stage)
	if sess := liveSessionFor(root, projectID, runID, stage); sess != nil {
		return filepath.Join(sess.WorktreePath, canvasRel), nil
	}
	return filepath.Join(root, canvasRel), nil
}

func stageRegistered(stages []string, stage string) bool {
	for _, s := range stages {
		if s == stage {
			return true
		}
	}
	return false
}

// liveSessionFor returns the open session for (run, stage), or nil
// when none exists. A session.List failure is swallowed — falling
// back to the checkout copy is the right answer: the worktree-vs-
// checkout split exists so reads pick up uncommitted agent edits,
// not so a missing worktree-list blocks the read entirely.
func liveSessionFor(root, projectID, runID, stage string) *session.Session {
	sessions, err := session.List(root)
	if err != nil {
		return nil
	}
	for _, s := range sessions {
		if s.Project == projectID && s.Run == runID && s.Doc == stage {
			return s
		}
	}
	return nil
}

// pickLatestRun resolves @latest to a concrete run id: the run in
// (project, workflow) with the most recent journal activity. Status
// doesn't matter — in-progress, merged, closed all compete equally,
// so a just-closed run remains reachable for re-reading. Returns a
// clean error when no such run exists.
func pickLatestRun(root, workflow, projectID string) (string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return "", err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return "", err
	}
	var bestID string
	var bestWhen time.Time
	for _, md := range mds {
		if md.Workflow != workflow || md.Project != projectID {
			continue
		}
		when := idx.LastActivity[md.ID]
		if bestID == "" || when.After(bestWhen) {
			bestID = md.ID
			bestWhen = when
		}
	}
	if bestID == "" {
		return "", fmt.Errorf("no runs for %s in workflow %s", projectID, workflow)
	}
	return bestID, nil
}
