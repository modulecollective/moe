package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// twinStageRun is the Command.Run factory behind every per-stage twin
// verb (`moe twin vision`, `moe twin architecture`, …). The stage name
// is baked into the cfg so the dispatch table in twin.go stays a thin
// list of (name, Run) pairs. Routes through the shared runStageVerb
// body so twin's six verbs carry the same cascade vocabulary
// (--once/--to/--ship/--chain) sdlc's do — twin operatorCascades, so
// the flags are honored. Per-turn --agent (persistAgent: false) and a
// plain slug resolver are twin's only departures from sdlc's cfg.
func twinStageRun(stage string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		return runStageVerb(stageVerbCfg{
			workflow: "twin",
			verb:     "twin " + stage,
			stage:    stage,
			usage: []string{
				"Opens an interactive agent session on the " + stage + "-stage canvas",
				"of a twin reflect run. First use on a run creates the document;",
				"re-runs resume the session.",
			},
			open: func(projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
				return openTwinStage(stage, projectID, runID, headless, agentOverride, stdout, stderr)
			},
			resolveSlug: plainRunSlug,
		}, args, stdout, stderr)
	}
}

// openTwinStage is the Go-level seam behind both the typed verb (`moe
// twin <stage>`, headless=false) and the chain prompt's cascade driver
// (`!` / `!<stage>` / `!!` / `!!!`, headless varies). Identical contract
// to openSdlcStage one workflow over: switch on the stage name, hand to
// the right helper, and carry headless through to runTwinStageSession.
// A `default` branch surfaces an unknown-stage call as a stderr line
// rather than silently routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain doesn't trip Go's init-order cycle checker — same shape
// openSdlcStage uses, same reason.
var openTwinStage func(stage, projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int

func init() {
	openTwinStage = func(stage, projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
		switch stage {
		case "vision", "architecture", "patterns", "operations", "glossary", "finalize":
			if code := requireTwinRun("twin "+stage, projectID, runID, stderr); code != 0 {
				return code
			}
			if err := requireTwinPriorCanvas(stage, projectID, runID); err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			return runTwinStageSession(stage, projectID, runID, headless, agentOverride, stdout, stderr)
		default:
			moePrintf(stderr, "twin: openTwinStage: unknown stage %q\n", stage)
			return 1
		}
	}
	// Register the dispatcher so generic chain-prompt / cascade code
	// can reach twin stages via workflow name, not via a hardcoded
	// switch on "sdlc". The cascade carries no per-call --agent
	// override — the run's persisted agent (from run.json) takes over
	// inside runStageSession, matching openSdlcStage one workflow over.
	registerCascadeDispatcher("twin", func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int {
		return openTwinStage(stage, projectID, runID, headless, "", stdout, stderr)
	})
}

// runTwinStageSession wraps runStageSession with the twin-specific
// options the six stages share: closed-schema wiki builder, a
// read-only source sandbox, and SkipFinalize=true for every stage *but*
// finalize. Finalize additionally wires the post-flight hygiene gate
// (the engine refuses to seal the pass with leftover findings) and a
// canvas skeleton.
//
// The pass-scoped kickoff context (events, hygiene findings, twin
// feedback, history summary) is assembled against the session worktree
// via InitialPromptBuilder — after the worktree exists and the wiki cfg
// is rewritten to worktree paths — and folded into the InitialPrompt.
// Each stage sees the same payload — the design's "no in-session
// iteration across docs, every stage reads the same events list"
// contract.
func runTwinStageSession(stage, projectID, runID string, headless bool, agentOverride string, stdout, stderr io.Writer) int {
	opts := stageSessionOpts{
		Headless: headless,
		Agent:    agentOverride,
		// The design/chat/pulse shape: a per-run clone of the project's
		// source, exposed via --add-dir, with a close gate that refuses
		// the stage if any tracked file moved. A reflect stage's job is
		// to check prose claims against the code that motivated them,
		// and it cannot do that from the session worktree — a plain
		// `git worktree add` leaves projects/<p>/src an unpopulated
		// submodule mountpoint. Uniform across all six stages: finalize
		// doesn't strictly need source, but one opts literal beats a
		// per-stage split and the boundary gate is pure protection.
		NeedsSandbox:           true,
		EnforceSandboxBoundary: true,
		// Deferred so the kickoff's by-path references (history summary,
		// managed docs) render against the worktree, not the canonical
		// checkout. Building it eagerly here — before the worktree
		// existed — handed the agent absolute canonical paths and let a
		// reflect pass edit the operator's live tree.
		InitialPromptBuilder: func(workRoot string, worktreeWiki *wiki.Config, stubbed bool) (string, error) {
			return buildTwinStageKickoff(projectID, workRoot, worktreeWiki, stubbed)
		},
		WikiBuilder: func(root string, md *run.Metadata) (*wiki.Config, error) {
			return twinWikiBuilder(root, projectID)
		},
		// Per-stage commits don't bump the checkpoint; finalize does.
		// Without this, stage two's `EventsSinceCheckpoint` would
		// return a shorter list than stage one's, because finalize on
		// stage one would have already advanced `LastIngestAt`.
		SkipFinalize: stage != "finalize",
	}
	if stage == "finalize" {
		opts.CanvasSkeleton = finalizeCanvasSkeleton
		// Post-flight hygiene gate: re-scan the worktree-rewritten wiki
		// before sealing. Non-empty findings refuse FinalizeIngest *and*
		// the per-turn commit — the operator re-runs finalize and walks
		// the agent through the remainder inline.
		opts.PreFinalizeGate = func(workRoot string, worktreeWiki *wiki.Config) error {
			return reflectPostFlightGate(worktreeWiki, stderr)
		}
	}
	return runStageSession(projectID, runID, stage, opts, stdout, stderr)
}

// buildTwinStageKickoff renders the per-stage InitialPrompt: the
// pass-scoped context block (events, hygiene findings, feedback,
// history summary) every stage reads. The block is load-bearing data
// the agent needs whether or not an operator is present, so it rides on
// both the interactive and headless paths.
//
// Wired as stageSessionOpts.InitialPromptBuilder, so runStageSession
// invokes it after the session worktree is open and worktreeWiki has
// been rewritten to worktree paths. It renders entirely against that
// worktree — workRoot is both the feedback-scan root and the
// BureaucracyPath behind every absolute path the kickoff names — so the
// agent's first instruction points inside the worktree. Assembling this
// against the canonical root before the worktree existed is what let a
// reflect pass write `glossary.md` / `history-summary.md` into the
// operator's live checkout.
func buildTwinStageKickoff(projectID, workRoot string, worktreeWiki *wiki.Config, stubbed bool) (string, error) {
	return reflectKickoffContext(workRoot, projectID, *worktreeWiki, stubbed)
}

// finalizeCanvasSkeleton seeds the finalize canvas with the three
// load-bearing sections finalizeStageGate enforces. Mirrors
// testCanvasSkeleton in shape — anti-theater: a committed skeleton
// with the seeded `(...)` placeholder lines does not advance the
// stage.
const finalizeCanvasSkeleton = `# Finalize

## What I fixed

(agent fills: concrete cleanups applied inline this pass — which entry renamed, which broken link patched, which doc tidied. Empty if the prior stages left a clean sheet, but say so.)

## What I left

(agent fills: findings I couldn't fix here, with the reason and the feedback-twin note or follow-up where the residue lives. "Nothing left" is a valid answer.)

## History-summary delta

(agent fills: the rolled-up history-summary.md for this pass. Don't just append — fold this pass's events in at full detail as the newest horizon, compress the prior few horizons into progressively terser lines, and reduce the oldest to a line or drop them once what survives still carries their signal. The doc is a decaying-resolution timeline, not an append-only log: newest sharp, older blurred, oldest gone — so it stays bounded instead of growing every reflect. Preserve signal a future reflect needs; shed detail it won't.)
`

// requireTwinRun is the per-stage entry guard: load run.json, refuse
// if missing, refuse if the run isn't a twin run. Returns the process
// exit code (0 to proceed, non-zero with stderr already written).
func requireTwinRun(verb, projectID, runID string, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "%s: run not found: %s/%s\n", verb, projectID, runID)
			return 1
		}
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	}
	if md.Workflow != "twin" {
		moePrintf(stderr, "%s: run %s/%s is a %s run, not twin\n", verb, projectID, runID, md.Workflow)
		return 1
	}
	return 0
}

// requireTwinPriorCanvas refuses a twin stage when the prior stage's
// canvas is missing or empty. Mirrors requirePriorCanvas one workflow
// over — same fail-loud invariant: a stage can't drive against
// upstream that was never opened. Vision has no prior stage and is
// exempt; finalize requires the glossary canvas.
//
// Unlike requirePriorCanvas, this helper does *not* check the canvas-
// blob-vs-kickoff invariant. Twin runs are minted by reflectCommand
// without seed canvases — the session-start commit's blob for any
// per-stage canvas is empty, and the no-op-session refusal is already
// covered by session.Close's CanvasUnchangedError on the upstream
// stage. The simpler "exists + non-empty" check is enough.
func requireTwinPriorCanvas(stage, projectID, runID string) error {
	prior := twinPriorStage(stage)
	if prior == "" {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return err
	}
	canvasRel := run.ContentPath(projectID, runID, prior)
	info, err := os.Stat(filepath.Join(root, canvasRel))
	switch {
	case errors.Is(err, fs.ErrNotExist), err == nil && info.Size() == 0:
		return fmt.Errorf("%s canvas missing — run `moe twin %s %s/%s` before `moe twin %s`",
			prior, prior, projectID, runID, stage)
	case err != nil:
		return fmt.Errorf("stat %s canvas: %w", prior, err)
	}
	return nil
}

// twinPriorStage returns the stage that must have committed a canvas
// before stage can open, or "" for vision (the first stage). Linear
// ladder — same shape twinStageOrder declares.
func twinPriorStage(stage string) string {
	for i, s := range twinStageOrder {
		if s == stage {
			if i == 0 {
				return ""
			}
			return twinStageOrder[i-1]
		}
	}
	return ""
}

// finalizeStageGate is the finalize-stage equivalent of testStageGate:
// it refuses to mark the stage satisfied when the canvas left the two
// anti-theater sections (What I fixed, What I left) empty or sitting
// on their seeded placeholder lines.
//
// The history-summary delta section is intentionally exempt from this
// check: a passlet with no events to compress can legitimately leave
// it short or even empty (the rolling summary already covers the
// prior horizon). The hygiene-finding refusal is enforced separately
// by reflectPostFlightGate at session close.
func finalizeStageGate(root string, md *run.Metadata) (bool, error) {
	canvas := filepath.Join(root, run.ContentPath(md.Project, md.ID, "finalize"))
	body, err := os.ReadFile(canvas)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	sections := parseTestCanvasSections(string(body))
	return testSectionFilled(sections["What I fixed"]) &&
		testSectionFilled(sections["What I left"]), nil
}

// reflectKickoffContext returns the per-stage kickoff payload: events,
// history summary, twin feedback, and hygiene findings. It is wired as
// the InitialPrompt / turn prompt
// (buildTwinStageKickoff → stageSessionOpts.InitialPromptBuilder) — the
// first user message the agent receives, distinct from the system prompt
// (Request.Prompt) that stage_prompt.go assembles. Both ride on argv, so
// neither may carry an unbounded string; the history summary is surfaced
// by path, not inlined, for exactly that reason (see the History summary
// block below). The whole block carries pass-scoped context every stage
// needs to see; the per-stage framing lives in the stage fragment.
//
// stubbed carries the EnsureManagedDocs seed signal: when true the
// managed docs were freshly created this turn, and the block opens with
// a seed framing that tells the agent to author the stubs rather than
// walk them against events. Without it a headless seed pass reads its
// own stub docs and the "don't rewrite intent" caution as "do nothing"
// — the dropped-thread this run was opened against.
//
// Returns the rendered markdown block plus any error from the
// underlying wiki / git reads. An empty block is valid (a fresh
// project with no events, no feedback, and a clean scan).
func reflectKickoffContext(root, projectID string, cfg wiki.Config, stubbed bool) (string, error) {
	events, err := wiki.EventsSinceCheckpoint(cfg)
	if err != nil {
		return "", fmt.Errorf("wiki: events: %w", err)
	}
	historySummary, err := wiki.ReadHistorySummary(cfg)
	if err != nil {
		return "", fmt.Errorf("wiki: history summary: %w", err)
	}
	feedback, err := loadTwinFeedback(root, projectID, cfg)
	if err != nil {
		return "", fmt.Errorf("wiki: feedback: %w", err)
	}
	findings, err := wiki.Scan(cfg)
	if err != nil {
		return "", fmt.Errorf("wiki scan: %w", err)
	}

	var b strings.Builder
	b.WriteString("## Pass context\n\n")
	b.WriteString("The blocks below are the same for every stage of this " +
		"twin reflect pass. Read them once at vision and lean on them at " +
		"each successor stage — the events list, the hygiene findings, and " +
		"the workflow feedback all carry pass-scoped context.\n\n")

	if stubbed {
		b.WriteString("### Seeding a fresh twin\n\n")
		b.WriteString("These managed docs are stubs — this is the twin's " +
			"first reflect pass and the docs are empty beyond a heading. " +
			"Author them from the events and project context below; a stub " +
			"is the strongest signal that a doc needs you to write it, not a " +
			"flag to defer. The committed pass is what the operator reviews, " +
			"so write your best evidence-backed draft rather than leaving the " +
			"docs blank for a confirmation that never comes.\n\n")
	}

	if !findings.IsEmpty() {
		b.WriteString("### Hygiene findings\n\n")
		b.WriteString("Structural issues the pre-flight scan surfaced. The " +
			"per-doc stages should fold relevant findings into their walks; " +
			"the finalize stage owns inline cleanup. The engine re-scans at " +
			"the finalize seal and refuses to ship a reflect with leftover " +
			"findings.\n\n")
		b.WriteString(wiki.RenderFindings(findings))
		b.WriteString("\n")
	}

	b.WriteString("### Workflow feedback\n\n")
	if len(feedback) == 0 {
		b.WriteString("(no workflow feedback since the last reflect)\n\n")
	} else {
		b.WriteString("Notes that workflow agents — including the previous " +
			"reflect pass's own residue — left for this " +
			"reflect pass. Treat as input, not direction — fold what's " +
			"real into the relevant managed doc on the appropriate " +
			"stage, set aside what isn't.\n\n")
		for _, fb := range feedback {
			fmt.Fprintf(&b, "#### %s (%s)\n\n", fb.runID, fb.when.Format("2006-01-02"))
			body := strings.TrimSpace(fb.body)
			if body == "" {
				b.WriteString("(empty feedback file)\n\n")
				continue
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("### History summary\n\n")
	if historySummary != "" {
		// Surface the summary by path, not inlined. It can grow past the
		// kernel's per-argv-string ceiling (MAX_ARG_STRLEN = 128 KiB) and
		// this kickoff rides on argv, so inlining it broke the launch with
		// E2BIG. The imperative must be unambiguous: a path the agent
		// skips is worse than the old inline.
		fmt.Fprintf(&b, "Read the rolling history summary at `%s` now, "+
			"before you start this stage — it is your primary context for "+
			"what past reflect passes changed and why, the compressed memory "+
			"of project history before the events listed below. Open it with "+
			"your read tool; don't skip it.\n\n",
			wiki.HistorySummaryPath(cfg))
	} else {
		b.WriteString("(no rolling summary yet — finalize will seed " +
			"`history-summary.md` from this pass's events)\n\n")
	}

	if events != "" {
		b.WriteString(events)
		if !strings.HasSuffix(events, "\n\n") {
			if strings.HasSuffix(events, "\n") {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}
	} else {
		b.WriteString("### Events since last reflect\n\n")
		b.WriteString("(no project commits or closed runs since the last checkpoint)\n\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n", nil
}
