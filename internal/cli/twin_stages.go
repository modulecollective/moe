package cli

import (
	"errors"
	"flag"
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
// is baked into the closure so the dispatch table in twin.go stays a
// thin list of (name, Run) pairs.
func twinStageRun(stage string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("twin "+stage, flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.Usage = func() {
			moePrintf(stderr, "usage: moe twin %s <project>/<run>\n", stage)
			moePrintln(stderr, "")
			moePrintf(stderr, "Opens an interactive Claude Code session on the %s-stage canvas\n", stage)
			moePrintln(stderr, "of a twin reflect run. First use on a run creates the document;")
			moePrintln(stderr, "re-runs resume the session.")
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
			moePrintf(stderr, "twin %s: %v\n", stage, err)
			return 2
		}
		return openTwinStageInteractive(stage, projectID, runID, stdout, stderr)
	}
}

// openTwinStageInteractive opens the named twin stage interactively
// (the operator typed `moe twin <stage> <project> <run>`). Runs the
// shared per-stage validation (run exists, twin workflow, prior canvas
// present where required), then dispatches into runStageSession with
// the twin-specific options. headless=false unconditionally — the
// matching headless entry is openTwinStage, which the chain prompt /
// cascade driver reaches.
func openTwinStageInteractive(stage, projectID, runID string, stdout, stderr io.Writer) int {
	if code := requireTwinRun("twin "+stage, projectID, runID, stderr); code != 0 {
		return code
	}
	if err := requireTwinPriorCanvas(stage, projectID, runID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	return runTwinStageSession(stage, projectID, runID, false, false, stdout, stderr)
}

// openTwinStage is the Go-level seam behind the chain prompt's
// cascade driver (`!` / `!<stage>` / `!!`) for twin runs.
// Identical contract to openSdlcStage one workflow over: switch on the
// stage name, hand to the right helper, run headless, and carry
// suppressNextStage through to stageSessionOpts.SkipNextStage. A
// `default` branch surfaces an unknown-stage call as a stderr line
// rather than
// silently routing somewhere wrong.
//
// Declared as a var and assigned in init() so the static reference
// chain doesn't trip Go's init-order cycle checker — same shape
// openSdlcStage uses, same reason.
var openTwinStage func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int

func init() {
	openTwinStage = func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		switch stage {
		case "vision", "architecture", "patterns", "operations", "roadmap", "glossary", "finalize":
			if code := requireTwinRun("twin "+stage, projectID, runID, stderr); code != 0 {
				return code
			}
			if err := requireTwinPriorCanvas(stage, projectID, runID); err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			return runTwinStageSession(stage, projectID, runID, true, suppressNextStage, stdout, stderr)
		default:
			moePrintf(stderr, "twin: openTwinStage: unknown stage %q\n", stage)
			return 1
		}
	}
	// Register the dispatcher so generic chain-prompt / cascade code
	// can reach twin stages via workflow name, not via a hardcoded
	// switch on "sdlc".
	registerHeadlessDispatcher("twin", func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int {
		return openTwinStage(stage, projectID, runID, suppressNextStage, stdout, stderr)
	})
}

// runTwinStageSession wraps runStageSession with the twin-specific
// options the seven stages share: closed-schema wiki builder, no
// sandbox (document-only), and SkipFinalize=true for every stage *but*
// finalize. Finalize additionally wires the post-flight hygiene gate
// (the engine refuses to seal the pass with leftover findings) and a
// canvas skeleton.
//
// The pass-scoped kickoff context (events, hygiene findings, twin
// feedback, idea backlog, history summary) is read off the canonical
// bureaucracy root once per stage open and folded into the
// InitialPrompt. Each stage sees the same payload — the design's "no
// in-session iteration across docs, every stage reads the same
// events list" contract.
func runTwinStageSession(stage, projectID, runID string, headless bool, suppressNextStage bool, stdout, stderr io.Writer) int {
	kickoff, err := buildTwinStageKickoff(stage, projectID, headless)
	if err != nil {
		moePrintf(stderr, "twin %s: %v\n", stage, err)
		return 1
	}
	opts := stageSessionOpts{
		Headless:      headless,
		SkipNextStage: suppressNextStage,
		InitialPrompt: kickoff,
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

// buildTwinStageKickoff renders the per-stage InitialPrompt: a brief
// greeting (interactive only) plus the pass-scoped context block.
// Headless mode skips the greeting — the oneshot.md fragment already
// tells the agent there's no operator on stdin.
func buildTwinStageKickoff(stage, projectID string, headless bool) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return "", err
	}
	cfg, err := twinWikiBuilder(root, projectID)
	if err != nil {
		return "", err
	}
	ctx, err := reflectKickoffContext(root, projectID, *cfg)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if !headless {
		b.WriteString(twinStageGreeting(stage))
		b.WriteString("\n")
	}
	b.WriteString(ctx)
	return b.String(), nil
}

// twinStageGreeting is the one- or two-sentence framing that opens an
// interactive twin stage session — the same "acknowledge where things
// stand, then wait for the operator" shape openSdlcDesign and friends
// use.
func twinStageGreeting(stage string) string {
	return fmt.Sprintf(
		"The operator just opened the %s stage of a twin reflect pass. "+
			"Read the pass-context block below and the canvas file before "+
			"replying. In one or two sentences, acknowledge where the walk "+
			"stands (fresh start vs. resumed) and what you'd touch on this "+
			"stage. Then wait for the operator's go-ahead.\n",
		stage,
	)
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

(agent fills: the paragraph(s) appended to history-summary.md this pass — a prose compression of the events block, slow-growing, signal-preserving)
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

// reflectKickoffContext returns the per-stage kickoff payload that
// gets prepended to each stage's system prompt: events, history
// summary, twin feedback, hygiene findings, and the open idea backlog
// (for roadmap). The whole block carries pass-scoped context every
// stage needs to see; the per-stage framing lives in the stage
// fragment.
//
// Returns the rendered markdown block plus any error from the
// underlying wiki / git reads. An empty block is valid (a fresh
// project with no events, no feedback, and a clean scan).
func reflectKickoffContext(root, projectID string, cfg wiki.Config) (string, error) {
	events, err := wiki.EventsSinceCheckpoint(cfg)
	if err != nil {
		return "", fmt.Errorf("wiki: events: %w", err)
	}
	historySummary, err := wiki.ReadHistorySummary(cfg)
	if err != nil {
		return "", fmt.Errorf("wiki: history summary: %w", err)
	}
	ideas, err := loadIdeaBacklog(root, projectID)
	if err != nil {
		return "", fmt.Errorf("wiki: ideas: %w", err)
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
		"each successor stage — the events list, the hygiene findings, " +
		"the workflow feedback, and (for the roadmap stage) the open idea " +
		"backlog all carry pass-scoped context.\n\n")

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
		b.WriteString("Notes that non-twin workflow agents left for this " +
			"reflect pass. Treat as input, not direction — fold what's " +
			"real into the relevant managed doc on the appropriate " +
			"stage, set aside what isn't.\n\n")
		for _, fb := range feedback {
			title := fb.runTitle
			if title == "" {
				title = fb.runID
			}
			fmt.Fprintf(&b, "#### %s — %s (%s)\n\n", fb.runID, title, fb.when.Format("2006-01-02"))
			body := strings.TrimSpace(fb.body)
			if body == "" {
				b.WriteString("(empty feedback file)\n\n")
				continue
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("### Idea backlog\n\n")
	if len(ideas) == 0 {
		b.WriteString("(no open ideas captured for this project)\n\n")
	} else {
		b.WriteString("Open idea runs for this project. The roadmap stage " +
			"folds these into the roadmap; earlier stages may reference " +
			"them for context.\n\n")
		for _, idea := range ideas {
			fmt.Fprintf(&b, "#### %s — %s\n\n", idea.slug, idea.title)
			body := strings.TrimSpace(idea.body)
			if body == "" {
				b.WriteString("(empty canvas)\n\n")
				continue
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("### History summary\n\n")
	if historySummary != "" {
		b.WriteString(historySummary)
		b.WriteString("\n\n")
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
