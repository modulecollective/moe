# Stage: finalize

You are at the finalize stage. The per-doc walks are done; six
managed docs have been edited (or quietly left alone). Your job
here is to **seal the pass**: clear remaining hygiene findings
inline where you can, fold the events into `history-summary.md`,
and surface anything the prior stages flagged but didn't fix.

The session-close gate re-runs the structural hygiene scan and
**refuses to seal** if findings remain. Finalize is a working
stage: don't hand back to upstream stages for cleanups — fix
them here, the same way the test stage fixes small things found
during verification.

## What to do

- **Re-run the hygiene scan.** Read the kickoff's findings block.
  For each finding, decide: can you fix it inline (rename an
  entry, patch a broken link, fold an empty doc into its home),
  or does it imply a per-doc rewrite that an upstream stage
  should have handled? Inline fixes go in. Escalations become
  notes on the canvas + entries in `feedback/twin.md` for the
  next reflect.
- **Fold events into `history-summary.md`.** The events block
  in your kickoff is the verbatim tail since the last reflect.
  Compress it into a fresh paragraph appended to
  `digital-twin/<project>/history-summary.md`. Keep it slow-
  growing prose: the twin's compressed memory of everything
  before the next checkpoint. Don't drop signal future reflects
  will need.
- **Walk the prior stage canvases.** Anything they flagged "for
  operator" or "needs follow-up" that didn't get acted on — fold
  the residue into either an inline fix here, a feedback-twin
  note, or a roadmap follow-up (if the operator agrees).
- **Surface what you can't fix.** If a finding genuinely needs a
  per-doc rewrite that isn't a cleanup, name it on the canvas
  under "What I left" with the reason it didn't fit a finalize
  fix. The next reflect picks it up.

## What not to do

- **Don't escalate to upstream stages.** This is the test-stage
  contract: fix what you can fix in place. Escalation here means
  a feedback-twin note for the *next* reflect, not a re-walk of
  this pass's upstream stages.
- **Don't write a new history summary from scratch.** Append; the
  existing summary is load-bearing context.
- **Don't trim the events list.** The events block is what got
  walked; the history-summary delta reflects all of it (even
  the quiet entries).

## Canvas shape

```
# Finalize

## What I fixed
(bullets — concrete cleanups applied inline: which entry
renamed, which link patched, which doc tidied. Empty if the
prior stages left a clean sheet.)

## What I left
(bullets — findings I couldn't fix here, with the reason and
the feedback-twin note (or follow-up) where the residue
lives. Empty if everything cleared.)

## History-summary delta
(the paragraph(s) appended to history-summary.md this pass — a
prose compression of the events block, slow-growing, signal-
preserving)
```

The first three sections are load-bearing. The stage refuses
to seal with `What I fixed` and `What I left` both empty (an
anti-theater check — silence on findings reads as "skipped"),
and the post-flight hygiene scan refuses to seal with leftover
findings.

## How to work with the operator

- **Show the scan.** Quote the hygiene findings block at the
  top of your canvas, then mark each finding with `[fixed]`,
  `[left for next reflect]`, or `[didn't apply]`.
- **Justify the deltas.** History-summary edits should be
  legible at a glance — name what landed and what the operator
  should remember six reflects from now.

## Committing

Your inline fixes to any managed doc, the appended
`history-summary.md`, and the canvas summary land in one
per-turn commit. The wiki engine writes `log.md` and
`checkpoint.json` as part of the same commit. The session-close
gate refuses both an empty canvas and a non-empty hygiene scan.

## When you're done

1. The hygiene scan is clean (or the residue is named on the
   canvas with feedback-twin notes filed).
2. `history-summary.md` has a fresh paragraph appended that
   compresses this pass's events.
3. The canvas's `What I fixed` and `What I left` together name
   every finding from the kickoff.
4. The post-flight scan passes; the engine seals the pass.

## Before you start

Skim the six prior stage canvases (vision → glossary). Their
`What I flagged` / `Open questions for the operator` /
`No-change notes` sections are the surface this stage covers.
A finding the kickoff lists but no upstream stage mentioned is
fair game for inline fix; a finding an upstream stage explicitly
flagged for finalize is what you act on here.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to.
