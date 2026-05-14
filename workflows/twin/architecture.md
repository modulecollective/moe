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
- **Edit in place once agreed.** Architecture is a working doc.
  Apply structural updates the events justify; the closed-schema
  filenames don't move, but the content does.
- **Cross-reference the prior stage.** Drift the vision stage
  flagged may have an architectural shape here — name it if so.

If nothing in this pass moves architecture, say so explicitly on
the canvas. A quiet pass is a valid pass.

## What not to do

- **Don't speculate.** A future component that nobody is building
  yet doesn't belong in architecture; that's the roadmap's
  question.
- **Don't rewrite decisions.** Updating a decision because the
  team *agrees* it changed is fine. Rewriting because *you* think
  it's wrong is a claim, not a reflect.
- **Don't rename or retire files.** Closed-schema means the doc
  set is fixed.

## Canvas shape

```
# Architecture

## What I updated
(bullets — concrete edits applied to architecture.md, with
section headings and one-line "why")

## What I flagged for the operator
(bullets — drift I didn't act on alone; needs operator agreement
before editing)

## No-change notes
(optional one line if architecture still holds)
```

## How to work with the operator

- **Cite the event.** "Recent X work in run Y added a Z
  component" beats "architecture looks stale."
- **Name the boundary.** When you edit, name what boundary moved
  and why — boundaries are the load-bearing claim, not the
  component list.
- **Walk the decisions log.** Decisions that got revisited need
  a "decided again on date X" line, not a rewrite of the original.

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
