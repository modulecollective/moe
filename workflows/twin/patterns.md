# Stage: patterns

You are walking the project's `patterns.md` — the named patterns
and anti-patterns the project has crystallised. Patterns is the
project's prose-form eval suite: shapes that have proven worth
naming, and shapes that have proven worth refusing.

The canvas captures your narrative for the walk; the doc itself
is what you edit.

## What to do

- **Read `digital-twin/<project>/patterns.md` first.** Hold the
  named patterns and anti-patterns in your head before walking
  events.
- **Promote when you see ~3 appearances.** A shape that recurs
  three times in unrelated work has earned a name. Promote it
  with a terse pattern entry and a cited example or two.
- **Demote or retire stale entries.** A named pattern nothing has
  used in a long time, or that recent work has actively avoided,
  is a candidate to retire or rephrase.
- **Surface anti-patterns.** Something that got tried and rejected
  in recent work is a candidate anti-pattern. Name what was tried,
  why it failed, and what to do instead.
- **Cross-reference architecture.** If the architecture stage
  added a component, check whether its usage repeats an existing
  pattern (or implies a new one).

If patterns hasn't moved this pass, say so. Pattern promotion is
deliberately slow — a quiet pass is healthy.

## What not to do

- **Don't promote on one sighting.** Two appearances is a hint;
  three is a pattern. Premature naming creates dead entries.
- **Don't theorise.** Patterns name what the project *does*, not
  what it *should* do. Speculation lives in feedback notes for
  the operator, not in `patterns.md`.
- **Don't restate what another doc owns.** Patterns owns the
  named shape and its rule. The boundary or component the shape
  involves lives in architecture; the ritual that exercises it
  lives in operations. Link by section heading instead of
  duplicating the rule here.

## Canvas shape

```
# Patterns

## Promoted this pass
(bullets — new pattern entries, with the three-or-more cited
sightings)

## Demoted or retired
(bullets — entries removed or downgraded, with the reason)

## Anti-patterns surfaced
(bullets — shapes recent work rejected and why)

## No-change notes
(optional one line if patterns still holds)
```

## How to work with the operator

- **Show the sightings.** When you promote, list the events or
  files where the shape appears. The operator should be able to
  agree at a glance.
- **Quote the rejection.** Anti-patterns earn their entries when
  you can point at a place the project deliberately *didn't*
  take that shape.

## Committing

`patterns.md` edits and the canvas summary land in one per-turn
commit. The session-close gate refuses an empty canvas.

## When you're done

1. You've read `patterns.md` and the kickoff context.
2. Promotions, demotions, and anti-patterns (or their absence)
   are named on the canvas with sightings cited.
3. Edits to `patterns.md` are committed.
4. The operator has what they need to take the next step.

## Before you start

Skim the architecture-stage canvas for this run. Component or
boundary changes often imply pattern changes — fold them in here.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
