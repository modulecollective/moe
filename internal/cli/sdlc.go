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
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// The SDLC workflow owns the design→code→push lifecycle. Stages are
// nested under `moe sdlc` so kb (and future workflows) can pick their
// own short stage names without collision. `moe sdlc new` is the entry
// point that creates a run in this workflow.

func init() {
	g := NewCommandGroup("sdlc", "sdlc workflow: new, design, code, test, push")
	g.Register(newRunCommand("sdlc"))
	g.Register(&Command{
		Name:    "design",
		Summary: "open a Claude Code session on the run's design document",
		Run:     runDesign,
	})
	g.Register(&Command{
		Name:    "code",
		Summary: "open a Claude Code session on the run's code document (in a sandbox clone)",
		Run:     runCode,
	})
	g.Register(&Command{
		Name:    "test",
		Summary: "open a Claude Code session on the run's test document — verify the code stage's work",
		Run:     runTest,
	})
	g.Register(pushCommand("sdlc"))
	g.Register(&Command{
		Name:    "shell",
		Summary: "drop into a shell rooted at a run's workspace, or at a named workspace directly",
		Run:     runShell,
	})
	g.Register(closeCommand("sdlc", "Close sdlc run %s %s", releaseWorkspaceCleanup))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (sdlc cat <project> <run> <stage>)",
		Run:     runCat("sdlc", ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (sdlc log <project> <run> <stage>)",
		Run:     runLog("sdlc", ""),
	})
	g.Register(&Command{
		Name:    "reopen",
		Summary: "open a fresh sdlc run seeded with the design canvas of a terminal prior run",
		Run:     runSDLCReopen,
	})
	g.Register(&Command{
		Name:    "resume",
		Summary: "drive any pending stages of an opened run headlessly, then prompt at the merge gate",
		Run:     runResume,
	})
	RegisterGroup(g)

	w := NewWorkflow("sdlc")
	w.RegisterStage("design")
	w.RegisterStage("code", "design")
	w.RegisterStage("test", "code")
	w.RegisterStage("push", "test")
	// Test stage's anti-theater check: the work-turn commit alone
	// doesn't tell us whether the agent actually filled the canvas
	// or just committed the placeholder skeleton. The gate reads the
	// canvas and refuses to advance until "What was verified" and
	// "What wasn't verified" both have substantive content.
	w.RegisterStageGate("test", testStageGate)
	RegisterWorkflow(w)
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc design", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc design [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive agent session on the design canvas.")
		moePrintln(stderr, "First use on a run creates the document; re-runs resume the session.")
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
		moePrintf(stderr, "sdlc design: %v\n", err)
		return 2
	}
	return openSdlcDesign(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

func runCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc code", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc code [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive agent session on the code canvas. The agent")
		moePrintln(stderr, "works inside a private sandbox clone of the project's submodule, isolated")
		moePrintln(stderr, "from other activity until `moe sdlc push` opens a PR.")
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
		moePrintf(stderr, "sdlc code: %v\n", err)
		return 2
	}
	return openSdlcCode(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

func runTest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "override the run's agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc test [--agent <name>] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive agent session on the test canvas. The agent")
		moePrintln(stderr, "verifies the code stage's work — running the project's checks, driving")
		moePrintln(stderr, "the change end-to-end, applying small in-place fixes, and narrating what")
		moePrintln(stderr, "was and wasn't verified on the canvas. Pre-push hooks still gate ship.")
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
		moePrintf(stderr, "sdlc test: %v\n", err)
		return 2
	}
	return openSdlcTest(projectID, runID, false, false, *agentOverride, stdout, stderr)
}

// openSdlcDesign is the Go-level seam behind `moe sdlc design`. The
// typed `Command.Run` parses args and hands to this helper; the chain
// prompt's cascade driver reaches it directly via openSdlcStage. The
// contract is identical either way: resolveSDLCRunSlug guards the run
// (with the promoted/reopened descendant fallback baked in), then
// runStageSession opens an interactive (or headless) session against
// the design canvas. headless=true is the path that used to be
// `--one-shot`; the flag is gone, but the Go function still
// distinguishes the two so internal callers can ask for the bounded
// one-turn variant without re-entering the parser.
func openSdlcDesign(projectID, runID string, headless bool, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc design", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if headless {
		return runStageSession(projectID, runID, "design",
			stageSessionOpts{
				NeedsSandbox:           true,
				EnforceSandboxBoundary: true,
				Headless:               true,
				SkipNextStage:          suppressNextStage,
				Agent:                  agentOverride,
			}, stdout, stderr)
	}
	// The agent produces the user-facing cue itself: the interactive TUI
	// has no way to pre-seed the input box with editable text, so instead
	// of a printed banner (which the TUI would cover on launch) we ask
	// the agent to greet the operator and prompt for input.
	const kickoff = "The operator just opened this design session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"design stands (fresh start vs. resumed) and ask what they'd like to " +
		"work on next. Then wait for their reply."
	return runStageSession(projectID, runID, "design",
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			InitialPrompt:          kickoff,
			SkipNextStage:          suppressNextStage,
			Agent:                  agentOverride,
		}, stdout, stderr)
}

// openSdlcCode is the Go-level seam behind `moe sdlc code`. See
// openSdlcDesign for the broader contract. The extra step here is
// requireDesignCanvas: code can't drive against a design that was
// never opened, on either the interactive or headless path. The
// run-validation step runs *before* the canvas check so a wrong-
// project typo surfaces as "run not found" instead of sending the
// operator off to run a design stage that's also going to fail.
func openSdlcCode(projectID, runID string, headless bool, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc code", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if err := requireDesignCanvas(projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if headless {
		return runStageSession(projectID, runID, "code",
			stageSessionOpts{NeedsSandbox: true, Headless: true, SkipNextStage: suppressNextStage, Agent: agentOverride}, stdout, stderr)
	}
	const kickoff = "The operator just opened this code session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"implementation stands (fresh start vs. resumed) and ask what they'd " +
		"like to work on next. Then wait for their reply."
	return runStageSession(projectID, runID, "code",
		stageSessionOpts{NeedsSandbox: true, InitialPrompt: kickoff, SkipNextStage: suppressNextStage, Agent: agentOverride}, stdout, stderr)
}

// openSdlcTest is the Go-level seam behind `moe sdlc test`. Same
// shape as openSdlcCode one stage downstream — requireCodeCanvas
// stands in for requireDesignCanvas, and the canvas skeleton wires
// in so the agent's first read sees the structural shape it has to
// fill.
func openSdlcTest(projectID, runID string, headless bool, suppressNextStage bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc test", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if err := requireCodeCanvas(projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if headless {
		return runStageSession(projectID, runID, "test",
			stageSessionOpts{
				NeedsSandbox:   true,
				Headless:       true,
				SkipNextStage:  suppressNextStage,
				CanvasSkeleton: testCanvasSkeleton,
				Agent:          agentOverride,
			}, stdout, stderr)
	}
	const kickoff = "The operator just opened this test session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where " +
		"verification stands (fresh start vs. resumed) and ask what they'd " +
		"like to verify or spot-check next. Then wait for their reply."
	return runStageSession(projectID, runID, "test",
		stageSessionOpts{
			NeedsSandbox:   true,
			InitialPrompt:  kickoff,
			SkipNextStage:  suppressNextStage,
			CanvasSkeleton: testCanvasSkeleton,
			Agent:          agentOverride,
		}, stdout, stderr)
}

// openSdlcStage routes the chain prompt's cascade driver
// (`!` / `!<stage>` / `!!`) and the cascade's pre-push iteration to
// the right per-stage helper, headless, carrying suppressNextStage
// through to stageSessionOpts.SkipNextStage. Knowing the stage names
// statically (sdlc has three headlessable stages — push is not one
// of them) is what lets a
// switch beat a registry: the alternative is a typed-CLI re-entry
// via `cmd.Run` with a flag prepended, which is the pattern the run
// that removed `--one-shot` set out to retire.
//
// push deliberately has no case here. The cascade's yolo branch ships
// via pushCmd.Run with no flags, and runPushTyped owns the shared
// synthesis preflight before either ship path. An unexpected
// stage="push" call surfaces as the default branch's error rather
// than silently routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain promptStageNextStage → openSdlcStage → openSdlcDesign →
// runStageSession (a var whose initializer reaches back through
// promptNextStage) stays clear of Go's package init-order cycle
// checker. Closing the loop with a direct func declaration tipped
// it into an init-cycle error; the var has no initializer
// expression for the checker to follow.
var openSdlcStage func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openSdlcStage = func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		// Chain / cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession.
		switch stage {
		case "design":
			return openSdlcDesign(projectID, runID, true, suppressNextStage, "", stdout, stderr)
		case "code":
			return openSdlcCode(projectID, runID, true, suppressNextStage, "", stdout, stderr)
		case "test":
			return openSdlcTest(projectID, runID, true, suppressNextStage, "", stdout, stderr)
		default:
			moePrintf(stderr, "sdlc: openSdlcStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerHeadlessDispatcher("sdlc", func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openSdlcStage(stage, projectID, runID, suppressNextStage, stdout, stderr)
	})
}

// testCanvasSkeleton is the fixed structural shape every test canvas
// opens with. The Next.satisfied check (see workflow.go) enforces
// non-empty "What was verified" and "What wasn't verified" sections;
// the stage fragment instructs the agent on the anti-theater rules.
const testCanvasSkeleton = `# Test

## What was verified

(agent fills: commands run, end-to-end paths driven, what passed — cite and quote)

## What wasn't verified

(agent fills: skipped surfaces + why — needs human eye, needs prod-shaped data, out of scope. "Nothing — automated tests cover the change" is acceptable for pure-backend work.)

## Fixes applied during this stage

(agent fills: one row per in-place fix; empty if none)

## Operator spot-check

(optional; the operator may fill if they drove the change manually)
`

// runResume drives an already-opened sdlc run forward through whichever
// of design/code/test is still pending and hands off to the next chain
// prompt. Useful as a first-class operator verb: pick up an opened run
// and ride it to the next gate without typing two stage commands.
//
// Always interactive: invokes the next pending stage interactively;
// the stage's existing chain prompt (`[Y/n…]` / `[N/m/p…]`) walks
// the rest. Headless cascade is no longer a `resume` flag — the
// operator types `!<stage>` or `!!` at the chain prompt once they've
// seen the canvas, the same vocabulary every other headless decision
// uses.
//
// Refuses missing or terminal runs at the boundary so a resume call
// against a dead run fails fast instead of spawning a session.
func runResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sdlc resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe sdlc resume <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Picks up the run at its first pending stage and opens it")
		moePrintln(stderr, "interactively. The stage's post-turn chain prompt drives the rest:")
		moePrintln(stderr, "`!` runs the next stage headless, `!<stage>` cascades to a named gate,")
		moePrintln(stderr, "`!!` cascades and ships. Refuses runs that are missing or already")
		moePrintln(stderr, "terminal.")
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
		moePrintf(stderr, "sdlc resume: %v\n", err)
		return 2
	}

	resolved, code := resolveSDLCRunSlug("sdlc resume", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "sdlc resume: %v\n", err)
		return 1
	}
	if md.Workflow != "sdlc" {
		moePrintf(stderr, "sdlc resume: %s/%s is a %s run, not sdlc\n", projectID, runID, md.Workflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "sdlc resume: %s/%s is %s; nothing to resume\n", projectID, runID, md.Status)
		return 1
	case run.StatusPushed:
		moePrintf(stderr, "sdlc resume: %s/%s already pushed; resume cannot drive a pushed run\n", projectID, runID)
		return 1
	}

	// Decide where to start. Workflow.Next returns the parked stage
	// for any in_progress sdlc run — design, code, test, or push —
	// under the forward-walking satisfaction rule. NextKindDone is
	// reserved for terminal statuses and runs whose workflow has no
	// stages, neither of which can reach this point (resume refuses
	// terminal above; sdlc has four stages).
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	nextStage, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Interactive mode: invoke the next stage interactively. Its
	// post-stage chain prompt drives the rest — same behaviour the
	// operator gets today after a stage exits.
	if kind != NextKindStage || nextStage == "" {
		// Defensive: under the forward-walking satisfaction rule,
		// Next() returns the parked stage rather than NextKindDone for
		// any non-terminal in_progress run, and resume already refuses
		// terminal statuses above. In practice this branch isn't
		// reachable from a healthy run today; kept as a no-op safety
		// net so an unforeseen edge case (a bare-metadata run with no
		// turns at all and a workflow with no stages) doesn't panic.
		return promptNextStage(root, md, "", stdout, stderr)
	}
	g, err := LookupGroup(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	cmd := g.Lookup(nextStage)
	if cmd == nil {
		moePrintf(stderr, "sdlc resume: workflow %s has no command for stage %q\n", md.Workflow, nextStage)
		return 1
	}
	return cmd.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
}

// requireRun fails the stage entry point fast when the run doesn't
// exist, before any per-turn worktree is materialised. Without this
// check, a wrong-project typo produces an empty worktree per attempt
// plus a confusing downstream error (a missing design canvas, or a
// raw filesystem read error from inside the worktree). Returns the
// process exit code: 0 means proceed, non-zero means the caller
// already wrote the error and should bail.
func requireRun(verb, projectID, runID string, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if _, err := run.Load(root, projectID, runID); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "%s: run not found: %s/%s\n", verb, projectID, runID)
			return 1
		}
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	return 0
}

// requireDesignCanvas refuses the code stage when the run's design
// canvas is missing or empty. The fail-loud invariant the design twin
// records on the commit side carries into the read side: code can't
// drive against a design that was never opened. Applies to both
// interactive and headless paths so an operator skipping straight to
// `sdlc code` on a fresh run gets the same error either way.
func requireDesignCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "design", "code")
}

// requireCodeCanvas is the analogue for test stage: refuse to open a
// test session when there's no code canvas to verify. Same fail-loud
// invariant as requireDesignCanvas, one stage downstream.
func requireCodeCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "code", "test")
}

// requirePriorCanvas is the shared shape behind requireDesignCanvas and
// requireCodeCanvas: read the prior stage's canvas and bail with a
// pointer at the verb the operator needs to run first.
//
// Two failure modes, both fatal at this gate:
//
//  1. The canvas is missing or empty on disk — the prior stage was
//     never opened. Same shape today's check covers; kept as a cheap
//     early-out before reaching for git.
//  2. The canvas at HEAD is byte-identical to the kickoff commit's
//     blob — the prior stage was opened but the agent never wrote
//     to the canvas (or someone reverted it back to the seed). This
//     is the cascade footgun the design twin records: a `!!` after
//     a no-op session would otherwise dispatch the next stage
//     against an unchanged stub.
//
// Defense in depth: session.Close has its own gate that refuses to
// fast-forward an unchanged canvas, but operators can also commit
// directly via `git commit` outside sessions, so the read-side gate
// has to stand on its own.
func requirePriorCanvas(projectID, runID, priorStage, currentStage string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return err
	}
	canvasRel := run.ContentPath(projectID, runID, priorStage)
	canvas := filepath.Join(root, canvasRel)
	info, err := os.Stat(canvas)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("%s canvas missing — run `moe sdlc %s %s/%s` before `moe sdlc %s`",
			priorStage, priorStage, projectID, runID, currentStage)
	}
	// Compare the blob at HEAD to the blob at the canvas's kickoff
	// commit. The check only fires when the kickoff was an `Open
	// run` — i.e. run.New seeded the canvas via SeedDocs. When the
	// canvas's first commit is a work turn (no SeedDocs path), the
	// "first content was an agent edit" case isn't a meaningful
	// failure — there's no stub to be unchanged from.
	kickoffSHA, kickoffSubject, err := canvasKickoffCommit(root, canvasRel)
	if err != nil || kickoffSHA == "" {
		return nil
	}
	if !strings.HasPrefix(kickoffSubject, "Open run ") {
		return nil
	}
	headBlob, headErr := git.RevParse(root, "HEAD:"+canvasRel)
	kickoffBlob, kickoffBlobErr := git.RevParse(root, kickoffSHA+":"+canvasRel)
	if headErr != nil || kickoffBlobErr != nil {
		return nil
	}
	if headBlob == kickoffBlob {
		return fmt.Errorf("%s canvas unchanged from kickoff — run `moe sdlc %s %s/%s` and write to the canvas before `moe sdlc %s`",
			priorStage, priorStage, projectID, runID, currentStage)
	}
	return nil
}

// checkSandboxBoundary refuses the design→code transition when the
// project sandbox has moved past the snapshot taken at design open.
// Two failure modes; either trips the gate:
//
//  1. HEAD has advanced — the agent committed to the project repo
//     during design. The spike-as-handoff path the design closed off.
//  2. `git status` shows any modified, added, or deleted tracked
//     file — the agent left dirty work behind. Untracked files are
//     deliberately allowed; the agent is free to scribble outside
//     the tracked set.
//
// Caller writes the canvas commit first and then runs this; a failure
// here returns a non-zero exit (suppressing the cascade) but the
// canvas changes are already preserved on the session branch.
//
// Hooks-side contract: project dev-env hooks (under
// `projects/<project>/hooks/dev-env.d/*`) must leave tracked files
// in the project repo alone — they should write to MOE_DEV_TMPDIR /
// MOE_HOME or other extern locations. A hook that mutates the work
// tree would false-positive this check.
func checkSandboxBoundary(clonePath, entryHEAD string) error {
	currentHEAD, err := git.HEAD(clonePath)
	if err != nil {
		return fmt.Errorf("sandbox boundary: read HEAD: %w", err)
	}
	if entryHEAD != "" && currentHEAD != entryHEAD {
		return fmt.Errorf(
			"sandbox HEAD advanced during design (was %s, now %s); design must not commit to the project repo — reset the sandbox and re-run",
			git.ShortSHA(entryHEAD), git.ShortSHA(currentHEAD))
	}
	entries, err := git.Status(clonePath)
	if err != nil {
		return fmt.Errorf("sandbox boundary: git status: %w", err)
	}
	var dirty []string
	for _, e := range entries {
		// Untracked entries carry XY=="??"; everything else is a
		// tracked-file change that the design stage is contracted
		// not to leave behind.
		if e.XY == "??" {
			continue
		}
		dirty = append(dirty, e.XY+" "+e.Path)
	}
	if len(dirty) > 0 {
		return fmt.Errorf(
			"sandbox has uncommitted tracked-file changes (design must not modify the project repo):\n  %s\nreset the sandbox and re-run",
			strings.Join(dirty, "\n  "))
	}
	return nil
}

// canvasKickoffCommit returns the SHA and subject of the first commit
// that added canvasRel. `git log --diff-filter=A --format=%H %s --
// <path>` lists adds newest-first; the last line is the original add.
// Returns "", "" with nil error if the path has no add in history (an
// untracked canvas), so the caller can decide what to do without
// disambiguating "no history" from "git failed".
func canvasKickoffCommit(root, canvasRel string) (sha, subject string, err error) {
	out, err := git.Output(root, "log", "--diff-filter=A", "--format=%H %s", "--", canvasRel)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if last == "" {
		return "", "", nil
	}
	sha, subject, _ = strings.Cut(last, " ")
	return sha, subject, nil
}
