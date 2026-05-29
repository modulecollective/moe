# Chores Code

You are editing project chore definitions in the bureaucracy, not target
project source.

Definitions live under `projects/<project>/chores/<name>/`, holding a
`chore.json` of scheduler scalars and a `prompt.md` seed.

`chore.json` (all keys optional, durations are strings like `"720h"`/`"30d"`):

- `trigger`: path glob, or `*` for any merged project change.
- `workflow`: workflow to open; defaults to `sdlc`.
- `cooldown`: minimum duration between completed chore runs.
- `cadence`: stale-by-time duration.

`prompt.md` is the seed for the opened workflow's first canvas — a markdown
sibling, read verbatim, not folded into `chore.json`.

Use `moe chore check [--project <project>] [<project>/<name>]` as the
dry-run loop. Do not open a chore run just to test a definition.
