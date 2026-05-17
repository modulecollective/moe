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
// the seven-stage ladder. Each managed doc gets its own stage canvas
// under `documents/<stage>/content.md`; finalize seals the pass with
// hygiene cleanups, the history-summary fold, and the checkpoint bump.
// The structural kinship with kb lives at the wiki layer (wiki.Config
// + ingest loop), not the workflow layer.
//
// `moe twin claim <project>` is the out-of-band decided-edit recorder
// — it has no stage ladder and isn't part of the twin workflow DAG.

const twinWikiIngestPrompt = `This is the project's closed-schema digital twin.
Six managed docs hold the durable layer: vision, architecture,
patterns, operations, roadmap, and glossary. The doc set is fixed;
reflect updates the contents based on observed events, folds the
open idea backlog into the roadmap, and clears structural hygiene
findings. Decided edits (vision pivots, architectural intent) are
authored, recorded via claim, not derived.`

// twinManagedDocs is the hard-fixed set of managed docs every
// project's twin gets. Names, titles, purposes, and per-doc reflect
// prompts are project-agnostic — closed-schema means "opinions are
// the product." A new doc joins the set the same way a new wiki
// would: a code change here, not per-project config.
var twinManagedDocs = []wiki.ManagedDoc{
	{
		Filename: "vision.md",
		Title:    "Vision",
		Purpose:  "What this project is trying to be — bets, problem, non-goals.",
		ReflectPrompt: "Compare what the project is actually doing against what " +
			"the vision claims. Flag drift; surface gaps where recent work has " +
			"wandered from the stated bets, problems, or non-goals. Do not " +
			"propose a new vision — vision changes are decided edits, not " +
			"observed ones.",
	},
	{
		Filename: "architecture.md",
		Title:    "Architecture",
		Purpose:  "Components, boundaries, load-bearing decisions.",
		ReflectPrompt: "Did recent work introduce, remove, or reshape a " +
			"component or boundary? Did a decision recorded here get " +
			"revisited? Update the structural shape and the decisions list.",
	},
	{
		Filename: "patterns.md",
		Title:    "Patterns",
		Purpose:  "Named patterns and anti-patterns; the project's prose-form eval suite.",
		ReflectPrompt: "Did recent work repeat a shape that should be promoted " +
			"to a named pattern (look for ~3 appearances before promoting)? " +
			"Did it deviate from a recorded pattern in a way that's a " +
			"deliberate choice vs. drift? Did anything get tried and " +
			"rejected — that's a candidate anti-pattern.",
	},
	{
		Filename: "operations.md",
		Title:    "Operations",
		Purpose:  "How the project runs day-to-day — workflows, rituals, tools, escalation paths.",
		ReflectPrompt: "Did recent activity change a workflow, ritual, tool, " +
			"or escalation path? Did anything documented here become no " +
			"longer true? Update the runbook to match how the project " +
			"actually runs.",
	},
	{
		Filename: "roadmap.md",
		Title:    "Roadmap",
		Purpose:  "What's next — prioritized intent across near, mid, long term, and parked.",
		ReflectPrompt: "Flag drift between recent work and the stated " +
			"roadmap: near-term items that look done, near-term lists " +
			"recent work landed nothing on, long-term items now an open " +
			"run, parked items the project is quietly doing anyway. Do " +
			"not rewrite the roadmap — that's the plan verb's job.",
	},
	{
		Filename: "glossary.md",
		Title:    "Glossary",
		Purpose:  "Project-specific vocabulary — terse pointers back to the home doc where each term is anchored.",
		ReflectPrompt: "Walk the glossary against the other managed docs. " +
			"Apply the inclusion bar in the kickoff conventions: a term " +
			"earns an entry when it appears load-bearing in 2+ twin docs, " +
			"or when it names a code seam the twin discusses. Entries are " +
			"1–3 sentences pointing back to the home doc by section " +
			"heading, never line number — definitions live in the home " +
			"doc, the glossary is the index. Retire entries whose term no " +
			"longer appears elsewhere; normalize prose spellings to the " +
			"glossary form when synonyms drift apart.",
	},
}

// twinWikiBuilder is the (root, projectID) → *wiki.Config adapter
// the twin facades call. Closed-schema; ManagedDocs is twin's fixed
// six; AllowedPrimitives is empty (no split / merge / rename /
// retire on a closed-schema wiki).
func twinWikiBuilder(root, projectID string) (*wiki.Config, error) {
	contentDir := filepath.Join(root, "projects", projectID, wiki.TwinDirRel)
	cfg := &wiki.Config{
		Name:              "twin",
		ContentDir:        contentDir,
		ProjectRepoPath:   filepath.Join(root, project.SubmoduleDir(projectID)),
		Project:           projectID,
		BureaucracyPath:   root,
		Mode:              wiki.Closed,
		IngestPrompt:      twinWikiIngestPrompt,
		AllowedPrimitives: nil,
		ManagedDocs:       twinManagedDocs,
	}
	return cfg, nil
}

// twinStageOrder is the canonical ladder for `moe twin reflect`. Six
// per-doc stages walk the managed docs in dependency order — vision /
// architecture set the frame, patterns / operations encode conventions,
// roadmap is planning, glossary is the index that cross-refs everything
// — and finalize seals the pass (hygiene cleanups, history-summary
// fold, checkpoint bump). Exported as a package-level slice so the
// stage entry points and the dispatcher iterate one list.
var twinStageOrder = []string{
	"vision",
	"architecture",
	"patterns",
	"operations",
	"roadmap",
	"glossary",
	"finalize",
}

func init() {
	g := NewCommandGroup("twin", "digital-twin verbs: reflect, vision, architecture, patterns, operations, roadmap, glossary, finalize, claim, close")
	// `moe twin reflect <project>` is the user-facing entry. It mints
	// a fresh run and dispatches the first stage; the chain prompt
	// drives the rest of the ladder.
	g.Register(reflectCommand("twin", twinWikiBuilder))
	// Per-stage entry points (six doc stages plus finalize). Each opens
	// an interactive Claude Code session against the named stage's
	// canvas; the dispatcher behind them (openTwinStage) routes the
	// chain prompt's cascade driver (`!` / `!<stage>` / `!!`). Stage
	// order here matches twinStageOrder so a reordering shows up in
	// one place.
	g.Register(&Command{
		Name:    "vision",
		Summary: "open a Claude Code session on the run's vision-stage canvas",
		Run:     twinStageRun("vision"),
	})
	g.Register(&Command{
		Name:    "architecture",
		Summary: "open a Claude Code session on the run's architecture-stage canvas",
		Run:     twinStageRun("architecture"),
	})
	g.Register(&Command{
		Name:    "patterns",
		Summary: "open a Claude Code session on the run's patterns-stage canvas",
		Run:     twinStageRun("patterns"),
	})
	g.Register(&Command{
		Name:    "operations",
		Summary: "open a Claude Code session on the run's operations-stage canvas",
		Run:     twinStageRun("operations"),
	})
	g.Register(&Command{
		Name:    "roadmap",
		Summary: "open a Claude Code session on the run's roadmap-stage canvas",
		Run:     twinStageRun("roadmap"),
	})
	g.Register(&Command{
		Name:    "glossary",
		Summary: "open a Claude Code session on the run's glossary-stage canvas",
		Run:     twinStageRun("glossary"),
	})
	g.Register(&Command{
		Name:    "finalize",
		Summary: "open a Claude Code session on the run's finalize-stage canvas — clear hygiene findings, fold events, seal the pass",
		Run:     twinStageRun("finalize"),
	})
	// Claim stays out-of-band: no run.json, no stage ladder, just the
	// decided-edit recorder.
	g.Register(claimCommand("twin", twinWikiBuilder))
	// Close marks the in-progress twin run terminal once finalize has
	// landed. Mirrors `moe sdlc close`: no cleanup hook, since twin
	// runs have no sandbox.
	g.Register(closeCommand("twin", "Close twin reflect pass %s %s", nil))
	RegisterGroup(g)

	w := NewWorkflow("twin")
	w.RegisterStage("vision")
	w.RegisterStage("architecture", "vision")
	w.RegisterStage("patterns", "architecture")
	w.RegisterStage("operations", "patterns")
	w.RegisterStage("roadmap", "operations")
	w.RegisterStage("glossary", "roadmap")
	w.RegisterStage("finalize", "glossary")
	// Finalize is the working-stage equivalent of sdlc's test: anti-
	// theater on the canvas (both `What I fixed` and `What I left`
	// must have substantive content) plus a re-scan of the wiki for
	// leftover hygiene findings. The work-turn check still gates entry;
	// this gate decides whether the committed turn was substantive.
	w.RegisterStageGate("finalize", finalizeStageGate)
	RegisterWorkflow(w)
}
