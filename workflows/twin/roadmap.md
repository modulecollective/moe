# Stage: roadmap

You are walking the project's `roadmap.md` against recent activity
and the open idea backlog. Roadmap is thematic intent — what *kinds*
of work the project might invest in, plus whatever's committed and
about to land. It is not a backlog mirror; the backlog lives in
ideas and runs.

## What to do

**First, retire shipped work.** Before anything else this pass:
walk the events block. For every In flight entry the events show
as landed, remove it. Roadmap drift is dominated by shipped items
that linger; this step is mandatory, not a bullet among many. If
no items shipped, say so explicitly on the canvas.

Then walk the rest:

- **Read `digital-twin/<project>/roadmap.md` first.** Hold the
  three `##` sections (In flight, Themes, Parked) in your head as
  you walk events and ideas.
- **Reconcile In flight.** In flight is committed work about to
  land — strict cap of 0–2 items, empty by default. If an In
  flight item has gone quiet or isn't actually about to land,
  demote it (to a Theme if the *kind* of work is still live, to
  Parked if it isn't).
- **Promote ideas the project has greenlit into In flight.** Open
  ideas (`moe idea list`) that the operator has greenlit and
  that are about to land belong here. The bar is high — empty In
  flight is the normal state.
- **Walk Themes with the operator.** Themes name *kinds* of work
  the project might invest in — "technical debt reduction",
  "web UI enhancements". Each theme is a paragraph naming the
  opening and the rough shape of the bet, optionally trailed by
  0–N related slugs. Surface themes that look ripe to convert
  into committed work, and capture new themes the operator
  names.
- **Themes without slugs are first-class.** Do not require a slug
  bullet for a theme to exist; a theme can name an opening with
  no current backlog representation, and that's exactly the
  point of the register.
- **Park items the project has explicitly decided not to do.**
  Parked carries the reason on one line. Move out of Parked when
  recent work touches the parked thing.

If roadmap hasn't moved this pass, say so. A quiet roadmap pass
is healthy if the prior reflect was recent — but the retire-
shipped step still runs, and "no items shipped" is the canvas
note when that's true.

## Fresh roadmaps

On a freshly-stubbed `roadmap.md` (just the H1 and nothing else),
establish the three `##` sections — In flight, Themes, Parked —
before walking items. Subsequent passes walk the content.

## In flight vs. Themes vs. Parked

The boundary is commitment and altitude, not distance.

- **In flight** is committed work about to land — strict cap of
  0–2 items, empty by default. Committed work has a backlog home
  (ideas + runs); In flight exists for things the operator wants
  visible at the twin layer, which is a high bar.
- **Themes** is the primary thematic register — a paragraph per
  theme, optionally trailed by 0–N related slugs. A theme names
  a *kind* of work the project might invest in, not a specific
  deliverable. Themes without slugs are first-class.
- **Parked** holds items the project has decided not to do soon,
  with the reason on one line.

Promotion *into* In flight is the natural reflect-time
conversation — a theme heats up, the operator names a slug, the
slug moves into In flight (or kicks off a fresh idea / run).

## What not to do

- **Don't invent In flight items.** In flight maps to ideas the
  operator has greenlit or runs about to land. Empty In flight
  is fine; pad-filling it dilutes the signal.
- **Don't write Themes at slug altitude.** A theme is a *kind*
  of work; a slug is a specific deliverable. If the paragraph
  reads like a backlog item, it's not a theme — it belongs in
  an idea, In flight, or Parked.
- **Don't let Themes become a dumping ground.** Each theme
  should be a kind of work the operator has named or you've
  inferred from a clear signal in recent events. When in doubt,
  leave it in feedback for the operator, not on the roadmap.
- **Don't rewrite priorities.** Promote / demote / retire are
  the three moves; deep replanning is the operator's call.

## Canvas shape

```
# Roadmap

## Retired (shipped)
(bullets — In flight items the events confirm landed, removed.
Required section: write "no items shipped this pass" if true.
Empty silence is not valid.)

## Promoted
(bullets — items moved into In flight from Themes, ideas, or
new commitment, with the trigger cited)

## Demoted or parked
(bullets — items moved off In flight or onto Parked, with the
reason)

## Themes added or walked
(bullets — new themes the operator named, plus any existing
themes surfaced this pass with the operator's call — promote,
leave, or remove)

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
  In flight — name which idea, and why.
- **Walk Themes out loud.** Promotion out of a Theme (into a
  fresh idea or directly into In flight) is the natural
  conversation; name candidates rather than waiting for the
  operator to ask.
- **Hand off undecided items.** If you can't tell whether an
  idea belongs In flight, or whether a Theme is hot enough to
  ride, leave it in feedback for the operator rather than
  guessing.

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
tool often implies a new In flight item — fold it in here.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
