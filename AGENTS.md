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

## Before you say you're done

**Run `gofmt -l -w . && go vet ./...` at the end of every round of Go
edits.** Not optional — not even for a one-line fix, not even if the
tests already pass. If you're about to write "fixed it" or "done",
you're about to run these two commands first.

## Tools worth reaching for

Go's off-putting CLIs are agent superpowers — the ergonomics that make
them awkward for humans make them clean for tools.

- `go doc <pkg>` / `go doc <pkg>.<Symbol>` — the fastest way to check a
  signature or a package's public surface. Reach for it before guessing
  an API, including on the stdlib.
- `gopls` — find-references, go-to-def, and workspace symbol search.
  Beats grepping by name when navigating unfamiliar code; catches
  shadowed names and renames grep misses.
