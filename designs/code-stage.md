# Design: rename the `pr` stage to `code`

## Problem

The lifecycle stage currently named `pr` is overloaded. Its stage
fragment (`stages/pr.md`) fires during the window between
`design`-signed and `pr`-signed — i.e., while the agent is actually
writing code. But the stage is *called* `pr` because signing it is the
gate that will eventually push the submodule and open a GitHub PR. One
name, two meanings: the phase where code is written, and the moment
it's shipped. The fragment ends up labeled "Stage: pr" even though the
work in that window is coding.

## What to do

Rename the stage `pr` → `code`. Everything else stays: `code` still
requires `design`, signing `code` still flips status to `approved` and
(future) will still push the submodule and open the PR. Only the
label changes.

Concretely:

- `internal/stage/stage.go`: rename the map key and its `Help`.
- `internal/cli/work.go`: the two string literals (`"pr"` and
  `"pr.md"`) become `"code"` and `"code.md"`.
- `internal/cli/sign.go`: the `stageName == "pr"` branch becomes
  `== "code"`.
- `stages/pr.md` → `stages/code.md`; update the header from
  "Stage: pr" / "You are at the PR stage" to "Stage: code" /
  "You are at the code stage." Rest of the fragment is already about
  writing a landable diff, which is what the code stage does.
- `internal/cli/work_test.go`: rename the fixture filenames and stage
  strings the tests reference.
- `README.md`: update references to the *stage* (e.g.
  `moe sign … pr`, `MoE-Stage-Signed: pr`, `stages/pr.md`). Leave
  references to the *GitHub PR* alone — opening a PR on the target
  repo is still a thing, it just happens as a side-effect of signing
  `code`.

## Why not add `code` as a third stage?

A `design → code → pr` chain looks tidier on paper but earns nothing:
"code is done" and "ready to open the PR" are the same operator
decision. Adding a separate sign between them is ceremony without a
choice behind it. Smaller vocabulary beats symmetric vocabulary.

## Tradeoffs

- **Journal discontinuity.** Any pre-existing
  `MoE-Stage-Signed: pr` trailer in a bureaucracy's git log stops
  being recognized as a signed stage, because the lookup is a literal
  string match on the trailer value. `moe` is new enough that there is
  no meaningful production history to migrate; a bureaucracy that
  cares can re-sign or filter-repo. We don't build a migration path in
  this change.
- **Verb feel.** `moe sign … code` reads a shade worse than
  `moe sign … pr` as a standalone phrase, but the stage fragment it
  unlocks is unambiguously about coding, which is the more common
  cognitive load. Net positive.

## Scope

In: the rename listed above.

Out:
- Any behavior change to `sign` (still flips status; push/PR-open
  still TBD).
- A migration path for old `MoE-Stage-Signed: pr` trailers.
- Changing the shape of the `stages/code.md` fragment beyond the
  header lines.

## Open question

- Is there any real bureaucracy history with `MoE-Stage-Signed: pr`
  trailers we need to preserve? If yes, we'll need a `moe` command
  that rewrites trailers (out of scope here). If no — which is the
  working assumption — land it clean.
