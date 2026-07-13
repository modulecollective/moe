---
name: moe-howto
description: How to capture and groom the backlog from a MoE chat session — open, edit, close, and reopen idea runs, and read the dash/list surfaces that inform grooming. Use when the operator asks you to open an idea, track something, or tidy the idea backlog. Does not cover starting or driving coding work.
---

# Capturing and grooming the backlog

You are in a Ministry of Everything (MoE) chat session — a thinking
partner, not a builder. One concrete thing you do on the operator's
behalf is tend the **idea backlog**: capture new ideas, refine existing
ones, and close or reopen them as the operator decides. This skill is
the verb set for that, and nothing more.

You're interactive, so the operator sees every command before it runs.
Propose the command, run it when they're on board, and report what it
did. Grooming lands on the operator's **live** backlog: `moe idea new`
commits to the real bureaucracy on this box, so a capture shows up in
any other window's `moe dash` at once (and you see that window's commits
in turn). If a mutating command fails — another moe process holds the
lock, or the main checkout has uncommitted changes (`moe idea new`
refuses a dirty tree) — print the exact command and let the operator run
it from their own shell. Surfacing the command is always an acceptable
outcome.

## Capture a new idea

```
moe idea new <project>/<slug>
```

Opens a new idea run. The slug is lowercase kebab (`[a-z0-9-]+`). An
idea is a single free-form canvas — the operator's capture of "a thing
worth doing", not a design. Keep what you write tight: the problem, why
it matters, and any constraints the conversation surfaced. Don't design
the solution; that's the sdlc design stage's job if the idea ever gets
promoted.

`moe idea new` opens `$EDITOR` and commits the resulting note. From an
agent session, use a temporary editor script so the command stays
non-interactive:

```sh
tmp=$(mktemp)
cat >"$tmp" <<'EOF'
#!/bin/sh
cat >"$1" <<'BODY'
# <slug>

- <what to remember>
- <constraint or context>
BODY
EOF
chmod +x "$tmp"
EDITOR="$tmp" moe idea new <project>/<slug>
rm -f "$tmp"
```

## Refine an existing idea

```
moe idea edit <project>/<slug>
```

Use this to sharpen a captured idea the conversation revisited — tighten
the problem statement, add a constraint, record a decision. Same rule:
refine the *what*, leave the *how* to design.

For non-interactive edits, use the same temporary-editor pattern. The
script receives the canvas path as `$1`; modify that file and exit 0.

## Close / reopen

```
moe idea close <project>/<slug>     # the idea is handled or no longer wanted
moe idea reopen <project>/<slug>    # flip a closed idea back to in_progress
```

Close an idea the operator has decided against or that's been folded
elsewhere. Reopen recovers a closed idea (or one whose promoted
destination has since closed) — it refuses if reopening would create two
live owners of the same intent, so trust the refusal when it fires.

## Read the backlog before grooming

Look before you capture, so you don't open a duplicate or re-decide a
settled question:

- `moe dash` — the board: every run and idea with status at a glance.
- `moe idea list <project>` — the idea backlog specifically.
- `moe idea cat <project>/<slug>` — dump one idea's canvas.
- `moe idea log <project>/<slug>` — render an idea's capture transcript.

Pair this with the `moe-context` skill when you want the deeper history
(prior run canvases, the journal sliced by run/doc/workflow).

## What this skill is not

This is the **grooming** verb set — capture and tidy intent. It is
deliberately silent on starting or driving coding work: `moe sdlc new`,
the `design` / `code` / `test` ladder, and `push` are not here, because
the chat agent is not the one who starts building. When an idea is ripe
to build, the move is to leave it well-captured and let the operator
open the coding ladder themselves.
