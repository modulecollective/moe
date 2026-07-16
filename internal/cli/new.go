package cli

import (
	"flag"
	"io"
	"os"

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
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--workspace <name>] [--agent <name>] <project>/<slug>\n", workflowName)
		moePrintf(stderr, "       moe %s new [--workspace <name>] [--agent <name>] --from-idea <project>/<slug>\n", workflowName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
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
		m, err := runopen.Open(root, project, run.Options{
			ID:        slug,
			Workflow:  workflowName,
			Workspace: *workspaceName,
			Agent:     *agentOverride,
		}, stdout, stderr)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		md = m
		moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)
	}

	// Fresh run — no stage has just finished, so promptNextStage falls
	// back to Next() and offers the workflow's first incomplete stage.
	// Headless cascade is no longer a `new` flag: the operator picks
	// `!<stage>`, `!!` (ship this run), or `!!!` (ship + ride the chain)
	// at the chain prompt after seeing the seeded canvas. Scripted
	// automation that wants fire-and-forget can pipe the answer in
	// (`echo '!!!' | moe sdlc new ...`).
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
