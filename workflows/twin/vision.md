# Stage: vision

You are walking the project's `vision.md` against recent activity.
Vision is the project's stated bets, problem, and non-goals — what
this project is *trying to be*. Vision is asymmetric: your job here
is to **flag drift**, not to rewrite the vision.

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
- **Surface, don't decide.** Vision edits are decided edits —
  `moe twin claim` is where those live. Your job here is to make
  drift legible to the operator, who decides whether to claim.

If there is no drift this pass — vision still holds — say so on
the canvas explicitly. "No drift this pass; vision still holds"
is a valid one-line canvas. Silence is not.

## What not to do

- **Don't rewrite `vision.md`.** A new vision statement is a
  decided edit. Flag it; the operator runs claim later if they
  agree.
- **Don't manufacture drift.** A quiet pass is fine. If recent
  events haven't touched what vision claims, just say so.

## Canvas shape

The canvas is short by default. Use as much as you need; cut what
you don't.

```
# Vision

## Drift flagged this pass
(bullets — what the events show that vision doesn't, or vice
versa; each bullet a one-line claim with a pointer to the event)

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
- **Hand off, don't decide.** When you flag something, name the
  next move — usually "operator claims a vision edit" or
  "operator confirms no drift."

## Committing

Your edits to `vision.md` itself (if any structural cleanup is
warranted — typo fixes, broken links) and your canvas summary
land in the same per-turn commit. The session-close gate refuses
to seal an empty canvas; write at least the one-line "no drift"
note before exiting.

## When you're done

The vision stage is ready to hand back when:

1. You've read `vision.md` and the kickoff context.
2. Drift (or its absence) is named on the canvas.
3. Any structural fixes to `vision.md` are committed.
4. The operator has what they need to take the next step — or to
   say "not yet, because X." (The stage-location header above
   names the exact invocation.)
