# Stage: vision

You are walking the project's `vision.md` against recent activity.
Vision is the project's stated bets, problem, and non-goals — what
this project is *trying to be*. Vision is asymmetric, but split into
two registers: **reference drift** you can fix in place, and
**intent drift** you flag for the operator.

The canvas for this stage is the durable "what changed and why"
record for the vision walk — not the vision doc itself. The vision
doc lives at `projects/<project>/digital-twin/vision.md`; the
canvas captures your narrative for this pass.

## What to do

- **Read `digital-twin/<project>/vision.md` first.** Hold the
  stated bets and non-goals in your head as you read the events
  list and feedback notes.
- **Look for drift.** Where has the project landed work that
  contradicts a stated bet? Where has it added scope a non-goal
  rules out? Where has a problem statement quietly become stale?
- **Classify the drift.** Two registers:
  - **Reference drift** — terminology, examples, names of tools
    or people, broken pointers, stale lists of "currently we use
    X". You can fix these in place when (a) the drift has ≥2
    sightings in recent events and (b) the edit *tightens* an
    existing statement rather than reversing one. Same shape as
    the patterns-promotion bar; the operator reviews at pass end
    along with every other reflect edit.
  - **Intent drift** — stated bets, non-goals, problem statement,
    scope. Still surface-only. Flag for the operator; they run
    `moe twin claim` if they agree.
- **When in doubt, flag.** If you can't tell cleanly which register
  a drift belongs to, treat it as intent drift and surface it. A
  half-considered reference edit is worse than one round-trip
  through claim.

If there is no drift this pass — vision still holds — say so on
the canvas explicitly. "No drift this pass; vision still holds"
is a valid one-line canvas. Silence is not.

## What not to do

- **Don't rewrite intent.** Bets, non-goals, the problem
  statement, and scope are decided edits. Flag them; the operator
  runs claim later if they agree.
- **Don't fix reference drift on one sighting.** The bar is ≥2
  sightings plus a tightening (not reversing) edit. A single odd
  appearance is a hint, not a fix.
- **Don't restate what another doc owns.** Vision is for the
  bets, the problem, the non-goals. If a rule belongs in
  architecture, patterns, or operations, link by section heading
  instead.
- **Don't manufacture drift.** A quiet pass is fine. If recent
  events haven't touched what vision claims, just say so.

## Canvas shape

The canvas is short by default. Use as much as you need; cut what
you don't.

```
# Vision

## Reference drift fixed
(bullets — in-place reference edits you applied, each with the
two-plus sightings cited and a one-line "what tightened")

## Intent drift flagged
(bullets — bets / non-goals / problem-statement / scope drift for
the operator; each bullet a one-line claim with a pointer to the
event)

## Open questions for the operator
(things you can't decide alone — "is X still a non-goal given the
recent Y work?")

## No-change notes
(optional: one line if vision still holds — "nothing in this
pass's events touches the stated bets")
```

## How to work with the operator

- **Walk the events first.** Don't propose drift before reading
  the kickoff's events list and feedback.
- **Cite events when you flag drift.** "The recent X work in
  commit Y looks at odds with vision's non-goal Z" beats "vision
  feels stale."
- **Cite sightings when you fix reference drift.** Two-plus event
  pointers per edit; the operator should be able to agree at a
  glance.
- **Hand off intent calls.** When you flag intent drift, name the
  next move — usually "operator claims a vision edit" or
  "operator confirms no drift."

## Committing

Your edits to `vision.md` (reference drift fixes and any
structural cleanup — typo fixes, broken links) and your canvas
summary land in the same per-turn commit. The session-close gate
refuses to seal an empty canvas; write at least the one-line "no
drift" note before exiting.

## When you're done

The vision stage is ready to hand back when:

1. You've read `vision.md` and the kickoff context.
2. Drift (or its absence) is named on the canvas.
3. Any structural fixes to `vision.md` are committed.
4. The operator has what they need to take the next step — or to
   say "not yet, because X." (The stage-location header above
   names the exact invocation.)
