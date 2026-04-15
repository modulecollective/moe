# Design: notify the code-stage agent when prerequisites moved

## Problem

When the operator unsigns `design`, the cascade in
`runSignUnsign` (internal/cli/sign.go:129-154) unsigns `code` too,
and the next `moe work` rebuilds the system prompt with
`stages/design.md` instead of `stages/code.md`. The *outer* loop
notices.

The agent itself does not. `runWork` resumes the same Claude Code
session via `--resume <uuid>` (work.go:110-113), so the conversation
history — and Claude's working model of "what the design says" — was
built against the old design. Nothing in `operationalCore`
(work.go:194-225) tells Claude that the design moved. The
`stages/code.md` fragment line 11 ("Match the design") leaves the
agent to notice on its own. It won't.

The same problem occurs without a cascade: a prerequisite can be
re-signed while the dependent was never signed in the first place
(common: design is being iterated while no code work has happened
yet, then design is re-signed before the first code turn). There's
still upstream drift the agent should know about; there just wasn't
a cascade to telegraph it.

## What to do

When `operationalCore` runs for a stage that has prerequisites,
detect any prerequisite whose latest `MoE-Stage-Signed: <name>`
commit is newer than this document's most recent `work: update
<docID>` commit. For each, append a section to the system prompt
that gives the agent the three things it needs to investigate:

- which prerequisite stage moved and when,
- the absolute path to the prerequisite document's `content.md`
  (same shape as the canvas pointer already in the prompt),
- the bureaucracy SHA the agent last ran against, so it can run
  `git -C <root> diff <sha>..HEAD -- <path>` and see exactly what
  changed.

Concretely:

- `internal/stage/stage.go`: add a helper that returns the latest
  `MoE-Stage-Signed: <name>` commit's SHA and committer time for a
  request — symmetric with the existing `latestTrailerTime`, just
  carrying the SHA too.
- `internal/request/request.go`: add a helper that returns the SHA
  of the most recent `work: update <docID>` commit for the request
  (filtered by the existing `MoE-Document` trailer). Returns `""`
  when no work turn has happened yet.
- `internal/cli/work.go`: extend `operationalCore` (or split out a
  sibling) to compute the banner and append it. Pass
  `--add-dir <root>` to the `claude` invocation so the agent can
  read bureaucracy files and run `git -C <root> ...` without a
  per-call permission prompt.
- `internal/cli/work_test.go`: extend with a fixture journal where
  design is signed, a code work turn happens, design is re-signed,
  and assert the banner appears with the right SHA and path.

## Why this shape

- **Path + SHA, not inlined content.** Inlining the prerequisite
  document into the system prompt is real, but it's the upstream-
  document-assembly pass the README is already foreshadowing
  (work.go:158-165, README.md:1326). Bundling that work here
  doubles scope. Pointing at a path matches what
  `operationalCore` already does for the canvas, and the SHA gives
  the agent a precise diff target — better than reading the file
  cold.
- **No new persisted state.** "Last work turn SHA" is derivable
  from `git log` filtered by the trailers we already write on
  every turn (`commitTurn`, work.go:271-282). Adding a
  `LastSeenSHA` to `Document` would duplicate journal truth into
  JSON — the exact anti-pattern the package comment in
  `stage/stage.go` lines 4-7 calls out.
- **Detect every turn, not just after cascade.** A prerequisite
  can be re-signed without the dependent having been signed (so no
  cascade). Comparing "latest prereq sign time" against "last work
  turn time" catches cascade and non-cascade paths uniformly, with
  one rule.
- **Stage-agnostic helper.** Today only `code` has prerequisites,
  so the rule is operationally trivial. Writing it against
  `stage.Requires` instead of hard-coding `design`→`code` means
  any future stage with prerequisites gets the banner for free.

## Why not larger alternatives

- **Force a fresh session on cascade.** Throws away the useful
  conversation context with the stale bits. Hold this in reserve;
  reach for it only if the banner gets ignored in practice.
- **Inline upstream document content.** Real and probably right
  eventually, but it's the upstream-document pass. Track it as a
  follow-up.

## Allowed-tools posture

Today `runWork` passes no `--allowedTools` flag — Claude Code runs
with default permissions. The README plan (README.md:1369, 873-882)
is to tighten this per-document later. Two things to settle now so
the banner is usable:

- **Bureaucracy filesystem access.** Pass `--add-dir <root>` on the
  `claude` invocation. The canvas is already being read/written
  from outside the sandbox-clone cwd, so this formalizes existing
  reality. When the per-document `--allowedTools` work lands, the
  code-stage allowlist will need to keep this access.
- **`git diff` against the bureaucracy.** Until the broader
  `--allowedTools` work lands with a `Bash(git -C <root>
  diff:*, log:*, show:*)` shape, the agent will still get a Bash
  permission prompt the first time it runs the suggested diff.
  Acceptable; tighten with that broader effort.

## Tradeoffs

- **Wider filesystem reach.** `--add-dir <root>` lets the agent
  browse the whole bureaucracy, not just the canvas and the named
  prerequisite doc. That's the right capability for "go diff and
  read related context," but it's a step away from least-privilege
  until `--allowedTools` lands.
- **Banner is advisory.** Nothing forces the agent to actually run
  the diff or re-read the file before continuing. The contract is
  still social (`stages/code.md` line 11). If we find agents
  ignoring it, the next move is the fresh-session option above —
  not making the banner louder.

## Scope

In:
- Detection helpers in `stage` and `request`.
- Banner injection in `operationalCore`, gated on the request's
  current stage having unmet "prereq moved since last turn"
  conditions.
- `--add-dir <root>` on the `claude` invocation.
- A test covering the design-resigned-after-code-turn case.

Out:
- Per-document `--allowedTools` shaping (README plan, separate
  effort).
- Inlining prerequisite document content into the prompt.
- Operator-side notification (desktop notification, status line,
  etc.) when the state is detected.
- Schema changes to `Document` or `request.json`.

## Open questions

- If a prerequisite was re-signed but the document has had no work
  turns yet, do we surface anything? Working assumption: no — there
  is no "since" to compute against, and the agent will read the
  document fresh on its first turn anyway. If this turns out to
  matter, revisit.
- Should the banner cite all prerequisites that moved, or only the
  most recent? Working assumption: all of them, in dependency order.
  Cheap to render, and a chain like `spec → design → code` (when it
  exists) shouldn't hide an upstream change behind the immediate
  one.
