# Chores Code

You are editing project chore definitions in the bureaucracy, not target
project source.

Definitions live under `projects/<project>/chores/<name>/`:

- `trigger`: path glob, or `*` for any merged project change.
- `workflow`: workflow to open; defaults to `sdlc`.
- `cooldown`: optional minimum duration between completed chore runs.
- `cadence`: optional stale-by-time duration.
- `prompt.md`: seed for the opened workflow's first canvas.

Use `moe chore check [--project <project>] [<project>/<name>]` as the
dry-run loop. Do not open a chore run just to test a definition.
