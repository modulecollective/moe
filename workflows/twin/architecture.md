# Stage: architecture

You are walking the project's `architecture.md` against recent
activity. Architecture is the durable shape: components,
boundaries, load-bearing decisions. The canvas captures your
"what changed and why" for this walk; the doc itself is what you
edit when reality has moved.

## What to do

- **Read `digital-twin/<project>/architecture.md` first.** Hold
  its component list and decision log in your head as you read
  the events and feedback.
- **Walk events against shape.** Did recent work introduce,
  remove, or reshape a component or boundary? Did a recorded
  decision get revisited, contradicted, or quietly retired?
- **Edit in place on the evidence.** Architecture is a working
  doc. Apply the structural updates the events justify and record
  the case on the canvas; the closed-schema filenames don't move,
  but the content does. The committed pass is what the operator
  reviews — interactive or headless alike — not a mid-pass handshake.
- **Cross-reference the prior stage.** Drift the vision stage
  flagged may have an architectural shape here — name it if so.
- **Compress when the doc is over budget.** The kickoff's doc list
  marks each doc's soft budget. If architecture is over it,
  cutting it back is in scope this pass even when no event touches
  the shape — a compression-only edit is a valid pass.

If nothing in this pass moves architecture, say so explicitly on
the canvas. A quiet pass is a valid pass, and so is a pass whose
only work was compression.

## What not to do

- **Don't speculate.** A future component that nobody is building
  yet doesn't belong in architecture.
- **Don't rewrite decisions on a hunch.** Updating a decision the
  events show has *actually* changed is fine — cite them. Rewriting
  because *you* think the original was wrong, with no event behind
  it, is a flag for the operator, not a reflect edit.
- **Don't rename or retire files.** Closed-schema means the doc
  set is fixed.
- **Don't restate what another doc owns.** Architecture owns
  shape and boundaries; patterns owns named recurring or refused
  shapes; operations owns rituals and tools. If a rule already
  lives in one of those, link by section heading instead of
  rewording it here.
- **Don't inline provenance narrative.** A decision keeps at most
  a one-line trailer pointing at the run slug — "decided 2026-05;
  see run `moe/<slug>`". The story of how it got made lives in
  `history-summary.md` and the run canvas, not in architecture.

## Canvas shape

```
# Architecture

## What I updated
(bullets — concrete edits applied to architecture.md, with
section headings and one-line "why")

## What I flagged for the operator
(bullets — drift I didn't act on alone, with the reason: thin
evidence, or a call I couldn't settle from the events)

## No-change notes
(optional one line if architecture still holds)
```

## How to work with the operator

- **Cite the event.** "Recent X work in run Y added a Z
  component" beats "architecture looks stale."
- **Name the boundary.** When you edit, name what boundary moved
  and why — boundaries are the load-bearing claim, not the
  component list.
- **Walk the decisions log.** A revisited decision gets its
  *rule* rewritten to current state, plus at most a one-line
  trailer naming when and which run — not an accreting stack of
  amendments. The superseded narrative folds into
  `history-summary.md` at finalize, and then comes out of
  architecture.

## Committing

`architecture.md` edits and the canvas summary land in one
per-turn commit. The session-close gate refuses an empty canvas.

## When you're done

1. You've read `architecture.md` and the kickoff context.
2. Updates (or their absence) are named on the canvas, with
   pointers to events that justify them.
3. Edits to `architecture.md` are committed.
4. The operator has what they need to take the next step.

## Before you start

Skim the vision-stage canvas for this run. If it flagged drift
the architecture stage should act on, name it on your canvas as
you fold it in.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to. Other
runs under `projects/*/runs/*` are read-only context — never
edit their `content.md` files, even if you notice something
that looks wrong.
