---
name: moe-twin
description: The writing contract for a project's digital twin (projects/<project>/digital-twin/ — vision, architecture, patterns, operations, glossary). Read before editing any of those five docs: what each one owns, how much license you have to change what's there, and the compression discipline that keeps them worth reading. Use when your run's evidence has moved the project's recorded intent, or when a stage prompt points you here.
---

# Writing the digital twin

The twin is the project's durable layer of intent: what it is trying to
be, how it is shaped, what shapes it has named or refused, how it runs.
Code is the implementation; the twin is the intent. When the two
disagree, the twin wins until a run updates it — and this is how a run
updates it.

You edit the docs in place, under `projects/<project>/digital-twin/`.
Your stage's per-turn commit picks them up alongside the canvas, so
there is no separate landing step: write the edit, mention it on your
canvas, and it ships with the turn. On the way out the stage scans the
tree for structural breakage (broken links, empty docs, dangling
cross-references) and refuses to close on findings — so a citation you
invent is a problem you fix this turn, not one a future run inherits.

## The five docs

The doc set is fixed. Never add, rename, or retire a file.

- **vision.md** — what the project is trying to be; the bets, the
  problem, the non-goals.
- **architecture.md** — components, boundaries, load-bearing decisions.
- **patterns.md** — named patterns and anti-patterns; the project's
  prose-form eval suite.
- **operations.md** — how the project runs day-to-day: workflows,
  rituals, tools, escalation paths.
- **glossary.md** — project-specific vocabulary; terse entries pointing
  back to the home doc where each term is anchored.

## How twin docs are written

Twin docs are written for a reader who doesn't already know the
project. **Primer-plus-reference, not changelog.** Current state only:
durable rules, terse prose, examples in service of the rule. Every
stage's recommended first read is this twin, so every kilobyte is a tax
on every stage — write for the reader who has to pay it.

**Single home per rule.** Each rule, principle, or named shape has one
home. If it already lives in another managed doc, point there by section
heading instead of restating it. Architecture owns shape and boundaries;
patterns owns named recurring or refused shapes; operations owns rituals
and tools; glossary is the index, not a second definition surface. A
line that could plausibly live in two docs lives in one — pick by which
doc the reader would search first.

**Provenance is a reference, not a retelling.** A decision keeps its
rule plus at most a one-line trailer naming when and where — "decided
2026-05; see run `moe/<slug>`". The story behind it lives in the run
canvases and the journal, both of which this bureaucracy already keeps.
Do not inline it a second time.

**You have license to drop.** When a section describes a transition that
has finished, delete it. When a feature was tried and removed, the rule
it taught stays (YAGNI in this corner, surface area earned scrutiny,
this kind of speculation didn't pay); the section about the removed
feature does not. Extract the rule, drop the example.

**Compression is a valid edit.** There is no right size — only smaller
while still serving the reader. A turn whose only twin work was cutting
a doc back has done real twin work. When you add, look for what the
addition lets you drop.

## How much license you have

The tier depends on what you're doing to what's already there.

- **Filling a gap** — the docs are silent on something your run
  established. Write it. No ceremony; that's the twin catching up.
- **Tightening on evidence** — the rule is right but the wording has
  drifted from what the code does, or the section has bloated past its
  point. Rewrite it, and cite what you saw.
- **Reversing a stated bet** — the twin records a decision and your run
  contradicts it. Do it loudly: name the conflict on your canvas, say
  what changed the answer, and rewrite the rule to current state rather
  than stacking an amendment under the old one. A reversal nobody
  narrates is how the twin stops being trusted.

Below all three: don't speculate. The twin records what the project
*does* and has *decided*, not what it might. A future component nobody
is building doesn't belong in architecture; a shape seen once isn't a
pattern (three unrelated sightings is the bar). If your evidence is
thin, or the call is the operator's to make rather than yours, that's a
note in `feedback/twin.md` — see the `moe-bureaucracy` skill — not a
twin edit.

## Scope discipline

A twin edit rides your run; it doesn't become your run. Record what
*this* change established and stop. Rewriting three docs because you
were in the neighbourhood makes your diff unreviewable and buries the
one paragraph that mattered. If you spot twin work worth doing that
isn't yours, file it in `feedback/twin.md` and move on.
