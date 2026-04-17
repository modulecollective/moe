# Design: save the conversation transcript per turn

## Problem

The README has promised `thread.jsonl` since day one — a per-document
append-only log of the human/agent exchange alongside each document's
compressed `content.md` (README.md:300-311, 895). It is the durable
record that survives a Claude Code session rotation (README.md:356)
and the raw material any future `moe show` tail would render
(README.md:527, 797).

Nothing is written today. `runWork` launches `claude` interactively
(work.go:121-135) with its stdio wired straight to the operator's
terminal, lets it exit, and commits only `content.md` plus
`request.json` (work.go:350-362). Whatever was said during the turn
lives on Anthropic's servers — keyed to a UUID that would survive a
disk wipe but nothing local. Rewinding a conversation is
`git reset --soft` per the README, but there is currently nothing to
rewind the conversation itself *against*; only the document file
moves.

## What to do

Claude Code already writes every turn to
`<CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<session-id>.jsonl`
(default config dir `~/.claude`). After `runWork`'s child `claude`
process exits, locate that transcript by globbing
`<config>/projects/*/<session-id>.jsonl` — session ids are UUIDs, so
the match is unambiguous regardless of which cwd the session ran in
(doc-only runs at the bureaucracy root, code runs inside the sandbox
clone). Copy it over the document's `thread.jsonl`. Stage it with
the rest of the doc dir in `commitTurn`.

Concretely:

- `internal/claude/transcript.go` (new) — `TranscriptPath(sessionID)`
  does the glob; `CopyTranscript(sessionID, dest)` copies the file
  and returns a `(found bool, err error)` so callers can distinguish
  "no transcript yet" (legal — operator ctrl-C'd before claude
  persisted anything) from a real I/O failure.
- `internal/request/request.go` — add `ThreadPath(projectID, id,
  docID)` alongside `ContentPath`.
- `internal/cli/work.go` — between `cmd.Run()` and `commitTurn`,
  call `claude.CopyTranscript(doc.Session, threadPath)`. Log copy
  errors to stderr but don't abort — the document edits in
  `content.md` are still worth committing. `commitTurn` already
  stages the whole doc dir via `request.DocDir`, so `thread.jsonl`
  rides along without changes there.
- Tests for `CopyTranscript` under a fake `CLAUDE_CONFIG_DIR`:
  transcript present, absent, and config dir missing entirely.

`CommitTurn`'s `ErrNothingToCommit` path still works: if the turn
produced no edits *and* no new transcript bytes, the staged diff is
empty. If claude wrote to the transcript but the document didn't
change, the commit lands on the thread alone — which is the correct
signal ("we talked, nothing crystallized yet").

## Why this shape

- **No new backend plumbing.** Claude Code already persists a
  structured log per turn. Copying it is a one-time `io.Copy` at
  session exit. The alternative — driving claude with `-p
  --output-format stream-json` and writing events ourselves —
  replaces an interactive terminal session with a headless one
  (operator loses mid-turn follow-ups, streaming colors,
  slash-commands, the editor integrations). That's the README's
  eventual plan, but it's a UX-breaking switch; this pass keeps the
  current `moe work` flow intact.
- **Glob by session id, not cwd encoding.** Claude Code's
  `<encoded-cwd>` transform is an implementation detail of its own
  storage layout (slashes → dashes today, could drift). Session ids
  are UUIDs and globally unique inside `<config>/projects/`, so
  `projects/*/<session-id>.jsonl` is robust without reimplementing
  anyone else's encoding.
- **Copy, don't symlink.** Symlinks would make `git` track whatever
  lives at `~/.claude/projects/...`, which is unstable (Claude Code
  may rotate, rename, compress). A copy severs that dependency —
  our repo holds a snapshot we control.
- **Overwrite, not append.** Claude Code rewrites the full session
  JSONL each turn (every turn's file includes all prior turns).
  Mirroring that — `thread.jsonl` always holds the full conversation
  — means the current file is self-contained, and the append-only
  semantics the README wants come from git history, not from the
  file itself. Rewinding is still `git reset --soft`; the thread
  rolls back with the content.
- **Raw Claude Code schema, not the README's blip schema.** The
  README's `{"id": "blip-001", ...}` shape was pre-implementation
  sketch. Claude Code's actual format is richer (tool_use,
  tool_result, thinking blocks) and normalizing to blips loses
  information. Keep the source-of-truth format; a renderer in
  `moe show` can pretty-print when that command ships. Flagged as a
  README drift to fix in a follow-up.

## Why not larger alternatives

- **Switch to headless stream-json now.** Kills the interactive
  terminal UX for a feature that can be had today without that
  sacrifice. If and when we need live per-token display or
  per-event notifications, the capture side is ready and we swap
  the invocation shape.
- **Normalize on copy into a blip schema.** Doubles scope (parser +
  schema + tests) and throws away fidelity. The transcript is for
  auditability; auditors want the raw events.
- **Teach `git revert` / `git reset` about thread.jsonl
  specifically.** Unnecessary. Git already handles both files the
  same way; the soft-reset story in the README already covers it.

## Tradeoffs

- **Depends on Claude Code's on-disk layout.** If Claude Code stops
  writing `<config>/projects/*/<session-id>.jsonl`, our copy
  silently returns `(false, nil)` and no transcript is saved. The
  document commit still lands. Worst case the feature goes dark
  until we chase the new location; it does not break `moe work`.
- **Thread includes tool events.** The JSONL carries tool_use /
  tool_result bodies, which for code-stage work can be large (file
  diffs, test output). That's useful audit material but bloats the
  bureaucracy repo. Acceptable: if it becomes a problem we can
  filter on copy, or adopt `.gitattributes` diff suppression for
  `thread.jsonl`. Not doing either in this pass.
- **No cross-machine continuity.** The transcript lives under
  `~/.claude` on whichever machine ran the session. Running the
  next turn on a different machine finds nothing to copy; the
  README's session-expiry recap path (README.md:356) remains the
  fallback. We haven't implemented that recap either; noting it as
  a known gap, not widening scope.
- **First turn before commit has no prior thread.** On a fresh
  session the copy happens right after claude exits and before the
  commit, so the first turn's transcript is captured as part of the
  very first commit. No special-casing.

## Scope

In:
- New `internal/claude` package with `TranscriptPath` and
  `CopyTranscript`.
- `request.ThreadPath` helper.
- Wire-up in `runWork`.
- Unit tests for the copy helper.

Out:
- `moe show` renderer for `thread.jsonl` (separate command, deferred).
- Session-expiry recap that injects the last N turns back into the
  prompt (README.md:356) — needs its own design.
- Switch to headless `claude -p --output-format stream-json`.
- Schema normalization (blip format), and the README drift that
  implies.
- Size policy for large tool_result bodies.
- Gitignored pre-commit staging (the transcript is tracked from
  the start; if that becomes noisy, revisit).

## Open questions

- Do we want to ignore `thread.jsonl` for `request.Scan`-driven
  aggregations like `moe dash`? Working assumption: no — dash
  reads `request.json` only, so the thread is invisible to it
  unless future commands explicitly open it. Revisit if dash gains
  a "last chatter" column.
- Should a copy failure (e.g., permissions) be fatal to `moe
  work`? Working assumption: no — warn to stderr, commit the
  document anyway. The operator's edits are the valuable state;
  losing a transcript is a degraded audit log, not a broken run.
