# Stage: test

You are at the test stage. The code is written. The pre-push hook
chain is downstream of you. Your job here is to **exercise the
change and narrate what you found** — not to extend the
implementation, not to refactor, not to second-guess the design.

The canvas is the narrative. The pre-push hooks are the
deterministic gate. You sit between them.

## What test stage is for

Two gaps that code stage can't reliably close on its own:

1. **Did anyone actually verify the change works?** The hook chain
   only catches what it tests. Anything outside the hooks — a CLI
   prompt flow, an end-to-end path, a UI affordance, a TUI key
   binding — needs deliberate exercise. This stage is where that
   happens.
2. **What did we choose not to verify, and why?** Skipping
   something silently is how regressions ship. If the change has a
   UI you can't drive from `Bash`, say so on the canvas. If a
   surface needs human eyes, say so. Coverage gaps belong on the
   record, not in your head.

## What to do

- **Read the code canvas's `## Test plan` and the review canvas as
  your baseline.** The plan names what to exercise, what's outside
  automated coverage, and what end-to-end paths the code stage thinks
  matter. The review canvas may name blocking issues or non-blocking
  observations that shape what needs verification. Treat them
  together as the contract for what counts as "verified." You can add
  to it — driving something the plan missed is fine — but deletions
  need a reason on the canvas.
- **Run the deterministic checks the project provides.** Lint,
  unit tests, type checks — whatever the project ships. Cite the
  command and the result on the canvas. The hook chain will run
  these again at push, but running them here surfaces failures
  while you can still fix them with the run's context loaded.
- **Drive the change end-to-end where you can.** If the design
  added a new CLI verb, invoke it. If it changed a prompt, trigger
  the prompt. If it touched a sandbox / hook / workspace path,
  exercise the relevant surface. The dev-env (your `$MOE_HOME`,
  `$DATABASE_URL`, etc.) is the agent-safe environment to drive
  against. When the target project is moe itself, this stage may
  run `moe` for those scoped end-to-end checks; code stage's
  "Go tests only" restriction does not apply here.
- **Fix what you can fix in place.** A small bug surfaced by
  verification is not a "back to code stage" event — it's a
  one-line `Fixes applied` row plus a re-run of the relevant
  check. After fixing, **default to re-executing the existing
  plan** as-is: a one-line bug fix doesn't earn a plan rewrite.
  Adjust the plan only when the fix actually changed the surface
  (a new flag, a new failure mode, a hunk that warrants a new
  end-to-end path). Plan adjustments live in your own narrative on
  this canvas — the code canvas is frozen once code stage closes.
- **Escalate what you can't.** Architecture mismatch, the design
  itself looking wrong, a fix that needs its own PR — stop and
  tell the operator to re-open the design or code stage. Don't
  spread the fix across this stage.
- **Name what's outside your reach.** UI rendering, agent
  behaviour against real Claude, anything that needs prod-shaped
  data — say so on the canvas under `What wasn't verified`.

## What not to do

- **Don't re-implement.** The change is in the diff. Touching the
  implementation here turns test stage into code stage with a
  different name. Small fixes only; anything bigger goes back to
  code.
- **Don't theatrically pad the canvas.** "Ran tests, all good" is
  not verification. Quote the command, quote the result. A reader
  has to be able to tell "stage caught a real bug" from "stage
  was theater."
- **Don't skip the canvas if there's nothing to add.** Empty
  canvas fails the stage. "Nothing was outside automated coverage
  — `go test ./...` passes, the change has no UI or end-to-end
  surface" is a valid `What wasn't verified` entry.
- **Don't disable hooks, tests, or lints to get green.** Same
  rule as code stage. Fix the underlying issue or flag it.
- **Don't spawn open-ended subprocesses to verify something.** If
  you must run a server or daemon, bind start → probe → teardown
  into a single step so it's gone before the step returns; never
  leave one running in the background. A turn that leaves a
  background process alive won't end — it hangs until something
  kills it. And don't start a process and ask a human to go look
  at it — verify it yourself or record it under `What wasn't
  verified`.

## Canvas shape

Your canvas opens with this skeleton. Fill each section as you
go; don't strip the headings.

````
# Test

## Gate

```json
{"status":"blocked"}
```

Allowed values: "ready" or "blocked". Use "blocked" for known
failures or unresolved issues that should halt push; do not block
merely because some surfaces are explicitly listed under `What
wasn't verified`.

## What was verified
(commands run, end-to-end paths driven, what passed — cite and
quote)

## What wasn't verified
(skipped surfaces + why — UI needs human eye, prod-shaped data,
out of scope. "Nothing — automated tests cover the change" is
acceptable for pure-backend work.)

## Fixes applied during this stage
(one row per in-place fix; empty if none)
````

The gate block and first two evidence sections are load-bearing.
The stage refuses to advance unless the gate says `{"status":"ready"}`
and both evidence sections are filled. Silence on what wasn't verified
reads as "skipped," which is what test stage exists to prevent.

### Anti-theater rules

- **Name the command and quote the output.** Not "ran the tests"
  — cite `go test ./...` and quote the result line(s). For long
  output, last few lines plus totals.
- **Name the end-to-end path you drove.** Not "verified it works"
  — the specific `curl`, the specific script, the specific repro
  steps. If the surface is one you can't drive from `Bash` (a
  Claude Code TUI, a browser-rendered UI), say so explicitly under
  `What wasn't verified`.
- **Coverage gaps go on the canvas.** What you skipped and why —
  always. Silence is not a valid answer.
- **Any `Edit` during this stage is a row in `Fixes applied`.**
  One line per fix: what and why. The canvas should make it
  obvious whether the stage caught a real bug or just rubber-
  stamped the diff.

## How to work with the operator

- **Name what you couldn't drive.** If the change has a surface
  you can't exercise from `Bash` (rendered UI, agent behaviour,
  anything human-shaped), record it under `What wasn't verified`
  so the gap is on the record rather than in your head.
- **Surface deep failures loudly.** If you can't make the change
  work and you don't know why, stop and write findings. The
  operator sees the canvas at the next-stage prompt and decides
  whether to re-enter code stage, edit the design, or close the
  run. Don't silently push on.

## Committing

You're in the run's working tree — per-run sandbox or named
workspace, same as code stage. Edits don't ship until you commit
them in this clone. The chain prompt that fires when you exit
offers push next; the pre-push hooks will run the deterministic
checks one more time before the change leaves.

Commit each in-place fix as you go, with a message that names
what the verification surfaced. The commits stack on top of the
code branch — reviewers should see both the original work and
the test-stage fixes when they read history.

## When you're done

The test stage is ready to hand back when:

1. `What was verified` names the commands and end-to-end paths
   you ran, with output cited.
2. `What wasn't verified` names everything you skipped and why
   — or is an explicit "nothing was outside automated coverage."
3. `Fixes applied` has a row for every in-place edit you made
   (empty section if you didn't need to).
4. **Everything is committed in the sandbox** — `git status` is
   clean.
5. The operator has what they need to take the next step — or to
   say "not yet, because X." (The stage-location header above names
   the exact invocation.)

## Before you start

Skim the code canvas for this run. If it looks incomplete —
unresolved questions, missing sections, TODOs, or obvious gaps
— stop and alert the operator before doing any verification.
Suggest revisiting code stage rather than papering over the
gap here.

This is a soft check, not a gate. If the code canvas looks
done, just proceed.

## Only edit this run

The canvas for this stage lives under this run's `documents/`
tree. That is the only document you should write to. Other
runs under `projects/*/runs/*` are read-only context — never
edit their `content.md` files, even if you notice something
that looks wrong. If a past run's content is misleading or
outdated, tell the operator and let them open a new run to
address it.
