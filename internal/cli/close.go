package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
)

// closeCleanup runs workflow-specific tear-down after state guards pass
// and before the shared status-flip commit. Returning an error aborts
// the close before run.json is modified.
type closeCleanup func(root string, md *run.Metadata, stdout, stderr io.Writer) error

// closeCommand builds the `close` subcommand for a workflow. The
// state-guard / status-flip / trailered-commit skeleton is shared; the
// workflow-specific piece is the optional cleanup (e.g., sdlc removes
// the sandbox clone and its moe/<run> branch in one sweep).
//
// subject is the commit subject template — a fmt.Sprintf string taking
// two %s args (projectID, runID). It stays a parameter rather than a
// derived string so existing commit-history shapes stay stable: idea
// close lands `Close idea <p>/<r>`, while sdlc/kb land `Close <wf> run
// <p>/<r>` per the design doc.
func closeCommand(workflow, subject string, cleanup closeCleanup) *Command {
	return &Command{
		Name:    "close",
		Summary: "close an in-progress run without pushing",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runClose(workflow, subject, cleanup, args, stdout, stderr)
		},
	}
}

func runClose(workflow, subject string, cleanup closeCleanup, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Idea runs are driven by the top-level `moe idea` command, so
	// their usage line is the short form. Every other workflow's close
	// is dispatched via `moe workflow <wf> close`.
	fs.Usage = func() {
		if workflow == ideaWorkflow {
			moePrintf(stderr, "usage: moe idea close <project> <run>\n")
		} else {
			moePrintf(stderr, "usage: moe workflow %s close <project> <run>\n", workflow)
		}
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)
	runID := fs.Arg(1)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			moePrintf(stderr, "%s %s/%s does not exist\n", workflow, projectID, runID)
			return 1
		}
		moePrintf(stderr, "%s: %v\n", workflow, err)
		return 1
	}
	if md.Workflow != workflow {
		moePrintf(stderr, "run %s/%s is a %s run, not %s\n", projectID, runID, md.Workflow, workflow)
		return 1
	}

	switch md.Status {
	case run.StatusInProgress:
		// Proceed.
	case run.StatusPushed:
		// Refusing here keeps PR-state reconciliation on a single path
		// (GitHub → sync); letting local close race the remote state
		// risks divergence on partial failure.
		moePrintf(stderr,
			"%s %s/%s is pushed — close the PR on GitHub and run `moe sync` to reconcile\n",
			workflow, projectID, runID)
		return 1
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "%s %s/%s already %s\n", workflow, projectID, runID, md.Status)
		return 1
	default:
		moePrintf(stderr, "%s %s/%s has unexpected status %q\n", workflow, projectID, runID, md.Status)
		return 1
	}

	if cleanup != nil {
		if err := cleanup(root, md, stdout, stderr); err != nil {
			moePrintf(stderr, "%s: close: %v\n", workflow, err)
			return 1
		}
	}

	md.Status = run.StatusClosed
	runJSONRel := filepath.Join(run.Dir(projectID, runID), "run.json")
	msg := fmt.Sprintf(subject+`

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
`, projectID, runID, runID, projectID, workflow)
	err = withRepoLock(root, repolock.Options{
		Purpose: workflow + "-close",
		Run:     projectID + "/" + runID,
	}, func() error {
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
	if err != nil {
		moePrintf(stderr, "%s: close: %v\n", workflow, err)
		return 1
	}
	moePrintf(stdout, "closed %s %s/%s\n", workflow, projectID, runID)
	return 0
}

// sdlcCloseCleanup removes the run's sandbox clone. The moe/<run>
// branch lives only inside that clone (push hasn't happened — refuse
// state-guard catches the pushed case above), so sandbox.Remove is
// the single step that takes both branch and worktree with it.
// Idempotent: a never-opened sandbox (operator abandoned before
// `moe workflow sdlc code`) is a no-op.
func sdlcCloseCleanup(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	if err := sandbox.Remove(root, md.Project, md.ID); err != nil {
		return fmt.Errorf("remove sandbox: %w", err)
	}
	return nil
}
