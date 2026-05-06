package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
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
	// --no-edit skips the followups.md editor step (idiom from `git
	// commit --no-edit`). The harvester still runs against whatever is
	// already on disk, so operators driving close from scripts/CI can
	// trim the file ahead of time and keep close non-interactive.
	noEdit := fs.Bool("no-edit", false, "skip the followups.md editor step (harvest the file as-is)")
	fs.Usage = func() {
		if workflow == ideaWorkflow {
			moePrintf(stderr, "usage: moe idea close <project> <run>\n")
		} else {
			moePrintf(stderr, "usage: moe %s close [--no-edit] <project> <run>\n", workflow)
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
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
	// Idea closes have no follow-ups dance — the run *is* the capture.
	// For everything else, the operator's local edits to followups.md
	// are expected (that's where stage-time captures land), so the
	// clean-tree gate ignores changes on that path. Anything else
	// dirty stays a refusal.
	harvest := workflow != ideaWorkflow
	followupsRel := run.FollowupsPath(projectID, runID)
	if harvest {
		dirty, derr := dirtyOutsidePath(root, followupsRel)
		if derr != nil {
			moePrintf(stderr, "%v\n", derr)
			return 1
		}
		if dirty {
			moePrintf(stderr, "working tree has uncommitted changes; commit or stash first\n")
			return 1
		}
	} else if err := requireCleanTree(root); err != nil {
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

	msg := fmt.Sprintf(subject+`

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
`, projectID, runID, runID, projectID, workflow)
	err = withRepoLock(root, repolock.Options{
		Purpose: workflow + "-close",
		Run:     projectID + "/" + runID,
	}, func() error {
		if cleanup != nil {
			if err := cleanup(root, md, stdout, stderr); err != nil {
				return err
			}
		}
		paths, err := enterTerminal(root, md, run.StatusClosed, *noEdit)
		if err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, paths...)
	})
	if err != nil {
		moePrintf(stderr, "%s: close: %v\n", workflow, err)
		return 1
	}
	moePrintf(stdout, "closed %s %s/%s\n", workflow, projectID, runID)
	if nudge := twinReflectNudge(root, projectID, runID, workflow); nudge != "" {
		moePrintf(stdout, "%s", nudge)
	}
	return 0
}

// twinReflectNudge returns a one-line suggestion to reflect the twin
// when the project's twin checkpoint is older than the run that just
// closed. Empty when the project has no twin, the twin was reflected
// after the run's last activity, or the workflow is one whose close
// shouldn't carry the nudge (idea, twin itself).
//
// The nudge is advisory — it doesn't gate close, doesn't auto-run
// reflect, and doesn't fail when the freshness check itself errors.
// Intent: lower-friction reminder so durable-layer drift surfaces
// while the just-finished run is still fresh in the operator's head.
func twinReflectNudge(root, projectID, runID, workflow string) string {
	switch workflow {
	case ideaWorkflow, "twin":
		return ""
	}
	cfg, err := twinWikiBuilder(root, projectID)
	if err != nil || cfg == nil {
		return ""
	}
	if _, statErr := os.Stat(cfg.ContentDir); statErr != nil {
		return ""
	}
	cp, ok, err := wiki.ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return ""
	}
	if !ok || cp.LastIngestAt == "" {
		return fmt.Sprintf(
			"twin never reflected — consider `moe twin reflect %s` when you have a moment\n",
			projectID,
		)
	}
	last, err := time.Parse(time.RFC3339, cp.LastIngestAt)
	if err != nil {
		return ""
	}
	when, err := run.LastActivity(root, runID)
	if err != nil || when.IsZero() {
		return ""
	}
	if last.After(when) {
		return ""
	}
	return fmt.Sprintf(
		"twin not reflected since %s — consider `moe twin reflect %s` when you have a moment\n",
		last.Format("2006-01-02"), projectID,
	)
}

// sdlcCloseCleanup releases the run's hold on its workspace. For a
// per-run sandbox that means removing the clone (the moe/<run> branch
// lives only inside that clone — push hasn't happened, the
// state-guard above catches the pushed case — so the worktree
// removal also takes the branch with it). For a named workspace it
// means dropping the claim file and leaving the directory in place
// for the next run to reuse, which is the whole point of named
// workspaces. Idempotent: a never-opened workspace (operator
// abandoned before `moe sdlc code`) is a no-op either way.
func sdlcCloseCleanup(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	if err := releaseRunWorkspace(root, md); err != nil {
		return fmt.Errorf("release workspace: %w", err)
	}
	return nil
}
