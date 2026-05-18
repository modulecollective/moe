package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/workspace"
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
	// --workspace means two things across workflows: sdlc binds the run
	// to the named workspace as its working tree (claim taken on first
	// attach); hooks records it as a no-claim label so the operator can
	// see "this hooks run iterates against <name>" on the dash. The flag
	// parses on every workflow's shared `new` facade and we reject it
	// for the other workflows below before doing any work.
	workspaceName := fs.String("workspace", "", "(sdlc, hooks) bind the run to the named workspace at .moe/named/<project>/<name>/ — sdlc uses it as the run's working tree (claim taken); hooks records it as a no-claim label")
	agentOverride := fs.String("agent", "", "agent backend for this run (claude/codex). Explicit values persist to run.json; omitted values resolve at stage time via $MOE_AGENT, then claude")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--id <slug>] [--from-idea <slug>] [--workspace <name>] [--agent <name>] <project> [\"title\"]\n", workflowName)
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
	if *workspaceName != "" {
		if workflowName != "sdlc" && workflowName != hooksWorkflow {
			moePrintf(stderr, "--workspace: only sdlc and hooks accept --workspace today\n")
			return 2
		}
		if err := workspace.ValidateName(*workspaceName); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	// Validate --agent against the registry up-front so a typo surfaces
	// at run open rather than at first stage turn. Empty (the steady
	// state) skips validation — resolveAgentName fills it in later.
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
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
		ID:        *idOverride,
		Workflow:  workflowName,
		Workspace: *workspaceName,
		Agent:     *agentOverride,
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
		opts.Trailers = trailers.Block{Idea: *fromIdea}
		// Anchor the run slug to the idea's filename, not its (editable)
		// H1. run.New will date-suffix on collision.
		opts.IDBase = *fromIdea
		sourceIdea = src
	}

	if *workspaceName != "" && workflowName == "sdlc" {
		// Pre-flight: refuse to open a run against a workspace that is
		// already claimed. The actual claim is taken at first attach
		// (sdlc code), but checking here gives the operator a fail-fast
		// signal at the verb they actually typed instead of a confusing
		// error several commands later. Stale claims (a run that crashed
		// or was force-closed) can be cleared with `rm`; we don't paper
		// over them automatically.
		//
		// Hooks runs don't take a claim — the workspace is a label, not
		// a working tree — so this gate is sdlc-only.
		holder, herr := workspace.ReadClaim(root, project, *workspaceName)
		if herr != nil {
			moePrintf(stderr, "%v\n", herr)
			return 1
		}
		if holder != nil {
			moePrintf(stderr,
				"workspace %q for project %q is claimed by run %s; close that run first or pick a different workspace\n",
				*workspaceName, project, holder.Run)
			return 1
		}
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
	moePrintf(stdout, "opened run %s %s\n", md.Project, md.ID)

	if sourceIdea != nil {
		if err := markIdeaPromoted(root, sourceIdea, md); err != nil {
			moePrintf(stderr, "warning: could not mark idea %s %s promoted: %v\n", sourceIdea.Project, sourceIdea.ID, err)
			// The new run is already open; surface the warning but
			// don't fail the command, since the idea->run transition
			// is still greppable via the new run's MoE-Idea trailer.
		}
	}

	// Fresh run — no stage has just finished, so promptNextStage falls
	// back to Next() and offers the workflow's first incomplete stage.
	// Headless cascade is no longer a `new` flag: the operator picks
	// `!<stage>` or `!!` at the chain prompt after seeing the seeded
	// canvas. Scripted automation that wants fire-and-forget can pipe
	// the answer in (`echo '!!' | moe sdlc new ...`).
	return promptNextStage(root, md, "", stdout, stderr)
}

// promoteIdeaToSdlcRun opens a fresh sdlc run seeded by an idea's
// canvas, marks the idea promoted, and returns the new run's metadata.
// Mirrors the --from-idea path inside runNew without the --id /
// title-override / one-shot-chain plumbing — keeping this helper
// narrow makes the shared promote semantics easy to reason about.
//
// agentName, when non-empty, is stamped onto the new run's
// run.json.Agent. Empty leaves the field unset and the usual
// $MOE_AGENT → "claude" precedence ladder runs at first stage turn.
func promoteIdeaToSdlcRun(root, projectID, ideaSlug, agentName string) (*run.Metadata, error) {
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
		Workflow:    "sdlc",
		SeedDocs:    map[string]string{stages[0]: seed},
		SubjectFrom: "idea " + ideaSlug,
		Trailers:    trailers.Block{Idea: ideaSlug},
		Agent:       agentName,
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
		// or continue.
		return md, fmt.Errorf("warning: could not mark idea %s %s promoted: %w", src.Project, src.ID, err)
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
		if errors.Is(err, run.ErrRunNotFound) {
			return nil, "", fmt.Errorf("--from-idea: run %s %s does not exist", projectID, slug)
		}
		return nil, "", fmt.Errorf("--from-idea: %w", err)
	}
	if md.Workflow != ideaWorkflow {
		return nil, "", fmt.Errorf("--from-idea: run %s %s is a %s run, not an idea", projectID, slug, md.Workflow)
	}
	if md.Status != run.StatusInProgress {
		return nil, "", fmt.Errorf("--from-idea: idea %s %s is already %s", projectID, slug, md.Status)
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
	msg := fmt.Sprintf("Promote idea %s %s → %s %s\n\n", md.Project, md.ID, dest.Project, dest.ID) +
		trailers.Block{
			Run:        md.ID,
			Project:    md.Project,
			Workflow:   ideaWorkflow,
			PromotedTo: dest.Project + "/" + dest.ID,
		}.String()
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
