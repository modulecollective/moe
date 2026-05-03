package cli

import (
	"path/filepath"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/wiki"
)

// The twin workflow owns the closed-schema digital-twin lifecycle for
// a project. Two operator-facing verbs:
//
//   moe twin reflect <project>  — walk the five managed docs against
//                                 recent activity, fold the idea
//                                 backlog into the roadmap, and clear
//                                 structural hygiene findings (the
//                                 only twin-mutating pass)
//   moe twin claim <project>    — record context for decided edits
//
// Both are out-of-band relative to runs (no canvas, no stage ladder)
// — twin is project-scoped, not run-scoped.

const twinWikiIngestPrompt = `This is the project's closed-schema digital twin.
Five managed docs hold the durable layer: vision, architecture,
patterns, operations, and roadmap. The doc set is fixed; reflect
updates the contents based on observed events, folds the open idea
backlog into the roadmap, and clears structural hygiene findings.
Decided edits (vision pivots, architectural intent) are authored,
recorded via claim, not derived.`

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
}

// twinWikiBuilder is the (root, projectID) → *wiki.Config adapter
// the twin facades call. Closed-schema; ManagedDocs is twin's fixed
// four; AllowedPrimitives is empty (no split / merge / rename /
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

func init() {
	twin := NewWorkflow("twin", "Digital twin: reflect, claim")
	twin.RegisterFacade(reflectCommand("twin", twinWikiBuilder))
	twin.RegisterFacade(claimCommand("twin", twinWikiBuilder))
	RegisterWorkflow(twin)
	Register(twin.Command())
}
