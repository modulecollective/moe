package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
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
// It parses --from-idea, loads the bureaucracy root, writes the run,
// commits it, and prints the first stage's invocation so the operator
// can move straight into work. The workflow is baked in via the caller
// — there is no --workflow flag here, because the workflow is implicit
// in which workflow the operator typed (`moe sdlc new` vs `moe kb new`).
//
// Positional shape:
//   - Normal:        `<project>/<slug>` — operator-typed slug, collisions
//     fail loud with a free-suggestion in the error.
//   - --from-idea:   no positional      — the run reference is on the
//     flag value (`--from-idea=<project>/<slug>`); the new run's slug
//     is derived from the idea's filename via IDBase, date-suffixed on
//     collision so two promotes of the same idea read as "same topic,
//     opened on date X".
//
// --from-idea=<project>/<slug> promotes an idea run into a fresh run
// in the target workflow: the idea's canvas seeds the new run's
// first-stage doc, and the idea run's status is bumped to
// StatusPromoted with a MoE-Promoted-To trailer so the original is
// still greppable and the dash can tell "handed off" from "dropped".
// The new run carries a reciprocal MoE-Idea trailer on its open
// commit. Two commits total — not one, since the status bump lives
// on its own.
func runNew(workflowName string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflowName+" new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromIdea := fs.String("from-idea", "", "promote an open idea run (by `<project>/<slug>`) into a new run, seeding the first-stage doc from its canvas")
	// --workspace means two things across workflows: sdlc binds the run
	// to the named workspace as its working tree (claim taken at first
	// stage attach — sdlc design under the sdlc workflow); hooks records
	// it as a no-claim label so the operator can see "this hooks run
	// iterates against <name>" on the dash. The flag parses on every
	// workflow's shared `new` facade and we reject it for the other
	// workflows below before doing any work.
	workspaceName := fs.String("workspace", "", "(sdlc, hooks) bind the run to the named workspace at .moe/named/<project>/<name>/ — sdlc uses it as the run's working tree (claim taken at first stage attach); hooks records it as a no-claim label")
	agentOverride := fs.String("agent", "", "agent backend for this run (claude/codex). Explicit values persist to run.json; omitted values resolve at stage time via the model stylesheet, then $MOE_AGENT, then claude")
	// --park, --seed, and --ship are workflow-generic (they live on the
	// shared new facade, so sdlc/kb/hooks all get them). --park opens the
	// run and prints the next-stage hint instead of prompting to ride the
	// chain; --seed pops $EDITOR and opens the run with the edited body as
	// its first-stage seed; --ship opens the run and cascades every stage
	// headless to the ship, the flag twin of typing `!!` at the chain
	// prompt. --seed composes with either tail; --park and --ship are
	// opposite tails (mutually exclusive); --seed and --from-idea are
	// mutually exclusive (both claim the first-stage seed).
	park := fs.Bool("park", false, "open the run and stop: print the next-stage hint instead of prompting to run it")
	seed := fs.Bool("seed", false, "pop $EDITOR on a stub and open the run with the edited body as its first-stage seed (mutually exclusive with --from-idea)")
	ship := fs.Bool("ship", false, "open the run and cascade every stage headless to the ship (the flag twin of `!!` at the chain prompt; mutually exclusive with --park)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--workspace <name>] [--agent <name>] [--seed] [--park|--ship] <project>/<slug>\n", workflowName)
		moePrintf(stderr, "       moe %s new [--workspace <name>] [--agent <name>] [--park|--ship] --from-idea <project>/<slug>\n", workflowName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if *seed && *fromIdea != "" {
		moePrintf(stderr, "%s new: --seed and --from-idea are mutually exclusive (both seed the first stage)\n", workflowName)
		return 2
	}
	if *ship && *park {
		moePrintf(stderr, "%s new: --ship and --park are opposite tails (one cascades to the ship, the other stops) — pick one\n", workflowName)
		return 2
	}
	// Preflight the cascade dispatcher before the open commit: --ship
	// hands off to the same cascade path the `!!` chain answer takes, and
	// a workflow with no registered dispatcher can't cascade. Every
	// workflow ships one today, but the `new` facade is generic — refuse
	// at flag-parse time rather than minting a run we then can't ship.
	if *ship && lookupCascadeDispatcher(workflowName) == nil {
		moePrintf(stderr, "%s new: --ship: workflow %q has no cascade — open without --ship and drive the stages yourself\n", workflowName, workflowName)
		return 2
	}
	if *fromIdea != "" {
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
	} else if fs.NArg() != 1 {
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

	// Single identifier shape across modes: `<project>/<slug>`. With
	// --from-idea the slug names the source idea (the new run's own
	// slug is derived via IDBase below); without it, the slug is the
	// operator-typed name of the new run.
	var project, slug string
	if *fromIdea != "" {
		p, s, err := splitProjectRun(*fromIdea)
		if err != nil {
			moePrintf(stderr, "%s new: --from-idea: %v\n", workflowName, err)
			return 2
		}
		project = p
		// Rewrite the flag value to the bare idea slug — runopen.Promote
		// wants the bare slug (it composes the SubjectFrom, MoE-Idea
		// trailer, and IDBase from it).
		*fromIdea = s
	} else {
		p, s, err := splitProjectRun(fs.Arg(0))
		if err != nil {
			moePrintf(stderr, "%s new: %v\n", workflowName, err)
			return 2
		}
		if canonical := run.Slugify(s); canonical != s {
			moePrintf(stderr, "%s new: slug must match [a-z0-9-]+ (lowercase kebab), got %q\n", workflowName, s)
			return 2
		}
		project, slug = p, s
	}

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

	if *workspaceName != "" && workflowName == "sdlc" {
		if code := preflightWorkspaceClaim(root, project, *workspaceName, stderr); code != 0 {
			return code
		}
	}

	var md *run.Metadata
	if *fromIdea != "" {
		stages := wf.Stages()
		if len(stages) == 0 {
			moePrintf(stderr, "workflow %q has no stages to seed from --from-idea\n", workflowName)
			return 1
		}
		promoted, err := runopen.Promote(root, project, *fromIdea, runopen.PromoteOptions{
			Workflow:   workflowName,
			FirstStage: stages[0],
			Workspace:  *workspaceName,
			Agent:      *agentOverride,
		}, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		md = promoted.Run
		moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)
		if promoted.MarkErr != nil {
			moePrintf(stderr, "warning: could not mark idea %s/%s promoted: %v\n", project, *fromIdea, promoted.MarkErr)
			// The new run is already open; surface the warning but
			// don't fail the command, since the idea->run transition
			// is still greppable via the new run's MoE-Idea trailer.
		}
	} else {
		openOpts := run.Options{
			ID:        slug,
			Workflow:  workflowName,
			Workspace: *workspaceName,
			Agent:     *agentOverride,
		}
		// seedTmpPath is non-empty once --seed's editor capture has left a
		// tempfile worth preserving; keepSeedTmp defers its cleanup so a
		// late runopen.Open failure keeps the operator's typed seed.
		var seedTmpPath string
		keepSeedTmp := false
		if *seed {
			code := seedFirstStage(root, wf, project, slug, &openOpts, &seedTmpPath, stdout, stderr)
			if code != 0 {
				return code
			}
			defer func() {
				if !keepSeedTmp {
					os.RemoveAll(filepath.Dir(seedTmpPath))
				}
			}()
		}
		m, err := runopen.Open(root, project, openOpts, stdout, stderr)
		if err != nil {
			if seedTmpPath != "" {
				keepSeedTmp = true
				moePrintf(stderr, "%s new: %v\n", workflowName, err)
				moePrintf(stderr, "%s new: your edited seed is preserved at %s\n", workflowName, seedTmpPath)
				return 1
			}
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		md = m
		moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)
	}

	// --park: open the run and stop. Print the next-stage hint (the same
	// line the non-TTY path emits) and exit without the chain prompt —
	// the "just open it" flow that costs an `n` + Enter otherwise.
	if *park {
		return promptNextStageParked(root, md, stdout, stderr)
	}

	// --ship: open the run and cascade to the ship without a stop at the
	// chain prompt. Resolve the first incomplete stage the same way
	// promptNextStage's default branch does (Next() on a fresh run reports
	// the workflow's first stage), then hand off to the existing cascade
	// with the literal `!!` answer — ship this run, stop. No new cascade
	// code: the summary line, failure exit codes, and blocked-gate
	// behaviour come along for free, exactly as if the operator had typed
	// `!!` at the prompt. `!!` not `!!!`: a freshly minted run has no
	// chained children, so the ride variant is a no-op here.
	if *ship {
		stage, kind, err := wf.Next(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if kind != NextKindStage || stage == "" {
			// A fresh run always has a first incomplete stage; this guard
			// mirrors promptNextStage's default branch for the impossible
			// case rather than feeding an empty start to the cascade.
			return 0
		}
		return dispatchCascade("!!", stage, root, md, stdout, stderr)
	}

	// Fresh run — no stage has just finished, so promptNextStage falls
	// back to Next() and offers the workflow's first incomplete stage. The
	// operator picks `!<stage>`, `!!` (ship this run), or `!!!` (ship +
	// ride the chain) at the chain prompt after seeing the seeded canvas;
	// --ship above is the decided-up-front twin of `!!` for the tee-up
	// flow that doesn't want to sit at the prompt.
	return promptNextStage(root, md, "", stdout, stderr)
}

// preflightWorkspaceClaim refuses to bind a run to a workspace that is
// already claimed by another run. The actual claim is taken at first
// stage attach (sdlc design under the sdlc workflow); checking at the
// verb gives the operator a fail-fast signal at the command they typed
// instead of a confusing error one stage later. Stale claims (a run
// that crashed or was force-closed) clear with `moe workspace release`;
// we don't paper over them automatically.
//
// Hooks runs don't take a claim — the workspace is a label, not a
// working tree — so callers gate this on workflow themselves.
//
// Returns 0 to proceed; non-zero exit code (already printed to stderr)
// otherwise.
func preflightWorkspaceClaim(root, projectID, name string, stderr io.Writer) int {
	holder, err := workspace.ReadClaim(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if holder != nil {
		moePrintf(stderr,
			"workspace %q for project %q is claimed by run %s; close that run first or pick a different workspace\n",
			name, projectID, holder.Run)
		return 1
	}
	return 0
}

// seedFirstStage runs --seed's editor capture and, on success, wires the
// edited body into opts.SeedDocs under the workflow's first stage — the
// same SeedDocs mechanism `--from-idea`'s promote uses. It gates on an
// editor being configured, pre-flights the slug so a collision fails
// before the editor pops (not after the operator types into a tempfile
// we'd throw away), pops $EDITOR on a `# slug` stub, and refuses to mint
// anything when the operator leaves the stub unchanged.
//
// *tmpPath is set to the capture tempfile so the caller can preserve it
// (and name its path) if the later open commit fails — the multi-minute
// editor window makes the typed seed the recoverable asset. On every
// failure seedFirstStage resolves within itself (prints the cause;
// preserves the tempfile on an editor/read failure, cleans it up on an
// unchanged-stub abort) and returns a nonzero exit code; the caller just
// bubbles it up.
//
// The unchanged-stub abort deliberately diverges from `idea new`, which
// happily captures a bare heading: an idea is cheap to capture and cheap
// to close, but an accidental sdlc run is a dashboard entry that needs an
// explicit close, so a no-op edit should mint nothing.
func seedFirstStage(root string, wf *Workflow, project, slug string, opts *run.Options, tmpPath *string, stdout, stderr io.Writer) int {
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintf(stderr, "%s new: set $EDITOR or $VISUAL — --seed needs an editor\n", wf.Name)
		return 1
	}
	stages := wf.Stages()
	if len(stages) == 0 {
		moePrintf(stderr, "%s new: workflow %q has no stages to seed\n", wf.Name, wf.Name)
		return 1
	}
	// Pre-flight the slug before the editor pop. run.New re-checks inside
	// the lock and is the authority on collisions; this refuses the
	// obvious case up front. Match run.New's wording so the operator sees
	// the same error regardless of which gate caught it.
	if taken, err := run.SlugTaken(root, project, slug); err != nil {
		moePrintf(stderr, "%s new: %v\n", wf.Name, err)
		return 1
	} else if taken {
		suggestion, serr := run.NextFreeID(root, project, slug)
		if serr != nil {
			moePrintf(stderr, "%s new: %v\n", wf.Name, serr)
			return 1
		}
		moePrintf(stderr,
			"%s new: slug %q in project %s is already used (existing run or prior history); try %q or pick a different name\n",
			wf.Name, slug, project, suggestion)
		return 1
	}
	stub := fmt.Sprintf("# %s\n", slug)
	body, tp, code := captureEditorBody("moe-"+wf.Name+"-seed-", stub, stdout, stderr)
	*tmpPath = tp
	if code != 0 {
		if tp != "" {
			moePrintf(stderr, "%s new: your edited seed is preserved at %s\n", wf.Name, tp)
		}
		return code
	}
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == strings.TrimSpace(stub) {
		os.RemoveAll(filepath.Dir(tp))
		*tmpPath = ""
		moePrintf(stderr, "%s new: aborting: seed unchanged\n", wf.Name)
		return 1
	}
	opts.SeedDocs = map[string]string{stages[0]: body}
	return 0
}
