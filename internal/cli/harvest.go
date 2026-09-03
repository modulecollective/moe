package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/lore"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// harvestCommand builds the `harvest` subcommand for a workflow —
// sibling to `close`, but decoupled from the terminal transition.
// Harvest is welded to enterTerminal today: once a run is closed, a
// re-run of a stage rewrites followups.md with fresh entries that can
// never be picked up (close refuses an already-terminal run). This verb
// is the trigger that isn't the status flip — it re-runs the three
// harvest pipelines (followups, lore, twin observations) against the
// run's current scratch files and commits what they rewrote, leaving
// run.json untouched.
//
// Idempotent: the pipelines skip `- [x]` lines, so a clean run finds
// nothing new and a regenerated file harvests only the fresh entries.
// Registered for every workflow that can host an agent session — which,
// since `idea edit --chat` and `intent edit --chat`, includes the
// capture workflows. Their close still skips harvest (the run *is* the
// capture), so this verb plus HarvestOnExit is the only path their
// captures have.
func harvestCommand(workflow string) *Command {
	return &Command{
		Name:    "harvest",
		Summary: "re-harvest a run's followups.md and feedback/*.md without closing it",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runHarvest(workflow, args, stdout, stderr)
		},
		argKind: harvestArgKind(workflow),
	}
}

// harvestArgKind picks the completion token kind for the verb's one
// argument. The capture workflows own their own kinds — argProjectRun
// deliberately excludes idea and intent runs — so a plain
// argProjectRun here would leave `moe idea harvest <tab>` completing
// nothing.
func harvestArgKind(workflow string) argKind {
	switch workflow {
	case dash.IdeaWorkflow:
		return argIdea
	case dash.IntentWorkflow:
		return argIntent
	default:
		return argProjectRun
	}
}

func runHarvest(workflow string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" harvest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// --no-edit mirrors close: skip the editor pre-flight and harvest the
	// files as-is. The pop is on by default here — the operator is
	// explicitly invoking harvest, so they get to review what fans out.
	noEdit := fs.Bool("no-edit", false, "skip the followups.md / feedback/*.md editor steps (harvest as-is)")
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

// harvestRunInProcess re-runs all three scratch harvests for an
// already-resolved run and commits what they rewrote — without flipping
// run status. Unlike closeRunInProcess it is status-agnostic: harvest is
// journal-local (it creates idea runs, promotes lore files, and rewrites
// the scratch files), touching neither the PR nor run.json, so it is
// safe on in_progress, closed, merged, or pushed runs. That
// status-blindness is the whole point — the reported gap is a closed run
// whose regenerated follow-ups can never reach ideas through the close
// path.
//
// All three scratch files, in enterTerminal's order and with its
// staging: the verb shipped followups-only with a note that a symmetric
// lore re-harvest could be a separate verb if the gap ever bit. It bit
// — a conversational session on an intent stranded a lore entry
// alongside its followups — and a "harvest" that silently skips part of
// the captures is the trap that produced that run, so the grouping
// lives in the one verb.
//
// No workflow is refused. The idea refusal this function shipped with
// ("the run *is* the capture") predates `idea edit --chat`: an agent
// session on a capture run files followups and lore like any other
// stage, and capture close deliberately skips harvest, so refusing here
// would leave those captures with no path at all.
func harvestRunInProcess(root, workflow, projectID, runID string, skipEdit bool, stdout, stderr io.Writer) error {
	if err := requireProject(root, projectID); err != nil {
		return err
	}

	// Same clean-tree tolerance as close, over the same path set: the
	// operator's local edits to the scratch files are expected (they're
	// what's being harvested) and lore/ is this commit's own output, so
	// the gate ignores those while still refusing on anything else dirty.
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

	msg := fmt.Sprintf("harvest: capture follow-ups, lore, and twin observations for %s/%s\n\n", projectID, runID) +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
		}.String()
	return repolock.With(root, repolock.Options{
		Purpose: workflow + "-harvest",
		Run:     projectID + "/" + runID,
	}, func() error {
		if err := harvestFollowups(root, projectID, runID, workflow, skipEdit); err != nil {
			return err
		}
		if err := harvestLore(root, projectID, runID, workflow, skipEdit); err != nil {
			return err
		}
		if err := harvestTwinFeedback(root, projectID, runID, workflow, skipEdit); err != nil {
			return err
		}
		// Stage exactly what enterTerminal stages, and only what's on
		// disk: a run with no scratch file has nothing to commit.
		// lore/ goes in as a dir so every newly-promoted slug rides along
		// without enumerating them.
		var paths []string
		for _, rel := range []string{followupsRel, loreRel, twinRel, lore.DirRel} {
			if _, statErr := os.Stat(filepath.Join(root, rel)); statErr == nil {
				paths = append(paths, rel)
			}
		}
		if len(paths) == 0 {
			return nil
		}
		// A clean re-run (all `- [x]`) leaves every file byte-identical,
		// so there's nothing staged: swallow ErrNothingToCommit and report
		// the no-op as success. The harvested ideas' own open commits
		// (written by createIdea inside the pipeline) are already landed.
		if err := run.StageAndCommit(root, msg, paths...); err != nil && !errors.Is(err, run.ErrNothingToCommit) {
			return err
		}
		return nil
	})
}
