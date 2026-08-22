package cli

import (
	"path/filepath"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/wiki"
)

// `moe twin` is the top-level verb group for the closed-schema digital
// twin lifecycle.
//
// `moe twin reflect <project>` is the operator-facing entry: it mints
// a fresh `reflect-<timestamp>` run and dispatches the first stage of
// the six-stage ladder. Each managed doc gets its own stage canvas
// under `documents/<stage>/content.md`; finalize seals the pass with
// hygiene cleanups, the history-summary fold, and the checkpoint bump.
// The wiki engine (wiki.Config + ingest loop) is the twin's alone
// now that kb is gone.

// twinWikiIngestPrompt is the schema-config body wikiPreamble lays
// down at the top of every reflect stage's system prompt — the one
// block all five doc stages and finalize receive. It carries the
// writing contract, because that contract has to reach the agent on
// the live path: a twin doc's failure mode is not "wrong", it's
// "bloated", and the guide that only ever licenses adding is how the
// twin got to ~380 KB.
const twinWikiIngestPrompt = `This is the project's closed-schema digital twin.
Five managed docs hold the durable layer: vision, architecture,
patterns, operations, and glossary. The doc set is fixed;
reflect updates the contents based on observed events and clears
structural hygiene findings. Decided edits (vision pivots,
architectural intent) are confirmed by the operator in an
interactive reflect session, not derived.

## How twin docs are written

Twin docs are written for a reader who doesn't already know the
project. **Primer-plus-reference, not changelog.** Current state
only: durable rules, terse prose, examples in service of the rule.
Every stage's recommended first read is this twin, so every
kilobyte is a tax on every stage — write for the reader who has to
pay it.

**Provenance is a reference, not a retelling.** A decision keeps
its rule plus at most a one-line trailer naming when and where —
"decided 2026-05; see run ` + "`moe/<slug>`" + ` for the case". The
narrative behind it lives in ` + "`history-summary.md`" + ` and the run
canvases, both of which this bureaucracy already keeps. Do not
inline the story a second time.

**You have license to drop.** When a section describes a
transition that has finished, delete it. When a feature was tried
and removed, the rule it taught stays (YAGNI in this corner,
surface area earned scrutiny, this kind of speculation didn't
pay); the section about the removed feature does not. Extract the
rule, drop the example. Compress over preserve.

**Compression is a valid pass.** Each doc's current size is rendered
next to it in the on-disk shape list below. There is no right size —
only smaller while still serving the reader. Every doc is a
compression candidate every pass, touched by events or not; the sizes
tell you where cutting pays most. A pass whose only edit was cutting a
doc back is a real pass, not an empty one. When you add, look for what
the addition lets you drop.

**Single home per rule.** Each rule, principle, or named shape
has one home. If it already lives in another managed doc, point
there by section heading instead of restating it. Architecture
owns shape and boundaries; patterns owns named recurring or
refused shapes; operations owns rituals and tools; glossary is
the index, not a second definition surface. A line that could
plausibly live in two docs lives in one — pick by which doc the
reader would search first.`

// twinManagedDocs is the hard-fixed set of managed docs every
// project's twin gets. Names, titles, and purposes are
// project-agnostic — closed-schema means "opinions are
// the product." A new doc joins the set the same way a new wiki
// would: a code change here, not per-project config.
var twinManagedDocs = []wiki.ManagedDoc{
	{
		Filename: "vision.md",
		Title:    "Vision",
		Purpose:  "What this project is trying to be — bets, problem, non-goals.",
	},
	{
		Filename: "architecture.md",
		Title:    "Architecture",
		Purpose:  "Components, boundaries, load-bearing decisions.",
	},
	{
		Filename: "patterns.md",
		Title:    "Patterns",
		Purpose:  "Named patterns and anti-patterns; the project's prose-form eval suite.",
	},
	{
		Filename: "operations.md",
		Title:    "Operations",
		Purpose:  "How the project runs day-to-day — workflows, rituals, tools, escalation paths.",
	},
	{
		Filename: "glossary.md",
		Title:    "Glossary",
		Purpose:  "Project-specific vocabulary — terse pointers back to the home doc where each term is anchored.",
	},
}

// twinWikiBuilder is the (root, projectID) → *wiki.Config adapter
// the twin facades call. ManagedDocs is twin's fixed five.
func twinWikiBuilder(root, projectID string) (*wiki.Config, error) {
	contentDir := filepath.Join(root, "projects", projectID, wiki.TwinDirRel)
	cfg := &wiki.Config{
		Name:            "twin",
		ContentDir:      contentDir,
		ProjectRepoPath: filepath.Join(root, project.SubmoduleDir(projectID)),
		Project:         projectID,
		BureaucracyPath: root,
		IngestPrompt:    twinWikiIngestPrompt,
		ManagedDocs:     twinManagedDocs,
	}
	return cfg, nil
}

// twinStageOrder is the canonical ladder for `moe twin reflect`. Five
// per-doc stages walk the managed docs in dependency order — vision /
// architecture set the frame, patterns / operations encode conventions,
// glossary is the index that cross-refs everything — and finalize seals
// the pass (hygiene cleanups, history-summary fold, checkpoint bump).
// Exported as a package-level slice so the stage entry points and the
// dispatcher iterate one list.
var twinStageOrder = []string{
	"vision",
	"architecture",
	"patterns",
	"operations",
	"glossary",
	"finalize",
}

func init() {
	g := NewCommandGroup("twin", "digital-twin verbs")
	// `moe twin reflect <project>` is the user-facing entry. It mints
	// a fresh run and dispatches the first stage; the chain prompt
	// drives the rest of the ladder.
	g.Register(reflectCommand("twin", twinWikiBuilder))
	// Per-stage entry points (five doc stages plus finalize). Each opens
	// an interactive agent session against the named stage's
	// canvas; the dispatcher behind them (openTwinStage) routes the
	// chain prompt's cascade driver (`!` / `!<stage>` / `!!` / `!!!`). Stage
	// order here matches twinStageOrder so a reordering shows up in
	// one place.
	g.Register(&Command{
		Name:    "vision",
		Summary: "open an agent session on the run's vision-stage canvas",
		Run:     twinStageRun("vision"),
	})
	g.Register(&Command{
		Name:    "architecture",
		Summary: "open an agent session on the run's architecture-stage canvas",
		Run:     twinStageRun("architecture"),
	})
	g.Register(&Command{
		Name:    "patterns",
		Summary: "open an agent session on the run's patterns-stage canvas",
		Run:     twinStageRun("patterns"),
	})
	g.Register(&Command{
		Name:    "operations",
		Summary: "open an agent session on the run's operations-stage canvas",
		Run:     twinStageRun("operations"),
	})
	g.Register(&Command{
		Name:    "glossary",
		Summary: "open an agent session on the run's glossary-stage canvas",
		Run:     twinStageRun("glossary"),
	})
	g.Register(&Command{
		Name:    "finalize",
		Summary: "open an agent session on the run's finalize-stage canvas — clear hygiene findings, fold events, seal the pass",
		Run:     twinStageRun("finalize"),
	})
	// Close marks the in-progress twin run terminal once finalize has
	// landed. No cleanup hook — same contract as chat: twin stages do
	// open a sandbox clone, but a read-only one nothing pushes, so it
	// waits for `moe clone gc` rather than a bespoke teardown.
	g.Register(closeCommand("twin", "Close twin reflect pass %s/%s", nil))
	g.Register(harvestCommand("twin"))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (twin cat <project>/<run> <stage>)",
		Run:     runCat("twin", ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (twin log <project>/<run> <stage>)",
		Run:     runLog("twin", ""),
	})
	RegisterGroup(g)

	w := NewWorkflow("twin")
	w.RegisterStage("vision")
	w.RegisterStage("architecture", "vision")
	w.RegisterStage("patterns", "architecture")
	w.RegisterStage("operations", "patterns")
	w.RegisterStage("glossary", "operations")
	w.RegisterStage("finalize", "glossary")
	// Finalize is the working-stage equivalent of sdlc's test: anti-
	// theater on the canvas (both `What I fixed` and `What I left`
	// must have substantive content) plus a re-scan of the wiki for
	// leftover hygiene findings. The work-turn check still gates entry;
	// this gate decides whether the committed turn was substantive.
	w.RegisterStageGate("finalize", finalizeStageGate)
	RegisterWorkflow(w)

	// Serve declaration: let the web mark twin stages advanced (every
	// registered stage verb, no exclusions) and render the close chip.
	// Not a tag destination — a twin pass is minted by `moe twin
	// reflect`, not promoted from an idea.
	registerServeWorkflow("twin", serveWorkflowDecl{})
}
