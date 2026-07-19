# Agents working on moe

This file applies when you are editing the moe CLI itself (this repo).
It does not apply to agents dispatched by moe to work on target
projects — those get their own guidance assembled from `soul.md` and
`stages/*`.

## Ground rules

- **Stdlib only where practical.** moe is a thin wrapper around
  `claude -p`, `git`, and the Go standard library. No YAML parser, no
  CLI framework, no DAG engine, no third-party dependency without a
  reason that survives review.
- **All `git` calls go through `internal/git`.** That package owns
  the index-lock retry, the error shape, and the tracing hook —
  bypassing it skips all three. Reach for `git.Run` / `Output` /
  `Combined` / `Probe` / `Stream`, or one of the typed wrappers
  (`HEAD`, `HasRef`, `Upstream`, `AheadOf`, `LsRemoteDefault`,
  `RevParse`, `Status`). Raw `exec.Command("git", …)` outside that
  package fails CI.

## Before you say you're done

**Run `gofmt -l -w . && go vet ./...` at the end of every round of Go
edits.** Not optional — not even for a one-line fix, not even if the
tests already pass. If you're about to write "fixed it" or "done",
you're about to run these two commands first.

## Running moe itself

In code stage, don't run `moe` itself. It's easy to screw up state
unless you configure things exactly right, so do implementation testing
through Go's tests only.

In test stage, it is OK to run `moe` when the test plan calls for an
end-to-end CLI path. Use the run's configured dev environment, keep the
invocation scoped to the surface under test, and record the command and
result on the test canvas.

When the surface under test sits behind an agent turn — the rendered
prompt text a stage hands its agent, the sandbox boundary gate — don't
spend a live session. Put a **fake `claude` early on `PATH`** that dumps
its argv to a file and writes the stage canvas itself. Both backends
resolve their binary through `exec.LookPath` — the same seam the
`fakeClaudeOnPath` test helper uses — so real `moe sdlc` / `twin` /
`pulse` invocations against a scratch bureaucracy run end-to-end with no
session spent. Two gotchas: stage ladders are ordered (walk from the
first stage), and the scratch bureaucracy needs `.claude/` and
`.mcp.json` in `.git/info/exclude`. Full recipe: the digital twin's
operations.md, "Driving `moe` end-to-end without an agent session".

In test stage, **don't** spawn `moe serve` to check rendered HTML or
HTTP status — assert it in-process with `httptest` against
`s.Handler()` (the existing `internal/serve` test idiom). If a live
server is genuinely unavoidable, run it inside a single Bash call:
`serve & PID=$!; <readiness-poll>; <probe>; kill $PID; wait`. Never
`run_in_background` a server and never a bare blocking `moe serve` —
both detach into their own network namespace (curl from a later call
can't reach them) and wedge the turn. Browser- or TTY-only checks
can't be curled at all; record them under `What wasn't verified`,
don't defer them to a human.

## Tools worth reaching for

Go's off-putting CLIs are agent superpowers — the ergonomics that make
them awkward for humans make them clean for tools.

- `go doc <pkg>` / `go doc <pkg>.<Symbol>` — the fastest way to check a
  signature or a package's public surface. Reach for it before guessing
  an API, including on the stdlib.
- `gopls` — find-references, go-to-def, and workspace symbol search.
  Beats grepping by name when navigating unfamiliar code; catches
  shadowed names and renames grep misses.
