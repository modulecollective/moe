package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// newRunCommand returns a Command suitable for registering under a
// workflow as its `new` entry point (e.g., `moe sdlc new`,
// `moe kb new`). The workflow name is baked into the closure
// so each facade is a thin wrapper — all the real work (slug
// derivation, collision suffixing, git commit, next-stage hint) lives
// in runNew.
func newRunCommand(workflowName string) *Command {
	return &Command{
		Name:    "new",
		Summary: "open a new run in this workflow",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runNew(workflowName, args, stdout, stderr)
		},
	}
}

// runNew is the shared creator behind every workflow's `new` facade.
// It parses --id and --from-idea, loads the bureaucracy root, writes
// the run, commits it, and prints the first stage's invocation so the
// operator can move straight into work. The workflow is baked in via
// the caller — there is no --workflow flag here, because the workflow
// is implicit in which workflow the operator typed (`moe sdlc new`
// vs `moe kb new`).
//
// --from-idea=<slug> promotes an idea run into a fresh run in the
// target workflow: the idea's title and canvas seed the new run's
// first-stage doc, and the idea run's status is bumped to
// StatusPromoted with a MoE-Promoted-To trailer so the original is
// still greppable and the dash can tell "handed off" from "dropped".
// The new run carries a reciprocal MoE-Idea trailer on its open
// commit. Two commits total — not one, since the status bump lives on
// its own.
func runNew(workflowName string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflowName+" new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	fromIdea := fs.String("from-idea", "", "promote an open idea run (by slug) into a new run, seeding the first-stage doc from its canvas")
	// --one-shot is sdlc-only by design: the kb / quick / idea workflows
	// don't have a design→code chain to drive headlessly. The flag
	// parses on every workflow's `new` (one shared facade) but we
	// reject it for non-sdlc workflows below before doing any work.
	oneShot := fs.Bool("one-shot", false, "(sdlc only) after opening, drive design then code headlessly via `claude -p`; skip the next-stage prompt")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--id <slug>] [--from-idea <slug>] [--one-shot] <project> [\"title\"]\n", workflowName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	// Title is required normally, optional when --from-idea is set
	// (the idea's title supplies it).
	if fs.NArg() < 1 || (fs.NArg() < 2 && *fromIdea == "") {
		fs.Usage()
		return 2
	}
	if *oneShot && workflowName != "sdlc" {
		moePrintf(stderr, "--one-shot: only sdlc supports headless stage chaining\n")
		return 2
	}
	project := fs.Arg(0)
	title := strings.Join(fs.Args()[1:], " ")

	// Sanity check against the workflow registry. The facade's caller
	// supplies a compile-time constant, so this should never fail in
	// practice — but if a workflow's `new` slot gets wired up before
	// the workflow itself is registered (e.g., init-order bug), we
	// catch it before writing state to disk.
	wf, err := LookupWorkflow(workflowName)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
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

	opts := run.Options{
		ID:       *idOverride,
		Workflow: workflowName,
	}

	// Keep a handle on the source idea run so we can bump its status
	// *after* the new run opens. Doing it in the other order would
	// mean a failure mid-flight leaves the idea marked promoted with
	// no destination.
	var sourceIdea *run.Metadata
	if *fromIdea != "" {
		if workflowName == ideaWorkflow {
			moePrintf(stderr, "--from-idea: cannot promote an idea into another idea run\n")
			return 1
		}
		src, seed, err := loadIdeaForPromote(root, project, *fromIdea)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if title == "" {
			title = src.Title
		}
		stages := wf.Stages()
		if len(stages) == 0 {
			moePrintf(stderr, "workflow %q has no stages to seed from --from-idea\n", workflowName)
			return 1
		}
		opts.SeedDocs = map[string]string{stages[0]: seed}
		opts.SubjectFrom = "idea " + *fromIdea
		opts.ExtraTrailers = []string{"MoE-Idea: " + *fromIdea}
		// Anchor the run slug to the idea's filename, not its (editable)
		// H1. run.New will date-suffix on collision.
		opts.IDBase = *fromIdea
		sourceIdea = src
	}

	// Run-identifier for the lock record is advisory — use the
	// project plus whatever slug we have so far (may be blank if the
	// caller didn't pass --id; run.New will derive one inside the lock).
	runRef := project
	if *idOverride != "" {
		runRef = project + "/" + *idOverride
	}
	var md *run.Metadata
	err = withRepoLock(root, repolock.Options{
		Purpose: "run-new",
		Run:     runRef,
	}, func() error {
		m, err := run.New(root, project, title, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)

	if sourceIdea != nil {
		if err := markIdeaPromoted(root, sourceIdea, md); err != nil {
			moePrintf(stderr, "warning: could not mark idea %s/%s promoted: %v\n", sourceIdea.Project, sourceIdea.ID, err)
			// The new run is already open; surface the warning but
			// don't fail the command, since the idea->run transition
			// is still greppable via the new run's MoE-Idea trailer.
		}
	}

	if *oneShot {
		return runOneShotChain(root, md, "design", stdout, stderr)
	}
	// Fresh run — no stage has just finished, so promptNextStage falls
	// back to Next() and offers the workflow's first incomplete stage.
	return promptNextStage(root, md, "", stdout, stderr)
}

// oneShotStages is sdlc's headless ladder: design then code. The list
// is the source of truth for both runOneShotChain (start-from-stage
// dispatch) and any future workflow that earns --one-shot.
var oneShotStages = []struct {
	name         string
	needsSandbox bool
}{
	{name: "design", needsSandbox: false},
	{name: "code", needsSandbox: true},
}

// runOneShotChain drives sdlc's headless ladder starting from the named
// stage. `moe sdlc new --one-shot` passes "design" so design then code
// run in sequence; `moe sdlc resume --one-shot` passes whichever of
// design/code is still pending so an opened run can pick up where it
// left off. A startStage that doesn't appear in the ladder (e.g.
// "push") skips both stages and hands straight to promptNextStage.
//
// Chains only on a zero exit from the prior stage — commitTurn's
// canvas-existence assertion already refuses an empty turn, so a silent
// refusal stops the chain without needing a second sentinel. After the
// last stage lands, hand off to promptNextStage so an interactive
// operator gets the same `[N/m/p]` ship prompt; non-tty callers fall
// through to its `next: …` hint. Automatic pushing is still off the
// table — the prompt's default-N keeps a reflex Enter from shipping.
func runOneShotChain(root string, md *run.Metadata, startStage string, stdout, stderr io.Writer) int {
	var pending []string
	skipping := startStage != ""
	type step struct {
		name         string
		needsSandbox bool
	}
	var toRun []step
	for _, s := range oneShotStages {
		if skipping {
			if s.name == startStage {
				skipping = false
			} else {
				continue
			}
		}
		toRun = append(toRun, step{name: s.name, needsSandbox: s.needsSandbox})
		pending = append(pending, s.name)
	}
	if len(toRun) > 0 {
		moePrintf(stdout, "one-shot: %s (headless)\n", strings.Join(pending, " → "))
	}
	for _, s := range toRun {
		if code := runOneShotStage(md.Project, md.ID, s.name, s.needsSandbox, stdout, stderr); code != 0 {
			moePrintf(stderr, "one-shot: %s stage exited %d; not chaining further\n", s.name, code)
			return code
		}
	}
	// Hand off using the last stage we ran as justFinished so the chain
	// prompt asks about its successor (push), not whatever Next()
	// reports — under the forward-walking rule Next() parks at the
	// just-finished stage. When toRun was empty (resume from a state
	// where neither one-shot stage applies), we have no anchor; fall
	// back to Next() so the merge-gate hint still surfaces.
	justFinished := ""
	if len(toRun) > 0 {
		justFinished = toRun[len(toRun)-1].name
	}
	return promptNextStage(root, md, justFinished, stdout, stderr)
}

// runOneShotStage runs one stage of the sdlc one-shot chain. It's a
// thin wrapper over runStageSession that flips the headless and
// skip-next-stage knobs; runStageSession seeds the user prompt from
// the run title for any headless caller, so callers don't need to.
func runOneShotStage(projectID, runID, docID string, needsSandbox bool, stdout, stderr io.Writer) int {
	return runStageSession(projectID, runID, docID, stageSessionOpts{
		NeedsSandbox:  needsSandbox,
		Headless:      true,
		SkipNextStage: true,
	}, stdout, stderr)
}

// promoteIdeaToSdlcRun opens a fresh sdlc run seeded by an idea's
// canvas, marks the idea promoted, and returns the new run's metadata.
// Mirrors the --from-idea path inside runNew without the --id /
// title-override / one-shot-chain plumbing — the caller (queue add)
// has no use for those today, and keeping this helper narrow makes the
// shared promote semantics easy to reason about.
func promoteIdeaToSdlcRun(root, projectID, ideaSlug string) (*run.Metadata, error) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		return nil, err
	}
	src, seed, err := loadIdeaForPromote(root, projectID, ideaSlug)
	if err != nil {
		return nil, err
	}
	stages := wf.Stages()
	if len(stages) == 0 {
		return nil, fmt.Errorf("workflow sdlc has no stages to seed from --from-idea")
	}
	opts := run.Options{
		Workflow:      "sdlc",
		SeedDocs:      map[string]string{stages[0]: seed},
		SubjectFrom:   "idea " + ideaSlug,
		ExtraTrailers: []string{"MoE-Idea: " + ideaSlug},
		// Anchor the run slug to the idea's filename, not its (editable)
		// H1. run.New will date-suffix on collision.
		IDBase: ideaSlug,
	}
	var md *run.Metadata
	err = withRepoLock(root, repolock.Options{
		Purpose: "run-new",
		Run:     projectID,
	}, func() error {
		m, err := run.New(root, projectID, src.Title, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := markIdeaPromoted(root, src, md); err != nil {
		// New run already opened. Surface the warning via the returned
		// metadata + error pair so callers can decide whether to abort
		// or continue (queue add chooses to continue: the run is open
		// and queueable, the idea is just not yet flipped to promoted).
		return md, fmt.Errorf("warning: could not mark idea %s/%s promoted: %w", src.Project, src.ID, err)
	}
	return md, nil
}

// loadIdeaForPromote returns the source idea run and its canvas body
// to seed the next workflow's first-stage doc with. The canvas is the
// full file — H1 included — so the agent that opens the first stage
// starts on a canvas that already names what it's about. Errors when
// the slug doesn't name an idea run (wrong workflow or missing).
func loadIdeaForPromote(root, projectID, slug string) (*run.Metadata, string, error) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("--from-idea: run %s/%s does not exist", projectID, slug)
		}
		return nil, "", fmt.Errorf("--from-idea: %w", err)
	}
	if md.Workflow != ideaWorkflow {
		return nil, "", fmt.Errorf("--from-idea: run %s/%s is a %s run, not an idea", projectID, slug, md.Workflow)
	}
	if md.Status != run.StatusInProgress {
		return nil, "", fmt.Errorf("--from-idea: idea %s/%s is already %s", projectID, slug, md.Status)
	}
	canvasRel := run.ContentPath(projectID, slug, ideaDocID)
	b, err := os.ReadFile(filepath.Join(root, canvasRel))
	if err != nil {
		return nil, "", fmt.Errorf("--from-idea: read %s: %w", canvasRel, err)
	}
	return md, string(b), nil
}

// markIdeaPromoted bumps the source idea run's status to
// StatusPromoted and commits the transition with a MoE-Promoted-To
// trailer pointing at the new run. Separate commit from the new run's
// open: two short commits keep the git history honest (one event per
// commit) and dodges the RemovePaths hack that used to inline the
// idea-file delete into the open commit.
func markIdeaPromoted(root string, md *run.Metadata, dest *run.Metadata) error {
	md.Status = run.StatusPromoted
	runJSONRel := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf(`Promote idea %s/%s → %s/%s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Promoted-To: %s/%s
`, md.Project, md.ID, dest.Project, dest.ID, md.ID, md.Project, ideaWorkflow, dest.Project, dest.ID)
	return withRepoLock(root, repolock.Options{
		Purpose: "idea-promote",
		Run:     md.Project + "/" + md.ID,
	}, func() error {
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
}
