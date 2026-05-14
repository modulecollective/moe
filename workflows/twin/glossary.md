# Stage: glossary

You are walking the project's `glossary.md` — project-specific
vocabulary, alphabetised. The glossary is the *index*; the home
docs are the definitions. Entries are 1–3 sentences pointing back
to where each term is anchored.

## Inclusion bar

A term earns an entry when **either**:

- it appears load-bearing in **2+ twin docs**, **or**
- it names a **code seam** the twin discusses — a package,
  command, struct, or commit trailer.

A term that appears in one doc stays in that doc. Generic nouns
("the agent", "the operator") do not earn entries. Acronyms
follow the same rule: project-specific short forms belong;
general programming acronyms do not.

## What to do

- **Read `digital-twin/<project>/glossary.md` first.** Note the
  existing entries.
- **Walk the prior stages' canvases.** Vision, architecture,
  patterns, operations, roadmap — every term those stages used
  load-bearingly is a glossary candidate. The prior canvases
  in this run are the freshest source.
- **Promote new terms.** A term that hit the bar in 2+ docs gets
  a new entry: `### Term`, 1–3 sentences, pointer back to the
  home doc by **section heading** (never line number).
- **Retire stale entries.** A term whose home no longer mentions
  it, or that no longer appears in 2+ docs, comes out.
- **Normalize synonyms.** When prose has drifted apart (the same
  thing called two names in different docs), pick the glossary
  form and update the home docs to match. Cite the changes on
  your canvas.

If glossary hasn't moved this pass, say so. Glossary is meant to
be slow-moving — a quiet pass is the common case.

## What not to do

- **Don't define.** The home doc defines. The glossary entry
  compresses + points.
- **Don't reach for line numbers.** Line numbers rot; section
  headings survive.
- **Don't smuggle in generic vocabulary.** "Agent" is generic
  unless your project means something specific by it (and even
  then, the inclusion bar still applies).

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
  term appears in (or the code seam it names).
- **Walk normalizations carefully.** Renaming a term across home
  docs is a structural edit — surface what you're changing
  before applying.

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

Skim all five prior stage canvases (vision → roadmap) for this
run. Glossary is downstream of the others by design — the
sightings live in their canvases.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
