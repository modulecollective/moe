package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

// operatorCascades reports whether a workflow participates in the
// operator cascade vocabulary — the stage-verb cascade flags
// (--once/--to/--ship/--chain), chain-edit membership, and the serve
// advance/ship/chain chips. One predicate keys all three surfaces so a
// workflow can't be half-wired: a workflow that registers a cascade
// dispatcher gets the full vocabulary everywhere at once, and one that
// shouldn't (perpetual chat, machine-paced pulse) declares that once —
// SetPerpetual / SetMachinePaced — and is excluded everywhere at once.
//
// The rule: a registered cascade dispatcher, and neither perpetual nor
// machine-paced. A dispatcher is the "has a stage ladder to walk"
// signal; the two exclusions are the principled non-participants (chat
// is perpetual — "ship" is meaningless; pulse is machine-minted and
// machine-driven). Everything else is an operator-paced workflow an
// operator opens and rides to a terminal state.
func operatorCascades(workflow string) bool {
	if lookupCascadeDispatcher(workflow) == nil {
		return false
	}
	wf, err := LookupWorkflow(workflow)
	if err != nil {
		return false
	}
	return !wf.Perpetual() && !wf.MachinePaced()
}

// cascadeUnavailableReason names why a workflow can't --ship, for the
// mint-tail preflight refusal. operatorCascades has already returned
// false; this reports which leg failed so the operator sees a reason,
// not a bare "can't". The no-dispatcher wording keeps the "no cascade"
// phrase the new --ship refusal has always printed.
func cascadeUnavailableReason(workflow string) string {
	if wf, err := LookupWorkflow(workflow); err == nil {
		if wf.Perpetual() {
			return workflow + " is perpetual — there is no ship to cascade to"
		}
		if wf.MachinePaced() {
			return workflow + " is machine-paced — moe opens and drives its runs, not you"
		}
	}
	return fmt.Sprintf("workflow %q has no cascade — open without --ship and drive the stages yourself", workflow)
}

// mintCascadeFlag names the flag that produced a mint verb's cascade
// answer, so the exclusion checks below blame the flag the operator
// actually typed. Only `new` offers the upper two today; every other
// mint verb tops out at --ship, which is also the zero-ish default.
func mintCascadeFlag(cascade string) string {
	switch cascade {
	case "!!!":
		return "--chain"
	case "!!!!":
		return "--dynamic"
	default:
		return "--ship"
	}
}

// shipAnswer is the cascade answer for the mint verbs whose ladder
// stops at --ship: `!!`, or "" when the flag is absent. `new` carries
// the full ladder and resolves its own answer via
// cascadeAnswerFromFlags.
func shipAnswer(ship bool) string {
	if ship {
		return "!!"
	}
	return ""
}

// parkCascadeExclusive refuses --park alongside a cascade flag, the
// combination every mint verb shares: they're opposite tails (one
// stops, one cascades). cascade is the bang answer the cascade flags
// resolved to, "" when none was given. verb is the "<wf> <verb>"
// stderr prefix. Returns 0 to proceed; exit 2 with the message written
// on conflict.
func parkCascadeExclusive(verb string, park bool, cascade string, stderr io.Writer) int {
	if park && cascade != "" {
		moePrintf(stderr, "%s: %s and --park are opposite tails (one cascades, the other stops) — pick one\n",
			verb, mintCascadeFlag(cascade))
		return 2
	}
	return 0
}

// preflightMintTail is the parse-time mint-verb check for the verbs
// that know their workflow up front (new, twin reflect): the --park /
// cascade exclusion, plus a cascade refusal on a workflow that can't
// cascade — so a run we couldn't ship is never minted. chore open,
// whose target workflow is only known after the open, uses
// parkCascadeExclusive here and leans on mintTail's own cascade guard.
func preflightMintTail(verb, workflow string, park bool, cascade string, stderr io.Writer) int {
	if code := parkCascadeExclusive(verb, park, cascade, stderr); code != 0 {
		return code
	}
	if cascade != "" && !operatorCascades(workflow) {
		moePrintf(stderr, "%s: %s: %s\n", verb, mintCascadeFlag(cascade), cascadeUnavailableReason(workflow))
		return 2
	}
	return 0
}

// mintTail is the shared post-open tail every mint verb ends with:
// --park prints the next-stage hint and stops; a cascade flag runs the
// fresh run headless from its first pending stage; neither offers the
// standard chain prompt.
//
// cascade is the bang answer the verb's flags resolved to, "" for
// neither tail. --ship is `!!` on every mint verb — a fresh run has no
// chained children, so shipping it is `!!` not `!!!`. `new` also offers
// `!!!` / `!!!!`, where the extra bangs are about what happens *after*
// the ship: ride whatever the run chains onto, and (at four) let the
// machine extend that ride. Same seam either way — the answer string is
// all that differs.
//
// Callers that know their workflow at parse time (new, reflect) have
// already refused a non-cascading tail via preflightMintTail; the guard
// here re-checks for chore open, whose workflow is only known now.
func mintTail(root string, md *run.Metadata, park bool, cascade string, stdout, stderr io.Writer) int {
	if park {
		return promptNextStageParked(root, md, stdout, stderr)
	}
	if cascade != "" {
		if !operatorCascades(md.Workflow) {
			moePrintf(stderr, "%s: %s\n", mintCascadeFlag(cascade), cascadeUnavailableReason(md.Workflow))
			return 1
		}
		wf, err := LookupWorkflow(md.Workflow)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
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
		return dispatchCascade(cascade, stage, root, md, stdout, stderr)
	}
	return promptNextStage(root, md, "", stdout, stderr)
}

// slugResolver resolves a typed <project>/<run> slug to the actual run
// id for a cascade-flag invocation. sdlc uses resolveSDLCRunSlug (the
// promoted/reopened descendant walk); every other workflow uses
// plainRunSlug (pass-through — resolveAndGuardForCascade does the load
// and the workflow check). Returns the resolved id and a process exit
// code (0 to proceed; non-zero with stderr already written).
type slugResolver func(verb, projectID, runID string, stdout, stderr io.Writer) (string, int)

// plainRunSlug is the no-lineage slug resolver every workflow but sdlc
// uses: pass the typed slug through unchanged. Existence, workflow, and
// status are checked by resolveAndGuardForCascade's load; a workflow
// with no promoted/reopened lineage has nothing to walk here.
func plainRunSlug(_, _, runID string, _, _ io.Writer) (string, int) {
	return runID, 0
}

// stageVerbCfg holds the per-stage knobs runStageVerb threads through:
// the owning workflow, the operator-facing verb label (for error
// messages), the stage's position in the workflow ladder (the
// cascade's start when a mode flag is set), the multi-line usage
// preamble (printed above the flag list), and the typed-CLI opener the
// no-flag path falls into. The two per-workflow hooks are resolveSlug
// (sdlc's lineage walk vs. plainRunSlug) and persistAgent (sdlc writes
// --agent to run.json; everyone else applies it per-turn).
type stageVerbCfg struct {
	workflow     string
	verb         string
	stage        string
	usage        []string
	open         func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int
	resolveSlug  slugResolver
	persistAgent bool
}

// runStageVerb is the shared body behind every workflow's stage verbs
// (sdlc design/code/review/test, twin's six, kb research/summarize,
// hooks/chores code): parse the per-stage flags, branch to interactive
// (no cascade flag) or cascade (one of --once / --to / --ship /
// --chain), and surface cascade-mode mutual exclusion at parse time.
// Per-workflow variance is the two cfg hooks; the cascade flags are
// gated on operatorCascades so a non-participating workflow that routes
// through here refuses them instead of half-honoring them.
//
// Keeping the body in one place is what makes the cascade vocabulary a
// property of the workflow rather than a per-verb opt-in: adding a
// workflow to the vocabulary is a cfg, not a re-implementation.
func runStageVerb(cfg stageVerbCfg, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cfg.verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", agentFlagUsage(cfg.persistAgent))
	once := fs.Bool("once", false, "run "+cfg.stage+" headless and park at the next chain prompt (= ! at the chain prompt)")
	to := fs.String("to", "", "walk headless from "+cfg.stage+" up to (but not including) the named gate (= !<stage>)")
	ship := fs.Bool("ship", false, "headless cascade through push, ship this run (= !!)")
	chain := fs.Bool("chain", false, "headless cascade through push, then ride the whole chain (= !!!)")
	dynamic := fs.Bool("dynamic", false, "as --chain, but the ride may grow while it runs (= !!!!)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s [--agent <name>] [--once | --to=<stage> | --ship | --chain | --dynamic] <project>/<run>\n", cfg.verb)
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
		moePrintln(stderr, "  --dynamic      as --chain, but the ride may grow: tail pulses may groom onto")
		moePrintln(stderr, "                 the ridden chain's tail, and kick threads they root (= !!!!)")
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
	answer, ok := cascadeAnswerFromFlags(*once, *to, *ship, *chain, *dynamic)
	if !ok {
		moePrintf(stderr, "%s: cascade mode flags (--once, --to, --ship, --chain, --dynamic) are mutually exclusive\n", cfg.verb)
		return 2
	}
	if answer != "" && !operatorCascades(cfg.workflow) {
		moePrintf(stderr, "%s: %s\n", cfg.verb, cascadeUnavailableReason(cfg.workflow))
		return 2
	}
	if cfg.persistAgent && *agentOverride != "" {
		resolvedRunID, code := persistSDLCStageAgent(cfg.verb, cfg.stage, projectID, runID, *agentOverride, stdout, stderr)
		if code != 0 {
			return code
		}
		runID = resolvedRunID
	}
	if answer == "" {
		return cfg.open(projectID, runID, false, *agentOverride, stdout, stderr)
	}
	return dispatchCascadeForStage(cfg, projectID, runID, answer, *to, stdout, stderr)
}

// agentFlagUsage picks the --agent help text for a stage verb: sdlc
// persists the value to run.json, every other workflow applies it for
// the one turn. Same knob, honestly documented per workflow.
func agentFlagUsage(persist bool) string {
	if persist {
		return "set the run's agent (claude/codex); persists to run.json"
	}
	return "override the run's agent for this turn (claude/codex); does not persist"
}

// cascadeAnswerFromFlags translates the five mode flags (--once,
// --to, --ship, --chain, --dynamic) into the bang answer dispatchCascade
// understands at the chain prompt. Exactly one of the five may be
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
//	--dynamic     → "!!!!"         same ride, and the machine may extend it
//
// --dynamic is a fifth mutually-exclusive member rather than a modifier
// on --chain, mirroring the bang grammar it maps to: the consent levels
// are a ladder, not a flag plus an option.
func cascadeAnswerFromFlags(once bool, to string, ship, chain, dynamic bool) (answer string, ok bool) {
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
	if dynamic {
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
	case dynamic:
		return "!!!!", true
	}
	return "", true
}

// dispatchCascadeForStage is the CLI-flag analogue of the chain
// prompt's bang dispatch: validate the --to=<stage> destination up
// front so a typo exits 2 (a real parse error) instead of falling
// through to dispatchCascade's chain-prompt-shaped no-op return of
// 0; resolve and guard the run (terminal / pushed / wrong-workflow
// refused fast); then hand to dispatchCascade exactly as the chain
// prompt does. cfg.verb is the "<wf> <stage>" preamble used in stderr
// so unknown-destination errors surface under the command the operator
// just typed, and cfg.workflow parameterizes the ladder the destination
// is checked against.
//
// Validation keys on the raw `to` flag, not the composed answer:
// cascadeAnswerFromFlags maps `--to=<stage>` to `"!"+<stage>`, so
// `--to=!` composes to the same `!!` string `--ship` legitimately
// produces. Sniffing the answer would let `--to=!` / `--to=!!` slip
// past as valid ship/chain forms; the ladder check below runs against
// the operator's actual `--to` value instead.
func dispatchCascadeForStage(cfg stageVerbCfg, projectID, runID, answer, to string, stdout, stderr io.Writer) int {
	if to != "" {
		dest := to
		wf, err := LookupWorkflow(cfg.workflow)
		if err != nil {
			moePrintf(stderr, "%s: %v\n", cfg.verb, err)
			return 1
		}
		stages := wf.Stages()
		destIdx := indexOfString(stages, dest)
		if destIdx < 0 {
			moePrintf(stderr, "%s: --to=%s is not a stage of %s; try: %s\n", cfg.verb, dest, cfg.workflow, strings.Join(stages, ", "))
			return 2
		}
		startIdx := indexOfString(stages, cfg.stage)
		if destIdx <= startIdx {
			past := stages[startIdx+1:]
			if len(past) == 0 {
				moePrintf(stderr, "%s: --to=%s is at or behind %s and no stage follows %s\n", cfg.verb, dest, cfg.stage, cfg.stage)
			} else {
				moePrintf(stderr, "%s: --to=%s is at or behind %s — pick a stage past %s (try: %s)\n", cfg.verb, dest, cfg.stage, cfg.stage, strings.Join(past, ", "))
			}
			return 2
		}
	}
	md, root, code := resolveAndGuardForCascade(cfg, projectID, runID, stdout, stderr)
	if code != 0 {
		return code
	}
	return dispatchCascade(answer, cfg.stage, root, md, stdout, stderr)
}

// resolveAndGuardForCascade is the cascade-entry preflight every
// `moe <wf> <stage> --<mode>` invocation shares: resolve the typed
// slug (cfg.resolveSlug — sdlc's descendant walk for promoted/reopened
// lineage, plain pass-through elsewhere), load the run, refuse a
// wrong-workflow / terminal / pushed run. Returns the resolved
// metadata, the bureaucracy root, and 0 on success; a non-zero exit
// code (with stderr already written) on refusal.
//
// The chain-prompt's bang dispatch enters dispatchCascade through
// promptStageNextStage, which has already loaded md by then — so
// these guards only need to fire on the CLI-flag-entry leg. The pushed
// case can only occur for a workflow with a push stage (sdlc today);
// leaving it in the shared switch is harmless for the workflows that
// never reach StatusPushed and keeps the guard in one place.
func resolveAndGuardForCascade(cfg stageVerbCfg, projectID, runID string, stdout, stderr io.Writer) (*run.Metadata, string, int) {
	resolved, code := cfg.resolveSlug(cfg.verb, projectID, runID, stdout, stderr)
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
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "%s: run not found: %s/%s\n", cfg.verb, projectID, runID)
		} else {
			moePrintf(stderr, "%s: %v\n", cfg.verb, err)
		}
		return nil, "", 1
	}
	if md.Workflow != cfg.workflow {
		moePrintf(stderr, "%s: %s/%s is a %s run, not %s\n", cfg.verb, projectID, runID, md.Workflow, cfg.workflow)
		return nil, "", 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "%s: %s/%s is %s; nothing to cascade\n", cfg.verb, projectID, runID, md.Status)
		return nil, "", 1
	case run.StatusPushed:
		moePrintf(stderr, "%s: %s/%s already pushed; cascade cannot drive a pushed run\n", cfg.verb, projectID, runID)
		return nil, "", 1
	}
	return md, root, 0
}
