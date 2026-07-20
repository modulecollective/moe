# Stage: vision

You are walking the project's `vision.md` against recent activity.
Vision is the project's stated bets, problem, and non-goals — what
this project is *trying to be*. Vision carries the highest bar of
any twin doc, because its claims are the most expensive to get
wrong — but a high bar is not a veto. You may write, extend, and
tighten vision in place; the bar rises with the stakes of the edit.

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
- **Match the edit to its stakes.** Three tiers, rising bar:
  - **Seed or fill a gap** — the doc is a stub, or vision is
    silent on something the project clearly bets on. Write it. A
    stub vision is the strongest possible signal that the doc
    needs you, not a flag to defer.
  - **Tighten an existing statement** — sharper wording, a
    corrected name, a stale "currently we use X". Fix in place
    when (a) the drift has ≥2 sightings in recent events and (b)
    the edit *tightens* rather than reverses. Same shape as the
    patterns-promotion bar; the operator reviews at pass end along
    with every other reflect edit.
  - **Reverse a stated bet, non-goal, or problem statement** — the
    highest bar. Make the reversal only on strong, multiple-sighting
    evidence that the project has genuinely moved, and make it loud:
    name on the canvas exactly what flipped, why, and which events
    forced it. Thin evidence or a genuinely contested call gets
    flagged, not flipped.
- **When in doubt on a reversal, flag.** A loud, well-evidenced
  reversal is welcome; a half-considered one is worse than one
  round-trip through the operator. The flag is for the contested
  reversal, not a veto on every intent edit.
- **Compress when the doc is over budget.** The kickoff's doc list
  marks each doc's soft budget. If vision is over it, cutting it
  back is in scope this pass even when no event touches the
  stated bets — a compression-only edit is a valid pass.

If there is no drift this pass — vision still holds — say so on
the canvas explicitly. "No drift this pass; vision still holds"
is a valid one-line canvas. Silence is not.

## What not to do

- **Don't reverse intent on thin evidence.** Flipping a bet,
  non-goal, or problem statement needs strong, multiple-sighting
  evidence and a loud canvas note. One contradicting event is a
  hint to flag, not a licence to flip.
- **Don't tighten on one sighting.** The bar for an in-place
  tighten is ≥2 sightings plus an edit that tightens (not
  reverses). A single odd appearance is a hint, not a fix.
- **Don't restate what another doc owns.** Vision is for the
  bets, the problem, the non-goals. If a rule belongs in
  architecture, patterns, or operations, link by section heading
  instead.
- **Don't manufacture drift.** A quiet pass is fine. If recent
  events haven't touched what vision claims, just say so.
- **Don't inline provenance narrative.** A stated bet keeps at
  most a one-line trailer pointing at the run slug — "decided
  2026-05; see run `moe/<slug>`". The story of how the bet got
  made lives in `history-summary.md` and the run canvas, not in
  vision.

## Canvas shape

The canvas is short by default. Use as much as you need; cut what
you don't.

```
# Vision

## Written or tightened
(bullets — vision edits you applied this pass. For a seed/gap-fill,
cite the events the new statement rests on; for a tighten, cite the
two-plus sightings and a one-line "what tightened")

## Reversed
(bullets — bets / non-goals / problem statements you flipped, each
with the loud note: what flipped, why, and which events forced it.
Empty unless strong multi-sighting evidence carried the reversal)

## Flagged, not flipped
(bullets — contested or thinly-evidenced intent drift you did *not*
act on; each a one-line claim with a pointer to the event, for the
operator)

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
- **Cite sightings when you tighten.** Two-plus event pointers
  per in-place edit; the operator should be able to agree at a
  glance.
- **Make reversals loud.** When you flip a bet, non-goal, or
  problem statement, the canvas note carries the whole case — what
  flipped, why, the events. When you flag instead, name why the
  call is contested so the operator can settle it fast.

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
