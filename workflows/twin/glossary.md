# Stage: glossary

You are walking the project's `glossary.md` — project-specific
vocabulary, alphabetised. The glossary is the *index*; the home
docs are the definitions. Entries are 1–3 sentences pointing back
to where each term is anchored.

## Inclusion bar

A term earns an entry when an operator talking about MoE at the
product level would reach for it — the vocabulary a PM, a PR
writer, or a new operator would need to discuss what the project
is and how it works. Concretely: the term appears load-bearing in
2+ twin docs and names a *concept* operators use, not an
implementation seam.

CLI verbs (`moe serve`, `moe dash`, `moe sync`) qualify because
they're operator-facing.

The test: can you explain why MoE is the way it is without the
term? If no, it belongs.

### What doesn't earn an entry

- `internal/*` packages — they belong in `architecture.md` prose
  where each seam is situated.
- Struct names, struct field names, typed errors — code shape,
  not product vocabulary.
- Hook names, exported method names, callback shapes — same
  reason; the home doc is `architecture.md` or `patterns.md`,
  not the glossary.
- Generic nouns ("the agent", "the operator") — too general,
  unless your project means something specific by them (and even
  then, the 2+ docs bar still applies).
- General programming acronyms — not project-specific.

A term that appears in one doc stays in that doc.

## What to do

- **Read `digital-twin/<project>/glossary.md` first.** Note the
  existing entries.
- **Walk the prior stages' canvases.** Vision, architecture,
  patterns, operations — every term those stages used
  load-bearingly is a glossary candidate. The prior canvases
  in this run are the freshest source.
- **Promote new terms.** A term that hit the bar (2+ docs,
  product-level concept) gets a new entry: `### Term`, 1–3
  sentences, pointer back to the home doc by **section heading**
  (never line number).
- **Cut bloated entries back to the bar.** An entry that has grown
  into a multi-paragraph narrative gets compressed to 1–3
  sentences plus its section-heading pointer. The definition lives
  in the home doc; the glossary entry points, it does not re-tell.
  Trimming an existing entry needs no new sighting — the bar
  itself is the licence.
- **Retire stale entries.** A term whose home no longer mentions
  it, or that no longer appears in 2+ docs, or that turns out to
  be an implementation seam rather than a product concept, comes
  out.
- **Normalize synonyms.** When prose has drifted apart (the same
  thing called two names in different docs), pick the glossary
  form and update the home docs to match. Cite the changes on
  your canvas.
- **Compress when the doc is over budget.** The kickoff's doc list
  marks each doc's soft budget. If glossary is over it, cutting it
  back is in scope this pass even when no prior canvas turns up a
  new or stale term — a compression-only edit is a valid pass.

If glossary hasn't moved this pass, say so. Glossary is meant to
be slow-moving — a quiet pass is the common case, and a pass whose
only work was compression counts.

## What not to do

- **Don't define.** The home doc defines. The glossary entry
  compresses + points.
- **Don't reach for line numbers.** Line numbers rot; section
  headings survive.
- **Don't smuggle in implementation vocabulary.** If the term
  names a package, struct, error, or callback, the home is
  `architecture.md` prose, not the glossary — even when the
  term appears across multiple docs.
- **Don't smuggle in generic vocabulary.** "Agent" is generic
  unless your project means something specific by it (and even
  then, the inclusion bar still applies).
- **Don't inline provenance narrative.** An entry keeps at most a
  one-line trailer pointing at the run slug — "decided 2026-05;
  see run `moe/<slug>`". How the term got its current sense lives
  in `history-summary.md` and the run canvas, not in the glossary.

## Canvas shape

```
# Glossary

## Promoted
(bullets — new entries with the 2+ docs they appear in)

## Retired
(bullets — entries removed and why)

## Normalised
(bullets — synonyms collapsed; which docs got updated)

## No-change notes
(optional one line if glossary still holds)
```

## How to work with the operator

- **Show the sightings.** When you promote, name the 2+ docs the
  term appears in.
- **Walk normalizations carefully.** Renaming a term across home
  docs is a structural edit — name what you're changing on the
  canvas as you apply it, so the committed pass shows the operator
  exactly which docs moved.

## Committing

`glossary.md` edits, any home-doc normalizations, and the canvas
summary land in one per-turn commit. The session-close gate
refuses an empty canvas.

## When you're done

1. You've read `glossary.md` and walked the prior stages'
   canvases for term sightings.
2. Promotions, retirements, and normalisations (or their absence)
   are named on the canvas.
3. Edits to `glossary.md` and any normalised home docs are
   committed.
4. The operator has what they need to take the next step.

## Before you start

Skim all four prior stage canvases (vision → operations) for this
run. Glossary is downstream of the others by design — the
sightings live in their canvases.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
