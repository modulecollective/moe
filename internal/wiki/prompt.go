package wiki

import (
	"fmt"
	"strings"
)

// IngestPromptSection is the wiki-specific block that gets appended to
// the system prompt for an ingest session. It carries the schema-config
// body, the on-disk shape contract, and the mode-specific rules so the
// agent knows which primitives it may apply during this turn.
//
// The section is one cohesive markdown block — the caller layers it
// into the prompt alongside soul.md, the stage fragment, and the
// operational core via the same `\n---\n\n` separator buildSystemPrompt
// uses for the rest of the prompt.
func IngestPromptSection(cfg Config) string {
	var b strings.Builder

	if body := strings.TrimSpace(cfg.IngestPrompt); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "## Wiki: %s (%s-schema)\n\n", cfg.Name, cfg.Mode)
	fmt.Fprintf(&b, "Wiki content directory:\n  %s\n\n", cfg.ContentDir)

	b.WriteString(`On-disk shape:
- index.md — corpus catalog. The authority on grouping; topic docs
  are flat under the wiki dir, sections in index.md provide the
  taxonomy.
- log.md — append-only changelog. The engine writes entries at the
  end of each ingest; do not edit it yourself.
- checkpoint.json — last-ran SHAs. Engine-managed; do not edit.
- <topic>.md — one file per topic, flat under the wiki dir. Topic
  identity is decoupled from run identity; a single ingest may
  update zero, one, or many topic docs.

`)

	switch cfg.Mode {
	case Open:
		b.WriteString(`Schema-evolution rules (open-schema):

You may evolve the doc set under the four primitives below. Maintain
index.md as content moves; maintain cross-links between topic docs.
Do not edit log.md or checkpoint.json — the engine writes those.

- **split** — when one topic doc covers two distinct things and a
  reader looking for one would have to skim past the other. Evidence:
  the doc has two top-level sections that don't share vocabulary; the
  index entry already strains to describe both. *Not for length alone.*
- **merge** — when two docs cover the same ground and a reader would
  have to read both to understand either. Evidence: substantial
  overlap in claims and sources, near-identical scope statements. *Not
  for "they're related."*
- **rename** — when the title no longer matches what the doc has
  drifted into. Evidence: the doc's opening sentences contradict its
  filename or index entry. *Not for cosmetic improvements.*
- **retire** — when nothing else in the wiki references the doc and
  its content is either fully absorbed elsewhere or no longer
  load-bearing. Evidence: zero inbound links, claims either obsolete
  or duplicated. *Not as a substitute for merging.*

Name what you did. As you apply a primitive, append one line to the
engine's stash file before the per-turn commit, in this exact shape:

    [wiki-op] split <src>.md → <dst1>.md, <dst2>.md
    [wiki-op] merge <src>.md into <dst>.md
    [wiki-op] rename <old>.md → <new>.md
    [wiki-op] retire <doc>.md

Stash file: ` + OpsStashPath(cfg.ContentDir) + `

The engine harvests these tags into log.md and truncates the stash at
session close. The stash never appears in diffs — it's engine-managed
scratch.

`)
	case Closed:
		b.WriteString(`Schema-evolution rules (closed-schema):
The doc set is fixed. Do not create, rename, or delete topic docs unless
the operator has explicitly authorized that change in this session.
Edits land inside the existing topic docs and index.md.

`)
	}

	if len(cfg.AllowedPrimitives) > 0 {
		fmt.Fprintf(&b, "Allowed primitives: %s.\n", strings.Join(cfg.AllowedPrimitives, ", "))
	} else {
		b.WriteString("Allowed primitives: (none — content edits only).\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}
