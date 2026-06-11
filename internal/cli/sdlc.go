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
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// The SDLC workflow owns the design→code→push lifecycle. Stages are
// nested under `moe sdlc` so kb (and future workflows) can pick their
// own short stage names without collision. `moe sdlc new` is the entry
// point that creates a run in this workflow.

// sdlcCloseSubject is the commit-subject template for closing an sdlc
// run (a fmt.Sprintf string taking projectID, runID). Shared by the
// `moe sdlc close` verb and `moe serve`'s in-process CloseRun callback
// so the two close paths land identically-shaped commits.
const sdlcCloseSubject = "Close sdlc run %s/%s"

func init() {
	g := NewCommandGroup("sdlc", "sdlc workflow")
	g.Register(newRunCommand("sdlc"))
	g.Register(&Command{
		Name:    "design",
		Summary: "open an agent session on the run's design document",
		Run:     runDesign,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "code",
		Summary: "open an agent session on the run's code document (in a sandbox clone)",
		Run:     runCode,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "review",
		Summary: "open an agent session on the run's review document — review the code stage's work",
		Run:     runReview,
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "test",
		Summary: "open an agent session on the run's test document — verify the reviewed work",
		Run:     runTest,
		argKind: argProjectRun,
	})
	g.Register(pushCommand("sdlc"))
	g.Register(&Command{
		Name:    "shell",
		Summary: "drop into a shell rooted at a run's workspace, or at a named workspace directly",
		Run:     runShell,
		argKind: argProjectRun,
	})
	g.Register(closeCommand("sdlc", sdlcCloseSubject, releaseWorkspaceCleanup))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (sdlc cat <project>/<run> <stage>)",
		Run:     runCat("sdlc", ""),
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (sdlc log <project>/<run> <stage>)",
		Run:     runLog("sdlc", ""),
		argKind: argProjectRun,
	})
	g.Register(&Command{
		Name:    "reopen",
		Summary: "open a fresh sdlc run seeded with the design canvas of a terminal prior run",
		Run:     runSDLCReopen,
		argKind: argProjectRun,
	})
	RegisterGroup(g)

	w := NewWorkflow("sdlc")
	w.RegisterStage("design")
	w.RegisterStage("code", "design")
	w.RegisterStage("review", "code")
	w.RegisterStage("test", "review")
	w.RegisterStage("push", "test")
	w.RegisterStageGate("review", reviewStageGate)
	// Test stage's anti-theater check: the work-turn commit alone
	// doesn't tell us whether the agent actually filled the canvas
	// or just committed the placeholder skeleton. The gate reads the
	// canvas and refuses to advance until "What was verified" and
	// "What wasn't verified" both have substantive content.
	w.RegisterStageGate("test", testStageGate)
	RegisterWorkflow(w)

	// Serve declaration: front sdlc in the new-run/promote forms and
	// render the cascade trio (advance/ship/chain) on run pages. push
	// is excluded from web spawning — terminal/CLI-only stays a
	// recorded decision; the bang vocabulary collapses there.
	registerServeWorkflow("sdlc", serveWorkflowDecl{
		excludeStages: []string{"push"},
		cascade:       true,
		newRun:        true,
		workspace:     true,
	})
}

func runDesign(args []string, stdout, stderr io.Writer) int {
	return runSDLCStage(stageVerbCfg{
		verb:  "sdlc design",
		stage: "design",
		usage: []string{
			"Opens an interactive agent session on the design canvas.",
			"First use on a run creates the document; re-runs resume the session.",
		},
		open: openSdlcDesign,
	}, args, stdout, stderr)
}

func runCode(args []string, stdout, stderr io.Writer) int {
	return runSDLCStage(stageVerbCfg{
		verb:  "sdlc code",
		stage: "code",
		usage: []string{
			"Opens an interactive agent session on the code canvas. The agent",
			"works inside a private sandbox clone of the project's submodule, isolated",
			"from other activity until `moe sdlc push` opens a PR.",
		},
		open: openSdlcCode,
	}, args, stdout, stderr)
}

func runReview(args []string, stdout, stderr io.Writer) int {
	return runSDLCStage(stageVerbCfg{
		verb:  "sdlc review",
		stage: "review",
		usage: []string{
			"Opens an interactive agent session on the review canvas. The agent",
			"performs a senior-engineer review of the code stage's committed diff,",
			"blocking only for correctness, scope, maintainability, or reviewability",
			"issues that should send the run back to code.",
		},
		open: openSdlcReview,
	}, args, stdout, stderr)
}

func runTest(args []string, stdout, stderr io.Writer) int {
	return runSDLCStage(stageVerbCfg{
		verb:  "sdlc test",
		stage: "test",
		usage: []string{
			"Opens an interactive agent session on the test canvas. The agent",
			"verifies the reviewed work — running the project's checks, driving",
			"the change end-to-end, applying small in-place fixes, and narrating what",
			"was and wasn't verified on the canvas. Pre-push hooks still gate ship.",
		},
		open: openSdlcTest,
	}, args, stdout, stderr)
}

// stageVerbCfg holds the per-stage knobs runSDLCStage threads through:
// the operator-facing verb label (for error messages), the stage's
// position in the workflow ladder (the cascade's start when a mode
// flag is set), the multi-line usage preamble (printed above the flag
// list), and the typed-CLI opener the no-flag path falls into.
type stageVerbCfg struct {
	verb  string
	stage string
	usage []string
	open  func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int
}

// runSDLCStage is the shared body behind runDesign / runCode / runReview / runTest:
// parse the per-stage flags, branch to interactive (no cascade flag)
// or cascade (one of --once / --to / --ship / --chain), and surface
// cascade-mode mutual exclusion at parse time.
// Same shape every sdlc stage verb takes — keeping the body in one
// place is what made adding the cascade flags a one-stop edit.
func runSDLCStage(cfg stageVerbCfg, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cfg.verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "set the run's agent (claude/codex); persists to run.json")
	once := fs.Bool("once", false, "run "+cfg.stage+" headless and park at the next chain prompt (= ! at the chain prompt)")
	to := fs.String("to", "", "walk headless from "+cfg.stage+" up to (but not including) the named gate (= !<stage>)")
	ship := fs.Bool("ship", false, "headless cascade through push, ship this run (= !!)")
	chain := fs.Bool("chain", false, "headless cascade through push, then ride the whole chain (= !!!)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>\n", cfg.verb)
		moePrintln(stderr, "")
		for _, line := range cfg.usage {
			moePrintln(stderr, line)
		}
		moePrintln(stderr, "")
		moePrintln(stderr, "Cascade mode flags (mutually exclusive):")
		moePrintln(stderr, "  --once         dispatch one stage headless, park at the next gate (= !)")
		moePrintln(stderr, "  --to=<stage>   walk headless up to (but not including) <stage> (= !<stage>)")
		moePrintln(stderr, "  --ship         headless cascade through push, ship this run (= !!)")
		moePrintln(stderr, "  --chain        headless cascade through push, then ride the whole chain (= !!!)")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "%s: %v\n", cfg.verb, err)
		return 2
	}
	answer, ok := cascadeAnswerFromFlags(*once, *to, *ship, *chain)
	if !ok {
		moePrintf(stderr, "%s: cascade mode flags (--once, --to, --ship, --chain) are mutually exclusive\n", cfg.verb)
		return 2
	}
	if *agentOverride != "" {
		resolvedRunID, code := persistSDLCStageAgent(cfg.verb, cfg.stage, projectID, runID, *agentOverride, stdout, stderr)
		if code != 0 {
			return code
		}
		runID = resolvedRunID
	}
	if answer == "" {
		return cfg.open(projectID, runID, false, *agentOverride, stdout, stderr)
	}
	return dispatchCascadeForStage(cfg.verb, cfg.stage, projectID, runID, answer, stdout, stderr)
}

func persistSDLCStageAgent(verb, stage, projectID, runID, agentName string, stdout, stderr io.Writer) (string, int) {
	resolvedRunID, code := resolveSDLCRunSlug(verb, projectID, runID, stdout, stderr)
	if code != 0 {
		return "", code
	}
	root, err := findRoot(stderr)
	if err != nil {
		return "", 1
	}
	md, err := run.Load(root, projectID, resolvedRunID)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return "", 1
	}
	if md.Agent == agentName {
		return resolvedRunID, 0
	}
	md.Agent = agentName
	if err := run.Save(root, md); err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", 1
	}
	runJSON := filepath.Join(run.Dir(md.Project, md.ID), "run.json")
	msg := fmt.Sprintf("switch agent: %s/%s to %s\n\n", md.Project, md.ID, agentName) +
		trailers.Block{
			Run:      md.ID,
			Project:  md.Project,
			Workflow: md.Workflow,
			Document: stage,
		}.String()
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "switch-agent",
		Run:     md.Project + "/" + md.ID,
	}, stdout, stderr, func() error {
		return run.StageAndCommit(root, msg, runJSON)
	})
	if err != nil {
		moePrintf(stderr, "commit agent switch: %v\n", err)
		return "", 1
	}
	moePrintf(stdout, "switched run agent to %s\n", agentName)
	return resolvedRunID, 0
}

// cascadeAnswerFromFlags translates the four mode flags (--once,
// --to, --ship, --chain) into the bang answer dispatchCascade
// understands at the chain prompt. Exactly one of the four may be
// set; otherwise the flags conflict and ok=false. An empty answer
// with ok=true signals the no-flag case the caller routes through
// the standard interactive opener.
//
// The mapping mirrors the chain-prompt bang vocabulary one-for-one:
//
//	--once        → "!"            run startStage headless, park
//	--to=<stage>  → "!" + <stage>  walk headless to that gate
//	--ship        → "!!"           headless cascade, ship this run
//	--chain       → "!!!"          headless cascade, ship + ride the chain
func cascadeAnswerFromFlags(once bool, to string, ship, chain bool) (answer string, ok bool) {
	set := 0
	if once {
		set++
	}
	if to != "" {
		set++
	}
	if ship {
		set++
	}
	if chain {
		set++
	}
	if set > 1 {
		return "", false
	}
	switch {
	case once:
		return "!", true
	case to != "":
		return "!" + to, true
	case ship:
		return "!!", true
	case chain:
		return "!!!", true
	}
	return "", true
}

// dispatchCascadeForStage is the CLI-flag analogue of the chain
// prompt's bang dispatch: validate the --to=<stage> destination up
// front so a typo exits 2 (a real parse error) instead of falling
// through to dispatchCascade's chain-prompt-shaped no-op return of
// 0; resolve and guard the run (terminal / pushed / non-sdlc
// refused fast); then hand to dispatchCascade exactly as the chain
// prompt does. verb is the "sdlc <stage>" preamble used in stderr
// so unknown-destination errors surface under the command the
// operator just typed.
func dispatchCascadeForStage(verb, stage, projectID, runID, answer string, stdout, stderr io.Writer) int {
	if strings.HasPrefix(answer, "!") && answer != "!" && answer != "!!" && answer != "!!!" {
		dest := strings.TrimPrefix(answer, "!")
		wf, err := LookupWorkflow("sdlc")
		if err != nil {
			moePrintf(stderr, "%s: %v\n", verb, err)
			return 1
		}
		stages := wf.Stages()
		destIdx := indexOfString(stages, dest)
		if destIdx < 0 {
			moePrintf(stderr, "%s: --to=%s is not an sdlc stage; try: %s\n", verb, dest, strings.Join(stages, ", "))
			return 2
		}
		startIdx := indexOfString(stages, stage)
		if destIdx <= startIdx {
			past := stages[startIdx+1:]
			if len(past) == 0 {
				moePrintf(stderr, "%s: --to=%s is at or behind %s and no stage follows %s\n", verb, dest, stage, stage)
			} else {
				moePrintf(stderr, "%s: --to=%s is at or behind %s — pick a stage past %s (try: %s)\n", verb, dest, stage, stage, strings.Join(past, ", "))
			}
			return 2
		}
	}
	md, root, code := resolveAndGuardForCascade(verb, projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	return dispatchCascade(answer, stage, root, md, stdout, stderr)
}

// resolveAndGuardForCascade is the cascade-entry preflight every
// `moe sdlc <stage> --<mode>` invocation shares: resolve the typed
// slug (descendant-walk for promoted/reopened lineage), load the
// run, refuse non-sdlc / terminal / pushed runs. Returns the
// resolved metadata, the bureaucracy root, and 0 on success; a
// non-zero exit code (with stderr already written) on refusal.
//
// The chain-prompt's bang dispatch enters dispatchCascade through
// promptStageNextStage, which has already loaded md by then — so
// these guards only need to fire on the CLI-flag-entry leg.
func resolveAndGuardForCascade(verb, projectID, runID string, stdout, stderr io.Writer) (*run.Metadata, string, int) {
	resolved, code := resolveSDLCRunSlug(verb, projectID, runID, stdout, stderr)
	if code != 0 {
		return nil, "", code
	}
	runID = resolved
	root, err := findRoot(stderr)
	if err != nil {
		return nil, "", 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		return nil, "", 1
	}
	if md.Workflow != "sdlc" {
		moePrintf(stderr, "%s: %s/%s is a %s run, not sdlc\n", verb, projectID, runID, md.Workflow)
		return nil, "", 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "%s: %s/%s is %s; nothing to cascade\n", verb, projectID, runID, md.Status)
		return nil, "", 1
	case run.StatusPushed:
		moePrintf(stderr, "%s: %s/%s already pushed; cascade cannot drive a pushed run\n", verb, projectID, runID)
		return nil, "", 1
	}
	return md, root, 0
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
func openSdlcDesign(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc design", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	return runStageSession(projectID, runID, "design",
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
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
func openSdlcCode(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc code", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if err := requireDesignCanvas(projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return runStageSession(projectID, runID, "code",
		stageSessionOpts{NeedsSandbox: true, Headless: headless, Agent: agentOverride}, stdout, stderr)
}

// openSdlcReview is the Go-level seam behind `moe sdlc review`. Same
// shape as openSdlcCode one stage downstream — requireCodeCanvas
// stands in for requireDesignCanvas, and the canvas skeleton wires
// in so the agent's first read sees the structural shape it has to
// fill.
func openSdlcReview(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc review", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if err := requireCodeCanvas(projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return runStageSession(projectID, runID, "review",
		stageSessionOpts{
			NeedsSandbox:           true,
			EnforceSandboxBoundary: true,
			Headless:               headless,
			CanvasSkeleton:         reviewCanvasSkeleton,
			Agent:                  agentOverride,
		}, stdout, stderr)
}

// openSdlcTest is the Go-level seam behind `moe sdlc test`. Same
// shape as openSdlcReview one stage downstream — requireReviewCanvas
// ensures a review canvas exists before verification starts.
func openSdlcTest(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	resolved, code := resolveSDLCRunSlug("sdlc test", projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	runID = resolved
	if err := requireReviewCanvas(projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return runStageSession(projectID, runID, "test",
		stageSessionOpts{
			NeedsSandbox:   true,
			Headless:       headless,
			CanvasSkeleton: testCanvasSkeleton,
			Agent:          agentOverride,
		}, stdout, stderr)
}

// openSdlcStage routes the chain prompt's cascade driver
// (`!` / `!<stage>` / `!!` / `!!!`) and the cascade's pre-push iteration to
// the right per-stage helper, headless. Knowing the stage names
// statically (sdlc has four headlessable stages — push is not one
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
var openSdlcStage func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

func init() {
	openSdlcStage = func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		// Chain / cascade entry: no per-call --agent override. The run's
		// persisted agent (from run.json) takes over inside
		// runStageSession. Every cascade path is headless now — the
		// dispatcher always passes headless=true — so `!` / `!<stage>` /
		// `!!` / `!!!` all run `claude -p`.
		switch stage {
		case "design":
			return openSdlcDesign(projectID, runID, headless, "", stdout, stderr)
		case "code":
			return openSdlcCode(projectID, runID, headless, "", stdout, stderr)
		case "review":
			return openSdlcReview(projectID, runID, headless, "", stdout, stderr)
		case "test":
			return openSdlcTest(projectID, runID, headless, "", stdout, stderr)
		default:
			moePrintf(stderr, "sdlc: openSdlcStage: unknown stage %q\n", stage)
			return 1
		}
	}
	registerCascadeDispatcher("sdlc", func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openSdlcStage(stage, projectID, runID, headless, stdout, stderr)
	})
}

const reviewCanvasSkeleton = `# Review

## Gate

` + "```json" + `
{"status":"blocked"}
` + "```" + `

Allowed values: "ready" or "blocked". Use "blocked" only for a known correctness, scope, maintainability, or reviewability problem that should stop the cascade. Non-blocking observations that shape verification can be recorded under Findings while leaving status "ready"; out-of-scope work worth doing later goes to the run's followups.md.

## Findings

(agent fills: blocking correctness, scope, maintainability, or reviewability issues; empty only when status is "ready".)

## Evidence Reviewed

(agent fills: design/code canvases, diff ranges, commands or tests read/run)
`

// testCanvasSkeleton is the fixed structural shape every test canvas
// opens with. The Next.satisfied check (see workflow.go) enforces a
// ready gate plus non-empty "What was verified" and "What wasn't
// verified" sections; the stage fragment instructs the agent on the
// anti-theater rules.
const testCanvasSkeleton = `# Test

## Gate

` + "```json" + `
{"status":"blocked"}
` + "```" + `

Allowed values: "ready" or "blocked". Use "blocked" for known failures or unresolved issues that should halt push; do not block merely because some surfaces are explicitly listed under "What wasn't verified".

## What was verified

(agent fills: commands run, end-to-end paths driven, what passed - cite and quote)

## What wasn't verified

(agent fills: skipped surfaces + why - needs human eye, needs prod-shaped data, out of scope. "Nothing - automated tests cover the change" is acceptable for pure-backend work.)

## Fixes applied during this stage

(agent fills: one row per in-place fix; empty if none)
`

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

// requireCodeCanvas is the analogue for review stage: refuse to open a
// review session when there's no code canvas to review. Same fail-loud
// invariant as requireDesignCanvas, one stage downstream.
func requireCodeCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "code", "review")
}

func requireReviewCanvas(projectID, runID string) error {
	return requirePriorCanvas(projectID, runID, "review", "test")
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
//     is the cascade footgun the design twin records: a `!!` / `!!!` after
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

// checkSandboxBoundary refuses to close a stage that sets
// EnforceSandboxBoundary (design, chat, audit plan/report, the pdlc
// stages) when the project sandbox has moved past the snapshot taken
// at stage open. stageDoc is the stage's doc name (e.g. "design",
// "frame"), used to attribute the refusal. Two failure modes; either
// trips the gate:
//
//  1. HEAD has advanced — the agent committed to the project repo
//     during the stage. The spike-as-handoff path the design closed
//     off.
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
func checkSandboxBoundary(clonePath, entryHEAD, stageDoc string) error {
	currentHEAD, err := git.HEAD(clonePath)
	if err != nil {
		return fmt.Errorf("sandbox boundary: read HEAD: %w", err)
	}
	if entryHEAD != "" && currentHEAD != entryHEAD {
		return fmt.Errorf(
			"sandbox HEAD advanced during %s (was %s, now %s); %s must not commit to the project repo — reset the sandbox and re-run",
			stageDoc, git.ShortSHA(entryHEAD), git.ShortSHA(currentHEAD), stageDoc)
	}
	entries, err := git.Status(clonePath)
	if err != nil {
		return fmt.Errorf("sandbox boundary: git status: %w", err)
	}
	var dirty []string
	for _, e := range entries {
		// Untracked entries carry XY=="??"; everything else is a
		// tracked-file change that the stage is contracted not to
		// leave behind.
		if e.XY == "??" {
			continue
		}
		dirty = append(dirty, e.XY+" "+e.Path)
	}
	if len(dirty) > 0 {
		return fmt.Errorf(
			"sandbox has uncommitted tracked-file changes (%s must not modify the project repo):\n  %s\nreset the sandbox and re-run",
			stageDoc, strings.Join(dirty, "\n  "))
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
