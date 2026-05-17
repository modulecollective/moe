# Stage: roadmap

You are walking the project's `roadmap.md` against recent activity
and the open idea backlog. Roadmap is prioritised intent across
near, mid, long term, directions, and parked — what's actually
next, plus where the project *could* grow.

## What to do

**First, retire shipped work.** Before anything else this pass:
walk the events block. For every near-term (or mid-term) entry
the events show as landed, move it to a `## Recently landed` line
or remove it outright. Roadmap drift is dominated by shipped
items that linger; this step is mandatory, not a bullet among
many. If no items shipped, say so explicitly on the canvas.

Then walk the rest:

- **Read `digital-twin/<project>/roadmap.md` first.** Hold the
  five `##` sections (Near term, Mid term, Long term, Directions,
  Parked) in your head as you walk events and ideas.
- **Reconcile near-term items.** A near-term item the project has
  quietly stopped working on is a candidate to demote or park.
- **Promote ideas the project has implicitly committed to.** Open
  ideas (`moe idea list`) that the operator has greenlit, or that
  recent work has started, belong on the roadmap. Promote them at
  the right horizon.
- **Walk Directions with the operator.** Directions is the place
  for uncommitted "we could go this way" entries — items that
  reflect-time conversation can promote into Long/Mid/Near as the
  project's appetite shifts. Each pass, surface Directions
  entries that look ready to commit, and capture new ones the
  operator names.
- **Demote or park unfunded items.** A roadmap item the project
  is *not* doing — and won't be soon — belongs in Parked with a
  reason.
- **Fold parked items the project is quietly doing.** If recent
  work touched something Parked, surface that — either remove from
  Parked (now near/mid term) or note the drift.

If roadmap hasn't moved this pass, say so. A quiet roadmap pass
is healthy if the prior reflect was recent — but the retire-
shipped step still runs, and "no items shipped" is the canvas
note when that's true.

## Fresh roadmaps

On a freshly-stubbed `roadmap.md` (just the H1 and nothing else),
establish the five `##` sections — Near term, Mid term, Long
term, Directions, Parked — before walking items. Subsequent
passes walk the content.

## Long term vs. Directions

The boundary is commitment, not distance.

- **Long term** holds intent the operator has agreed is real,
  just not soon. Committed but distant.
- **Directions** holds places the project could plausibly grow,
  with no current commitment. Each Directions entry carries an
  explicit "no current commitment" framing so it doesn't
  masquerade as a plan. Promotion *out* of Directions is the
  natural reflect-time conversation — Directions is a holding
  area, not a backlog.

## What not to do

- **Don't invent committed items.** Near, Mid, and Long term map
  to ideas the operator has agreed are real or work the project
  has started. Directions is the place for "we could go this
  way" — it lives separately so committed horizons don't get
  diluted.
- **Don't let Directions become a dumping ground.** Each
  Directions entry should be a direction the operator has
  actually named or you've inferred from a clear signal in recent
  events. When in doubt, leave it in feedback for the operator,
  not in Directions.
- **Don't rewrite priorities.** Promote / demote / retire are the
  three moves; deep replanning is the operator's call.

## Canvas shape

```
# Roadmap

## Retired (shipped)
(bullets — near/mid items the events confirm landed, moved to
Recently landed or removed. Required section: write "no items
shipped this pass" if true. Empty silence is not valid.)

## Promoted
(bullets — items moved up a horizon, onto the roadmap from
ideas, or out of Directions into a committed horizon, with the
trigger cited)

## Demoted or parked
(bullets — items moved down or off the active list, with the
reason)

## Directions added or walked
(bullets — new Directions entries the operator named, plus any
existing Directions entries surfaced this pass with the
operator's call — promote, leave, or remove)

## No-change notes
(optional one line if roadmap still holds beyond the retire-
shipped pass)
```

## How to work with the operator

- **Lead with the retire-shipped result.** First thing on the
  canvas: what shipped this pass and got cleared from the
  roadmap, or "no items shipped this pass."
- **Cite the trigger.** Each promote / demote should name the
  event, run, or idea that motivated it.
- **Walk ideas explicitly.** Don't silently fold an idea into
  the roadmap — name which idea, at which horizon, and why.
- **Walk Directions out loud.** Promotion *out* of Directions is
  the natural conversation; name candidates rather than waiting
  for the operator to ask.
- **Hand off undecided items.** If you can't tell whether an
  idea should be near or mid term — or whether something belongs
  in Long term or Directions — leave it in feedback for the
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
