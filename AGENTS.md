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

## Operator-local config

`moe config` (list/get/set/unset) reads and writes `.moe/config.json`.
Keyspace is closed — see `internal/config/keys` for the allowlist.
Adding a key is a code change plus one entry; nothing on disk is
free-form. Today the only key is `default_agent`, which slots between
`$MOE_AGENT` and the `"claude"` hard default in `resolveAgentName`.

## Tools worth reaching for

Go's off-putting CLIs are agent superpowers — the ergonomics that make
them awkward for humans make them clean for tools.

- `go doc <pkg>` / `go doc <pkg>.<Symbol>` — the fastest way to check a
  signature or a package's public surface. Reach for it before guessing
  an API, including on the stdlib.
- `gopls` — find-references, go-to-def, and workspace symbol search.
  Beats grepping by name when navigating unfamiliar code; catches
  shadowed names and renames grep misses.
