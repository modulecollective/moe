# Stage: code

You are at the hooks workflow's code stage. The canvas is the brief on
entry and the per-pass record on exit. The work itself happens in the
project's hook scripts under `projects/<project>/hooks/<event>.d/*`,
not in source code. There is no design stage before this and no push
verb after — the session-close commit IS the landing.

## What you edit

- **Hook scripts only.** `projects/<project>/hooks/<event>.d/*` for the
  named project. Today three events ship: `dev-env`, `dev-env-teardown`,
  `pre-push`. The agent may also add a new event's directory if the
  brief calls for it.
- **The canvas.** `documents/code/content.md` for this run. Update it
  as the per-pass record — what changed, why, anything the next person
  reading the history should know.
- **Nothing else.** Editing `src/` (the project's submodule code), or
  another project's hooks, is scope creep. Flag it back to the operator;
  don't smuggle it in.

## Iterating

`moe hook fire <project> <event>` mints a transient sandbox and runs
the named event's scripts once. Use it instead of opening a real run
just to test a 30-line bash change. The transient sandbox is left on
disk and its path is printed so you can poke around after a failure.

- `dev-env` — runs `dev-env.d/*` and dumps the merged `KEY=VALUE` on
  stdout. The cache (`.moe/dev-env.env`) is intentionally NOT written;
  this is throwaway.
- `dev-env-teardown` — runs `dev-env.d/*` in memory to populate the
  env teardown expects, then runs `dev-env-teardown.d/*` against it.
- `pre-push` — runs `pre-push.d/*` against the transient sandbox.

Fire is fire-and-inspect, not a test harness. There are no assertions
or fixtures; the operator reads stdout and decides whether it's right.

## Conventions for the scripts

The existing dev-env.d/* scripts set the bar; new scripts match it:

- Shebang first line (`#!/bin/sh` or `#!/usr/bin/env bash` if you need
  bash). The runtime requires the file be executable (`chmod +x`).
- `set -euo pipefail` (or the `sh` equivalent: `set -eu`). A hook that
  swallows a failure silently is worse than a hook that loudly stops
  the chain.
- Self-contained. If a script depends on an earlier script's output
  (e.g. `20-db.sh` reads `$PORT` set by `10-port.sh`), make that
  ordering obvious in the filename and document the dependency in a
  one-line comment near the read site.
- Lex-sorted execution. Two-digit prefixes (`10-`, `20-`, …) leave
  room for inserts.
- `dev-env.d/*` scripts emit `KEY=VALUE` lines on stdout; everything
  else goes to stderr. `dev-env-teardown.d/*` and `pre-push.d/*` are
  stream-through (stdout/stderr both reach the operator's terminal).
- Read the `MOE_*` env vars exported by the harness (`MOE_PROJECT`,
  `MOE_RUN`, `MOE_BUREAUCRACY`, `MOE_SANDBOX`, optionally
  `MOE_WORKSPACE`, `MOE_TARGET_BRANCH`) — they're the contract for
  "which run is this, where is its tree, what branch is it about to
  push against." Don't reach outside that contract.
- `dev-env.d/*` must not mutate tracked files in the project repo
  (`$MOE_SANDBOX`). Setup work belongs in external locations the
  script owns — emit a path on stdout (e.g. `MOE_DEV_TMPDIR=...`) and
  write into there. The design stage's sandbox-boundary check
  snapshots HEAD after dev-env runs and refuses the close if the tree
  is dirty; a hook that touches tracked files would false-positive
  that gate.

## What the canvas should hold

The canvas is the per-pass record, not a design doc rewritten in code:

- One short paragraph naming what changed and why.
- A note on what was exercised end-to-end with `moe hook fire` — which
  events, which projects.
- Any constraints discovered mid-pass (a host-specific quirk, a
  version requirement, a script that has to stay self-contained for a
  reason that wasn't obvious from the design). The next person editing
  these scripts will read this.
- Open items (a follow-up the operator should chase, an event the
  scripts could grow into) — but prefer dropping these in
  `followups.md` so they pick up an idea-run path; the canvas is the
  record, not the todo list.

If the change is one-line and you exercised it once, two sentences
on the canvas is the right amount. Don't pad.

## What to avoid

- **Editing other projects' hooks.** This run is scoped to one project.
  If you notice another project's hooks need the same fix, raise it
  as a follow-up — don't bundle.
- **Editing src/.** The project's source tree is the submodule. Hook
  scripts that *invoke* binaries inside `src/` are fine; *modifying*
  files inside `src/` is a different workflow (sdlc).
- **Disabling a script to make a chain pass.** If `pre-push.d/40-vet.sh`
  is failing, fix vet or fix the script — don't `chmod -x` it and
  call the chain green.
- **Inventing a test harness.** No assertion fixtures, no `tests.d/`
  convention, no "expected output" diff. If hook test surface grows
  enough to want tooling, raise it as a follow-up; don't build it now.

## Committing

This stage runs in the bureaucracy session worktree itself (no
sandbox clone). The session worktree IS the bureaucracy on a private
branch — your edits to `projects/<project>/hooks/<event>.d/*` and to
the canvas land in the per-turn commit, which is the landing. No
`moe hooks push` verb exists; the close commit closes the run, but
the edits are already in main by then.

- Don't run `git commit` yourself. The harness commits each turn —
  the canvas, run.json, and the project's hooks/ directory ride
  together in one trailered commit per turn.
- Before you hand the session back, `git status` should be clean
  apart from the canvas / hooks edits the harness will stage.

## When you're done

The code stage is ready to hand back when:

1. The hook script(s) implement what the brief asked for — no
   speculative scope, no half-finished extras.
2. You exercised the change at least once with `moe hook fire` and
   the canvas names what you fired.
3. The canvas reads as a per-pass record a future agent could pick
   up cold.
4. The operator has what they need to run `moe hooks close <project>
   <run>` — or to say "not yet, because X."

If you're polishing canvas prose past the point of clarity, you're
done. Stop and hand it over.

## Only edit this run

The canvas for this stage lives under this run's `documents/` tree.
That is the only document you should write to. Other runs under
`projects/*/runs/*` are read-only context — never edit their
`content.md` files, even if you notice something that looks wrong.
If a past run's content is misleading or outdated, tell the operator
and let them open a new run to address it.
