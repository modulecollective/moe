package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/lore"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

// isCaptureWorkflow reports whether a workflow is one of the cheap
// operator-authored capture surfaces — ideas and intents. Both share the
// same close shape: the run *is* the capture, so close skips the
// feedback harvest, gates on a fully clean tree, keeps the short
// `Close <wf> <p>/<r>` subject, and exempts the canvas from the
// non-empty seal (an empty capture on close is operator intent, not a
// missed write). Everything else is a staged workflow whose close runs
// the harvest and the canvas seal.
func isCaptureWorkflow(workflow string) bool { return dash.IsCapture(workflow) }

// closeCleanup runs workflow-specific teardown after the terminal commit,
// outside repolock while the caller still owns the run.
type closeCleanup func(root string, md *run.Metadata) error

type terminalCleanupError struct {
	Project, Run, Status string
	Err                  error
}

func (e *terminalCleanupError) Error() string {
	return fmt.Sprintf("run %s/%s is %s; cleanup failed and resources were retained: %v", e.Project, e.Run, e.Status, e.Err)
}
func (e *terminalCleanupError) Unwrap() error { return e.Err }

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
// is bespoke, runopen.CloseCapture).
func lookupCloseRegistration(workflow string) (closeRegistration, bool) {
	reg, ok := closeRegistrations[workflow]
	return reg, ok
}

// closeCommand builds the `close` subcommand for a workflow. The
// state-guard / status-flip / trailered-commit skeleton is shared; the
// workflow-specific piece is the optional cleanup (staged workflows remove
// the sandbox clone and its moe/<run> branch in one sweep).
//
// subject is the commit subject template — a fmt.Sprintf string taking
// two %s args (projectID, runID). It stays a parameter rather than a
// derived string so existing commit-history shapes stay stable: idea
// close lands `Close idea <p>/<r>`, while sdlc lands `Close <wf> run
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
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s close <project>/<run>\n", workflow)
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

	// Borrowed when this is the owning prompt's `x`; refused while another
	// invocation sits in the run.
	releaseRun, ok := ownRunForMutation(root, projectID, runID, stderr)
	if !ok {
		return 1
	}
	defer releaseRun()

	if err := closeRunInProcess(root, workflow, subject, cleanup, projectID, runID, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "closed %s %s/%s\n", workflow, projectID, runID)
	return 0
}

// closeRunInProcess runs the shared close pipeline — state guards,
// feedback harvest, status flip/commit, then workspace cleanup —
// for an already-resolved run, returning an error instead of
// printing. Both the CLI wrapper (runClose) and `moe serve` (via the
// CloseRun option, wired in cli/serve.go) drive it; the CLI wrapper owns
// flag/arg parse, sdlc slug resolution, and the success/nudge print,
// while serve maps the error to an HTTP status.
//
// State-rooted refusals (wrong workflow, pushed, already-terminal) come
// back as *runopen.NotClosableError so serve can answer 409; the
// canvas-empty gate and any IO/commit failure come back as plain errors
// (serve answers 500).
func closeRunInProcess(root, workflow, subject string, cleanup closeCleanup, projectID, runID string, stderr io.Writer, extraTrailers ...trailers.Block) error {
	return closeRunPrepared(root, workflow, subject, cleanup, nil, projectID, runID, stderr, extraTrailers...)
}

// prepare contributes journal content (pulse's stamp), never resource cleanup.
func closeRunPrepared(root, workflow, subject string, cleanup, prepare closeCleanup, projectID, runID string, stderr io.Writer, extraTrailers ...trailers.Block) error {
	release, err := ownRun(root, projectID, runID, false)
	if err != nil {
		return err
	}
	defer release()
	return closeRunPreparedOwned(heldRun(root, projectID, runID), root, workflow, subject, cleanup, prepare, projectID, runID, stderr, extraTrailers...)
}

func closeRunPreparedOwned(owner *runowner.Handle, root, workflow, subject string, cleanup, prepare closeCleanup, projectID, runID string, stderr io.Writer, extraTrailers ...trailers.Block) error {
	if owner == nil || !owner.Owns(root, projectID, runID) {
		return fmt.Errorf("close requires held run ownership")
	}
	if err := clonereclaim.CheckRunRecovery(owner); err != nil {
		return err
	}
	if _, err := cloneRunState(owner); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return fmt.Errorf("%s %s/%s does not exist", workflow, projectID, runID)
		}
		return err
	}
	var md *run.Metadata
	err := repolock.With(root, repolock.Options{
		Purpose: workflow + "-close",
		Run:     projectID + "/" + runID,
		// enterTerminal harvests, one commit per created idea and
		// lore file, then the cleanup runs. A long harvest can outlast
		// StaleThreshold; without the heartbeat the next acquirer
		// would take the lock out from under it.
		Heartbeat: true,
	}, func() error {
		if err := requireProject(root, projectID); err != nil {
			return err
		}

		// Capture closes (idea, intent) have no follow-ups dance — the run
		// *is* the capture. For everything else, the operator's local edits
		// to the harvest scratch files (followups.md, feedback/lore.md, feedback/twin.md) are
		// expected — that's where stage-time captures land — so the
		// clean-tree gate ignores changes on those paths. Anything else dirty
		// stays a refusal.
		//
		// lore/ is exempt for the same reason one level out: it's harvest
		// *output*, and enterTerminal stages it into this very close commit.
		// The exemption list is exactly the set of paths the close is about
		// to commit anyway, so exempting them widens nothing. It still earns
		// its keep on a retry after a failed close commit: a re-promoted
		// lore/<slug>.md that already existed is tracked-and-modified, which
		// would otherwise trip the run's own gate.
		if !isCaptureWorkflow(workflow) {
			followupsRel := run.FollowupsPath(projectID, runID)
			loreRel := run.FeedbackPath(projectID, runID, "lore")
			twinRel := run.FeedbackPath(projectID, runID, "twin")
			dirty, derr := dirtyOutsidePaths(root, followupsRel, loreRel, twinRel, lore.DirRel+"/")
			if derr != nil {
				return derr
			}
			if dirty {
				return errors.New("working tree has uncommitted changes; commit or stash first")
			}
		} else if err := requireCleanTree(root); err != nil {
			return err
		}

		fresh, err := run.Load(root, projectID, runID)
		if err != nil {
			if errors.Is(err, run.ErrRunNotFound) {
				return fmt.Errorf("%s %s/%s does not exist", workflow, projectID, runID)
			}
			return fmt.Errorf("%s: %w", workflow, err)
		}
		md = fresh
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
		// the session merged. Capture workflows (idea, intent) are exempt:
		// their content.md is the operator's free-form capture written at
		// open time, and an empty one on close is operator intent, not a
		// missed write.
		//
		// The walk is over md.Documents, which only carries entries for
		// documents the run actually opened (EnsureDocument populates it
		// from the stage session). A run that never reached `code` has no
		// `code` entry to verify — same satisfaction model Workflow.Next
		// uses.
		if !isCaptureWorkflow(workflow) {
			for _, docID := range sortedDocIDs(md) {
				// Through the shared layout resolver: a directory-form
				// document (build) has no content.md, and an ambiguous
				// or empty one refuses the same way a missing file did.
				canvasRel := canvasDiffPath(md, docID)
				body, err := run.ReadCanvas(root, md.Project, md.ID, docID)
				if err != nil || body == "" {
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
				Consent:  walkConsent(),
			}.String()
		for _, block := range extraTrailers {
			msg += block.String()
		}

		if err := session.RequireUnreserved(root, projectID, runID); err != nil {
			return err
		}

		paths, err := enterTerminalPrepared(root, md, run.StatusClosed, prepare)
		if err != nil {
			return err
		}
		return commitTerminal(root, md, run.StatusInProgress, msg, paths)
	})
	if err != nil {
		return err
	}
	if cleanup != nil {
		if err := cleanup(root, md); err != nil {
			return &terminalCleanupError{Project: md.Project, Run: md.ID, Status: md.Status, Err: err}
		}
	}

	// The close is durable and the repolock released. Advisory only,
	// and deliberately after the commit: a canvas that claims a followup
	// it never filed is a real dropped thread, but the rules are prose
	// regexes and blocking on one would risk wedging a cascade on a
	// false positive. Capture workflows have no canvas-vs-followups
	// relationship to check.
	if !isCaptureWorkflow(workflow) {
		warnUnverifiedFollowupClaims(root, md, stderr)
	}
	return nil
}

// releaseWorkspaceCleanup releases the run's hold on its workspace.
// For a per-run sandbox that means removing the clone (the moe/<run>
// branch lives only inside that clone — push hasn't happened, the
// state-guard above catches the pushed case — so the worktree
// removal also takes the branch with it). For a named workspace it
// means dropping the claim file and leaving the directory in place
// for the next run to reuse, which is the whole point of named
// workspaces. Idempotent: a never-opened workspace (operator
// abandoned before the first source stage) is a no-op either way.
//
// Used by staged-workflow closes, including automatic pulse disposal.
func releaseWorkspaceCleanup(root string, md *run.Metadata) error {
	if err := releaseRunWorkspace(heldRun(root, md.Project, md.ID), md); err != nil {
		return fmt.Errorf("release workspace: %w", err)
	}
	return nil
}
