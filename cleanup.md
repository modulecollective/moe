# Cleanup backlog

Findings from a principal-engineer pass over the codebase. Grouped by
severity. Each item is shaped to feed into MoE as an idea or a small
run on its own — file paths and line numbers included so the agent
doesn't have to rediscover them.

Tracked as one followup per item under MoE run `pe-cleanup-list-redux`.
This file is removed once every item is verified done.

## Real issues

### `git status --porcelain` parsed without `-z` in five places

- `internal/wiki/finalize.go:221`
- `internal/cli/followups.go:235`
- `internal/cli/push.go:350`
- `internal/run/run.go:686`
- `internal/cli/sync.go:261`

Without `-z`, git core-quotes paths with spaces, unicode, or control
bytes (e.g. `?? "my file.md"` or `?? "fa\303\247ade.md"`). Code reads
`path := line[3:]` and treats the result as a literal filesystem
path. `dirtyOutsidePath` even hand-strips `"` quotes but doesn't
unescape `\…`. Latent today (slug-pattern paths only), fires the
moment a topic title contains a space or non-ASCII byte.

Fix: switch every site to `--porcelain=v1 -z` and split on NUL. A
small `internal/git` helper is the natural home so the parsing
logic exists once.

### Three duplicated session-cleanup blocks in `runWikiSession`

`internal/cli/stage.go:296`, `:313`, `:349`. Same
`_ = withRepoLock(...) { return session.Close(sess) }` pattern. Easy
to miss one when adding a failure path. Refactor to a `defer` with a
"released" flag, or an `openSessionUnderLock` helper that returns a
`func()` to schedule.

### Cross-package duplicates

- `runGit` / `runGitOut` / `runGitCaptured` defined in
  `bureaucracy/`, `run/`, `project/`, `session/`, and
  `cli/sync.go` — five slightly different signatures.
- `gitRevParse` in `internal/cli/sync.go:411` and
  `internal/session/session.go:345`.
- `shortSHA` in `cli/sync.go` (7 chars) and `wiki/reflect.go` (12
  chars). Different cuts in different output contexts.

A small `internal/git` package wrapping `exec.Command("git", …)`
with the project's stdio conventions would centralise these and
collapse `shortSHA` into one function.

### `moe dash` does N×M git work on every render

`internal/cli/dash.go:buildTwinRows → twinStatusNote →
closedRunsSinceCount` calls `run.Scan(root)` and then
`run.LastActivity(root, md.ID)` (one `git log --grep` per run) for
each project — and `buildDashRows` already scanned. Fine on a tiny
bureaucracy; latency on every dash render as the run count grows.

Cache `[]Metadata` and a last-activity map at the top of `runDash`
and pass them down.

### Repolock corrupt-record path leaves `TimeoutError` empty

`internal/repolock/repolock.go:163-203`. When `readRecord` fails
with anything other than `os.ErrNotExist`, the code falls through
into the live-holder branch with `existing` as the zero `Record`.
If the budget runs out, `TimeoutError.Holder` has empty
owner/purpose/heartbeat fields and the message reads
`held by  for "" (no heartbeat info)`.

Either re-read on next iteration or treat unparseable as a
stale-after-settle case explicitly.

### `repolock.processAlive` treats EPERM as dead

`internal/repolock/repolock.go:417`. `Signal(0)` to a process owned
by a different user returns EPERM, and the code only counts `nil`
as alive. So if two users on the same host run `moe`, one can take
over the other's live lock. Niche (single-operator design), but
worth a comment acknowledging it or
`if errors.Is(err, syscall.EPERM) { return true }`.

## Smaller things

- **`commitTurn` stages twice per turn.** `stage.go:740` then
  `:777`. The first staging satisfies the canvas-presence check;
  the second re-runs `git add` on the same paths. Trivial perf
  cost; cleaner to stage once and gate the precondition on
  `os.Stat` of the canvas before staging.
- **`internal/session/session.go:345 gitRevParse` is unused** in
  the package — only referenced from the package-level test. Drop
  it or move to the test file.
- **`buildSystemPrompt` separator collisions.** Joins sections with
  `"\n---\n\n"`. If any section ends without a trailing newline the
  separator collides with the body. Each section currently ends
  with `\n` by convention; assert it via a test or normalise via a
  helper.
- **`promptNextStage` allocates a fresh `bufio.NewReader(os.Stdin)`
  each call.** Only matters if invoked twice in one process (current
  paths don't, but the chain rule could grow). Use one reader at the
  dispatcher level if it ever becomes a problem.
- **`hostname() == "unknown"` collisions** in
  `repolock.ownerString`. If `os.Hostname` fails, multiple machines
  all become `unknown/<pid>` and can spuriously look like the "same
  host" for pid-alive checks. A UUID fallback cached in
  `.moe/instance-id` would make ownership unambiguous. Likely never
  triggers.
- **`launchEditor` shells through `sh -c` with `EDITOR`
  interpolated.** Conventional and correct (honors
  `EDITOR="vim -X"`). Path is properly passed via `$1` rather than
  interpolated — that's the load-bearing safety detail. One-line
  comment so a future change doesn't collapse `$1` back into the
  format string.
- **Commit-message trailer scaffolding is duplicated.** Stage,
  lint, reflect, claim, push, sync all emit the same
  `MoE-Run`/`MoE-Project`/`MoE-Workflow`/`MoE-Document` block. A
  small `internal/trailers` helper that takes a struct and emits
  the canonical block reduces typo risk and makes adding a new
  trailer one place to edit.

## Out of scope for this list

- Architecture (stdlib-only, single repo lock, per-run COW
  sandbox, trailer-driven journal) — sound, leave alone.
- Workflow/prompt relocation — separate design (see the
  layered-overlay direction).
- Gate hooks — separate design (`gate-hooks.md`).
