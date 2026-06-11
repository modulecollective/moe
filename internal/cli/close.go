package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// closeCleanup runs workflow-specific tear-down after state guards pass
// and before the shared status-flip commit. Returning an error aborts
// the close before run.json is modified.
type closeCleanup func(root string, md *run.Metadata, stdout, stderr io.Writer) error

// closeRegistration is the (subject, cleanup) pair a workflow handed
// to closeCommand, recorded so other close entry points — today
// `moe serve`'s CloseRun callback — can dispatch the same pipeline by
// workflow name instead of baking one workflow's subject and cleanup
// into their wiring.
type closeRegistration struct {
	subject string
	cleanup closeCleanup
}

var closeRegistrations = map[string]closeRegistration{}

// lookupCloseRegistration returns the (subject, cleanup) recorded when
// workflow registered its close command. ok=false means the workflow
// has no closeCommand-built close (idea is the case today — its close
// is bespoke, runopen.CloseIdea).
func lookupCloseRegistration(workflow string) (closeRegistration, bool) {
	reg, ok := closeRegistrations[workflow]
	return reg, ok
}

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
//
// Building the command also records (subject, cleanup) into the close
// registry — registration is the declaration, so a workflow can't be
// closable from the CLI and un-closable from serve by drift. Panics on
// duplicates, same contract as RegisterWorkflow.
func closeCommand(workflow, subject string, cleanup closeCleanup) *Command {
	if _, dup := closeRegistrations[workflow]; dup {
		panic("cli: duplicate close registration for workflow " + workflow)
	}
	closeRegistrations[workflow] = closeRegistration{subject: subject, cleanup: cleanup}
	return &Command{
		Name:    "close",
		Summary: "close an in-progress run without pushing",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runClose(workflow, subject, cleanup, args, stdout, stderr)
		},
		argKind: argProjectRun,
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
		if workflow == dash.IdeaWorkflow {
			moePrintf(stderr, "usage: moe idea close <project>/<run>\n")
		} else {
			moePrintf(stderr, "usage: moe %s close [--no-edit] <project>/<run>\n", workflow)
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "moe %s close: %v\n", workflow, err)
		return 2
	}

	if workflow == "sdlc" {
		resolved, code := resolveSDLCRunSlug(workflow+" close", projectID, runID, stdout, stderr)
		if code != 0 {
			return code
		}
		runID = resolved
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	if err := closeRunInProcess(root, workflow, subject, cleanup, projectID, runID, *noEdit, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "closed %s %s/%s\n", workflow, projectID, runID)
	return 0
}

// closeRunInProcess runs the shared close pipeline — state guards,
// workflow cleanup, follow-up/lore harvest, status flip, trailered
// commit — for an already-resolved run, returning an error instead of
// printing. Both the CLI wrapper (runClose) and `moe serve` (via the
// CloseRun option, wired in cli/serve.go) drive it; the CLI wrapper owns
// flag/arg parse, sdlc slug resolution, and the success/nudge print,
// while serve maps the error to an HTTP status.
//
// State-rooted refusals (wrong workflow, pushed, already-terminal) come
// back as *runopen.NotClosableError so serve can answer 409; the
// canvas-empty gate and any IO/commit failure come back as plain errors
// (serve answers 500). skipEdit threads to enterTerminal's harvest
// pre-flight: serve always passes true since it can't host $EDITOR.
//
// stdout/stderr are handed to the workflow cleanup hook; the only hook
// today (sdlc's releaseWorkspaceCleanup) logs to the process streams
// directly, so serve passes io.Discard without losing anything.
func closeRunInProcess(root, workflow, subject string, cleanup closeCleanup, projectID, runID string, skipEdit bool, stdout, stderr io.Writer) error {
	if err := requireProject(root, projectID); err != nil {
		return err
	}

	// Idea closes have no follow-ups dance — the run *is* the capture.
	// For everything else, the operator's local edits to the harvest
	// scratch files (followups.md, feedback/lore.md) are expected —
	// that's where stage-time captures land — so the clean-tree gate
	// ignores changes on those paths. Anything else dirty stays a
	// refusal.
	if workflow != dash.IdeaWorkflow {
		followupsRel := run.FollowupsPath(projectID, runID)
		loreRel := run.FeedbackPath(projectID, runID, "lore")
		dirty, derr := dirtyOutsidePaths(root, followupsRel, loreRel)
		if derr != nil {
			return derr
		}
		if dirty {
			return errors.New("working tree has uncommitted changes; commit or stash first")
		}
	} else if err := requireCleanTree(root); err != nil {
		return err
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return fmt.Errorf("%s %s/%s does not exist", workflow, projectID, runID)
		}
		return fmt.Errorf("%s: %w", workflow, err)
	}
	if md.Workflow != workflow {
		return &runopen.NotClosableError{Reason: fmt.Sprintf(
			"run %s/%s is a %s run, not %s", projectID, runID, md.Workflow, workflow)}
	}

	switch md.Status {
	case run.StatusInProgress:
		// Proceed.
	case run.StatusPushed:
		// Refusing here keeps PR-state reconciliation on a single path
		// (GitHub → sync); letting local close race the remote state
		// risks divergence on partial failure.
		return &runopen.NotClosableError{Reason: fmt.Sprintf(
			"%s %s/%s is pushed — close the PR on GitHub and run `moe sync` to reconcile",
			workflow, projectID, runID)}
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		return &runopen.NotClosableError{Reason: fmt.Sprintf(
			"%s %s/%s already %s", workflow, projectID, runID, md.Status)}
	default:
		return &runopen.NotClosableError{Reason: fmt.Sprintf(
			"%s %s/%s has unexpected status %q", workflow, projectID, runID, md.Status)}
	}

	// Mirror commitTurn's per-turn predicate at the close seal: every
	// document the run reached must have a non-empty canvas on disk.
	// This is the post-merge belt to gate-1's pre-merge braces — it
	// catches a canvas hand-edited or `git rm`'d to zero bytes after
	// the session merged. Idea is exempt: its content.md is the
	// operator's free-form capture written at open time, and an empty
	// idea on close is operator intent, not a missed write.
	//
	// The walk is over md.Documents, which only carries entries for
	// documents the run actually opened (EnsureDocument populates it
	// from the stage session). A run that never reached `code` has no
	// `code` entry to verify — same satisfaction model Workflow.Next
	// uses.
	if workflow != dash.IdeaWorkflow {
		docIDs := make([]string, 0, len(md.Documents))
		for docID := range md.Documents {
			docIDs = append(docIDs, docID)
		}
		sort.Strings(docIDs)
		for _, docID := range docIDs {
			canvasRel := run.ContentPath(md.Project, md.ID, docID)
			info, err := os.Stat(filepath.Join(root, canvasRel))
			if err != nil || info.Size() == 0 {
				return fmt.Errorf(
					"%s %s/%s: canvas %s is empty\n"+
						"  reopen the session (`moe %s %s %s/%s`) and write to the canvas,\n"+
						"  or restore the file from git history",
					workflow, projectID, runID, canvasRel,
					workflow, docID, projectID, runID)
			}
		}
	}

	msg := fmt.Sprintf(subject+"\n\n", projectID, runID) +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: workflow + "-close",
		Run:     projectID + "/" + runID,
	}, stdout, stderr, func() error {
		if cleanup != nil {
			if err := cleanup(root, md, stdout, stderr); err != nil {
				return err
			}
		}
		paths, err := enterTerminal(root, md, run.StatusClosed, skipEdit)
		if err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, paths...)
	})
}

// releaseWorkspaceCleanup releases the run's hold on its workspace.
// For a per-run sandbox that means removing the clone (the moe/<run>
// branch lives only inside that clone — push hasn't happened, the
// state-guard above catches the pushed case — so the worktree
// removal also takes the branch with it). For a named workspace it
// means dropping the claim file and leaving the directory in place
// for the next run to reuse, which is the whole point of named
// workspaces. Idempotent: a never-opened workspace (operator
// abandoned before the code stage) is a no-op either way.
//
// Used by sdlc close to release the run workspace after abandoning code
// work. It is idempotent so closing before first code attach is still safe.
func releaseWorkspaceCleanup(root string, md *run.Metadata, stdout, stderr io.Writer) error {
	if err := releaseRunWorkspace(root, md); err != nil {
		return fmt.Errorf("release workspace: %w", err)
	}
	return nil
}
