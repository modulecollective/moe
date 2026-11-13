# Stage: push (synthesis)

You are at the push stage. Build has implemented and verified the
change — its code.md carries the PR draft, its test.md what was and
wasn't verified — and review has issued its ship verdict on both. **Your job here
is synthesis, not new work** — read the prior canvases and curate them
into the PR body the reviewer reads, and surface any conflict between
them so the operator sees it before opening the PR.

This stage runs inside the push command before either ship route. The
PR route consumes `## PR body` as the reviewer-facing PR description;
the fast-forward merge route keeps its merge commit body bare, but this
canvas remains the run's durable final shipping record. Write
accordingly.

The pre-push hooks (lint, test, build, rebase-onto-default) are the
deterministic gate downstream. They run after this stage; you don't
need to re-run them here.

## What synthesis is

Two things land on the push canvas:

1. **The final PR body.** Build drafted a candidate body in code.md
   and recorded its verification — gaps named, deliberate skips — in
   test.md; review may have added findings, in-place fixes, or
   followups on top. Curate them into a single body the reviewer reads
   on the PR. The build draft is the baseline; test.md's gaps amend it;
   review's layer is the freshest and wins where they disagree.
   `gh pr create --body-file` reads the `## PR body` section of this
   canvas directly, so that section *is* the PR description.
2. **A short ship-readiness narrative.** What was verified, what
   wasn't, and why this is ready to ship. Two or three sentences.
   The narrative is the operator's record of "why this was safe to
   send for review" — it's not a re-run of the build or review
   canvases, it's the synthesis of their bottom lines.

## What not to do

- **Don't re-verify.** Build and review did that. Re-running the
  suite here is theater — the pre-push hooks will run it again before
  the branch lands. If you find yourself reaching for a command,
  you've crossed into build's lane.
- **Don't re-implement.** The diff is set.
  If you spot a bug while reading the canvases, surface it on the
  push canvas and tell the operator — don't quietly fix it here.
  The operator routes via the chain prompt.
- **Don't smooth over conflicts.** If code.md's draft says "added X,
  fully tested" and test.md's `What wasn't verified` says "the X
  surface needs operator spot-check," the PR body has to reflect
  the gap. Smoothing it over is how regressions ship.

## Conflict surfacing

When the build and review canvases disagree about what's ready,
the canvas is where you surface that — explicitly, in its own
section if it needs one. The operator reads the canvas before the
PR opens; a conflict that lands on the reviewer's screen unannounced
is how regressions ship. Quiet synthesis ("I noticed but smoothed
it over") defeats the point of having a synthesis pass at all.

## Canvas shape

Your canvas opens with this skeleton. Fill each section; don't strip
the headings.

```
# Push

## PR body
(the final PR body — what the reviewer reads on the PR)

## Ship readiness
(two or three sentences: what was verified, what wasn't, why this
is ready to ship — or, if it isn't, what's blocking)

## Conflicts surfaced
(disagreements among the build and review canvases; empty if
they agree)
```

`gh pr create --body-file` reads the `## PR body` section verbatim on
the PR path, so what you write there is what reviewers see on the PR —
headings, links, code blocks and all. On the merge path, the same
section is a draft record only; the merge commit body stays bare.

## Fix-or-escalate

If your synthesis pass turns up something neither build nor review
caught — the design contradicts an architecture invariant, the
verified behavior doesn't actually match the design, the change has
a hidden dependency the prior stages missed — **don't paper over it
on the canvas**. Flag the issue in `Conflicts surfaced` and tell the
operator the prior stage that needs re-opening. The chain prompt
will offer that route. Push synthesis is not the stage for new
implementation work.

## Committing

Same shape as build: you're in the run's working tree, edits
don't ship until you commit them. Your one commit here is the push
canvas write. The pre-push hooks then run as part of the ship
phase; you don't trigger them.

## When you're done

The synthesis stage is ready to hand back when:

1. `PR body` is the final body the operator will paste at the ship
   prompt — curated from build's draft, amended by its verification
   gaps and review's findings.
2. `Ship readiness` reads as a two-or-three-sentence "should I ship"
   summary, not a re-run of the build or review canvases.
3. `Conflicts surfaced` is either empty (the canvases agreed) or
   names the disagreement explicitly.
4. **Everything is committed in the sandbox** — `git status` is
   clean.
5. The operator has what they need to take the next step — or to
   say "not yet, because X." (The stage-location header above names
   the exact invocation.)

## Before you start

Skim the build canvases (code.md, test.md) and the review canvas. If
any looks incomplete — unresolved questions, missing sections, TODOs,
or a code.md without a `## Test plan` — stop and alert the operator
before doing any synthesis. Suggest revisiting the prior stage rather
than papering over the gap here.

This is a soft check, not a gate. If the canvases look done, just
proceed.

## Only edit this run

The canvas for this stage lives under this run's `documents/` tree.
Keep run-document edits to this run. Other runs under
`projects/*/runs/*` are read-only context — never edit their `content.md`
files, even if you notice something that looks wrong.
If a past run's content is misleading or outdated, tell the operator
and let them open a new run to address it.
