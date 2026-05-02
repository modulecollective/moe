package wiki

import (
	"fmt"
	"strings"
)

// PlanPromptSection is the wiki-specific block for a twin plan
// session. Sibling of ReflectPromptSection / ClaimPromptSection: same
// preamble, different framing — synthesise a roadmap ordering from
// inputs the engine inlines in the kickoff, edit roadmap.md only,
// leave the other managed docs alone.
//
// Closed-schema only. Open-schema wikis have no plan-shaped doc, so
// this errors loudly on Open rather than inheriting kb framing.
func PlanPromptSection(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", fmt.Errorf("wiki: plan is closed-schema only (got %s)", cfg.Mode)
	}
	var b strings.Builder
	b.WriteString(wikiPreamble(cfg))
	b.WriteString(`Plan pass (closed-schema):

The operator opened this session to propose or re-propose roadmap.md
against the rest of the twin and the captured idea backlog. The
synthesis context — the four other twin docs, recent project
activity, the open idea backlog, and the current roadmap — has
already been inlined in the kickoff prompt. You don't need to
re-fetch any of it.

Edit roadmap.md only. Vision, architecture, patterns, and operations
are read-only inputs in this session. Drift you spot in those goes
to ` + "`moe twin reflect`" + `; decided edits go to ` + "`moe twin claim`" + ` —
not here.

Roadmap convention: four ` + "`##`" + ` sections — Near term, Mid term, Long
term, Parked. On a fresh roadmap.md (just ` + "`# Roadmap`" + ` and nothing
else), establish the four headings at this pass. On subsequent
passes, walk the prior content with the operator and promote /
demote / retire entries as agreed.

Schema-evolution rules (closed-schema): the doc set is fixed.
Do not create, rename, or delete managed docs.

`)
	if len(cfg.AllowedPrimitives) > 0 {
		fmt.Fprintf(&b, "Allowed primitives: %s.\n", strings.Join(cfg.AllowedPrimitives, ", "))
	} else {
		b.WriteString("Allowed primitives: (none — content edits only).\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}
