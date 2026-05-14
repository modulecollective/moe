# Stage: roadmap

You are walking the project's `roadmap.md` against recent activity
and the open idea backlog. Roadmap is prioritised intent across
near, mid, long term, and parked — what's actually next.

## What to do

- **Read `digital-twin/<project>/roadmap.md` first.** Hold the
  four `##` sections (Near term, Mid term, Long term, Parked) in
  your head as you walk events and ideas.
- **Reconcile near-term items.** A near-term item that recent
  work landed against is done — move it to a "## Recently
  landed" line or remove it. A near-term item the project has
  quietly stopped working on is a candidate to demote or park.
- **Promote ideas the project has implicitly committed to.** Open
  ideas (`moe idea list`) that the operator has greenlit, or that
  recent work has started, belong on the roadmap. Promote them at
  the right horizon.
- **Demote or park unfunded items.** A roadmap item the project
  is *not* doing — and won't be soon — belongs in Parked with a
  reason.
- **Fold parked items the project is quietly doing.** If recent
  work touched something Parked, surface that — either remove from
  Parked (now near/mid term) or note the drift.

If roadmap hasn't moved this pass, say so. A quiet roadmap pass
is healthy if the prior reflect was recent.

## Fresh roadmaps

On a freshly-stubbed `roadmap.md` (just the H1 and nothing else),
establish the four `##` sections — Near term, Mid term, Long
term, Parked — before walking items. Subsequent passes walk the
content.

## What not to do

- **Don't invent items.** Roadmap entries should map to ideas the
  operator has agreed are real or work the project has started.
  Speculation belongs in feedback notes, not the roadmap.
- **Don't rewrite priorities.** Promote / demote / retire are the
  three moves; deep replanning is the operator's call.

## Canvas shape

```
# Roadmap

## Promoted
(bullets — items moved up a horizon or onto the roadmap from
ideas, with the trigger cited)

## Demoted or parked
(bullets — items moved down or off the active list, with the
reason)

## Recently landed
(bullets — items the events confirm as done; removed from the
roadmap)

## No-change notes
(optional one line if roadmap still holds)
```

## How to work with the operator

- **Cite the trigger.** Each promote / demote should name the
  event, run, or idea that motivated it.
- **Walk ideas explicitly.** Don't silently fold an idea into
  the roadmap — name which idea, at which horizon, and why.
- **Hand off undecided items.** If you can't tell whether an
  idea should be near or mid term, leave it in feedback for the
  operator rather than guessing.

## Committing

`roadmap.md` edits and the canvas summary land in one per-turn
commit. The session-close gate refuses an empty canvas.

## When you're done

1. You've read `roadmap.md`, the kickoff context, and the open
   idea backlog.
2. Promotions, demotions, and landings (or their absence) are
   named on the canvas.
3. Edits to `roadmap.md` are committed.
4. The operator has what they need to take the next step.

## Before you start

Skim the operations-stage canvas for this run. A new ritual or
tool often implies a near-term roadmap item — fold them in here.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
