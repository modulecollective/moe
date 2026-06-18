package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// harvestCommand builds the `harvest` subcommand for a workflow —
// sibling to `close`, but decoupled from the terminal transition.
// Harvest is welded to enterTerminal today: once a run is closed, a
// re-run of a stage rewrites followups.md with fresh entries that can
// never be picked up (close refuses an already-terminal run). This verb
// is the trigger that isn't the status flip — it re-runs the existing
// followups harvest pipeline against the run's current followups.md and
// commits the rewritten file, leaving run.json untouched.
//
// Idempotent: the pipeline skips `- [x]` lines, so a clean run finds
// nothing new and a regenerated file harvests only the fresh entries.
// Registered for every non-idea workflow (idea runs have no follow-ups
// dance — the run is the capture).
func harvestCommand(workflow string) *Command {
	return &Command{
		Name:    "harvest",
		Summary: "re-harvest a run's followups.md into ideas without closing it",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runHarvest(workflow, args, stdout, stderr)
		},
		argKind: argProjectRun,
	}
}

func runHarvest(workflow string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" harvest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// --no-edit mirrors close: skip the editor pre-flight and harvest the
	// file as-is. The pop is on by default here — the operator is
	// explicitly invoking harvest, so they get to review what fans out.
	noEdit := fs.Bool("no-edit", false, "skip the followups.md editor step (harvest the file as-is)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s harvest [--no-edit] <project>/<run>\n", workflow)
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
		moePrintf(stderr, "moe %s harvest: %v\n", workflow, err)
		return 2
	}

	// sdlc slugs walk the promoted/reopened lineage like close does, so a
	// typed parent slug resolves to its terminal descendant.
	if workflow == "sdlc" {
		resolved, code := resolveSDLCRunSlug(workflow+" harvest", projectID, runID, stdout, stderr)
		if code != 0 {
			return code
		}
		runID = resolved
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	if err := harvestRunInProcess(root, workflow, projectID, runID, *noEdit, stdout, stderr); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "harvested %s %s/%s\n", workflow, projectID, runID)
	return 0
}

// harvestRunInProcess re-runs the followups harvest for an already-
// resolved run and commits the rewritten followups.md — without flipping
// run status. Unlike closeRunInProcess it is status-agnostic: harvest is
// journal-local (it creates idea runs and rewrites a scratch file),
// touching neither the PR nor run.json, so it is safe on in_progress,
// closed, merged, or pushed runs. That status-blindness is the whole
// point — the reported gap is a closed run whose regenerated follow-ups
// can never reach ideas through the close path.
//
// Lore is deliberately out of scope here (close still harvests both):
// the reported need is ideas, and the stray-content backstop already
// covers lore's silent-loss case. A symmetric lore re-harvest is a
// separate verb if the gap ever bites.
func harvestRunInProcess(root, workflow, projectID, runID string, skipEdit bool, stdout, stderr io.Writer) error {
	if err := requireProject(root, projectID); err != nil {
		return err
	}

	// Same clean-tree tolerance as close: the operator's local edits to
	// followups.md are expected (it's the file being harvested), so the
	// gate ignores that path while still refusing on anything else dirty.
	followupsRel := run.FollowupsPath(projectID, runID)
	dirty, derr := dirtyOutsidePaths(root, followupsRel)
	if derr != nil {
		return derr
	}
	if dirty {
		return errors.New("working tree has uncommitted changes; commit or stash first")
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return fmt.Errorf("%s %s/%s does not exist", workflow, projectID, runID)
		}
		return fmt.Errorf("%s: %w", workflow, err)
	}
	if md.Workflow != workflow {
		return fmt.Errorf("run %s/%s is a %s run, not %s", projectID, runID, md.Workflow, workflow)
	}
	if workflow == dash.IdeaWorkflow {
		return fmt.Errorf("idea %s/%s has no follow-ups to harvest", projectID, runID)
	}

	msg := fmt.Sprintf("harvest: capture follow-ups for %s/%s\n\n", projectID, runID) +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
		}.String()
	return sync.WithJournalPush(root, repolock.Options{
		Purpose: workflow + "-harvest",
		Run:     projectID + "/" + runID,
	}, stdout, stderr, func() error {
		if err := harvestFollowups(root, projectID, runID, workflow, skipEdit); err != nil {
			return err
		}
		// Nothing on disk means nothing fanned out — no file to commit.
		rel := run.FollowupsPath(projectID, runID)
		if _, statErr := os.Stat(filepath.Join(root, rel)); statErr != nil {
			return nil
		}
		// A clean re-run (all `- [x]`) leaves followups.md byte-identical,
		// so there's nothing staged: swallow ErrNothingToCommit and report
		// the no-op as success. The harvested ideas' own open commits
		// (written by createIdea inside the pipeline) are already landed.
		if err := run.StageAndCommit(root, msg, rel); err != nil && !errors.Is(err, run.ErrNothingToCommit) {
			return err
		}
		return nil
	})
}
