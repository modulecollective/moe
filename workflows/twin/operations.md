# Stage: operations

You are walking the project's `operations.md` — how the project
runs day-to-day. Workflows, rituals, tools, escalation paths.
Operations is the runbook: not what the project *is* (vision) or
*looks like* (architecture), but what the operator and the agents
actually *do*.

## What to do

- **Read `digital-twin/<project>/operations.md` first.** Hold the
  documented workflows and rituals in your head as you walk
  events.
- **Check what's still true.** Did a workflow change? Did a tool
  get replaced? Did an escalation path get rerouted? Operations
  goes stale faster than vision or architecture — recent work is
  the canonical source.
- **Edit in place once agreed.** Operations is a working doc;
  update the runbook to match how the project actually runs.
- **Capture new rituals.** A practice that's emerged in recent
  work and stuck is a candidate to document — even short-form
  ("Operators run X before Y on Z days").

If operations hasn't moved this pass, say so.

## What not to do

- **Don't aspire.** Operations names what the project *does*, not
  what you wish it did. Aspirational rituals belong in roadmap.
- **Don't rewrite without a sighting.** A rewrite needs at least
  one recent event that justifies the new shape.
- **Don't restate what another doc owns.** Operations owns the
  ritual and the tool. The named shape it exercises lives in
  patterns; the boundary it touches lives in architecture. Link
  by section heading instead of repeating the rule here.

## Canvas shape

```
# Operations

## What I updated
(bullets — concrete edits with section headings and one-line
"why")

## What I flagged
(bullets — drift I didn't act on alone)

## No-change notes
(optional one line if operations still holds)
```

## How to work with the operator

- **Name the workflow.** "The X ritual is now Y because of recent
  Z work" beats "operations is stale."
- **Cite the trigger.** Every update should point at the event or
  run that made the old shape no longer true.

## Committing

`operations.md` edits and the canvas summary land in one per-turn
commit. The session-close gate refuses an empty canvas.

## When you're done

1. You've read `operations.md` and the kickoff context.
2. Updates (or their absence) are named on the canvas with
   triggers cited.
3. Edits to `operations.md` are committed.
4. The operator has what they need to take the next step.

## Before you start

Skim the patterns-stage canvas for this run. A promoted pattern
often implies an operations note — fold them in here.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
