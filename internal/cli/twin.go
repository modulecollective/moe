package cli

import (
	"github.com/modulecollective/moe/internal/wiki"
)

// twinManagedDocs is the hard-fixed set of managed docs every
// project's twin gets. Names, titles, and purposes are
// project-agnostic — closed-schema means "opinions are
// the product." A new doc joins the set the same way it always would:
// a code change here, not per-project config.
//
// One reader now that the reflect ladder is gone: the project-doc
// hygiene gate, which scans the twin against this list after any stage
// commit that touched it. The writing contract the ladder's per-stage
// fragments used to carry lives in the moe-twin skill.
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
