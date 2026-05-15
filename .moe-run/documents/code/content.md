# Code: cwd inversion (option B)

Lands the design's option B: per-run sandboxes become object-shared
`git clone --local --shared` directories (no longer linked worktrees
of the canonical submodule), cwd flips to the bureaucracy session
worktree on code stages, and the `./.moe-run/` shuttle goes away.

## What changed

Implementation followed the design's "First implementation steps"
list, with one addition (project AGENTS.md / CLAUDE.md injection)
folded in so projects don't silently lose their guidance under the
inverted-cwd shape.

- **`internal/sandbox/sandbox.go`** — `EnsureAt` now runs `git clone
  --local --shared --no-checkout <src> <absDst>` plus `git checkout
  HEAD`. The resulting clone has a real `.git/` directory; objects
  are hardlinked / referenced via `objects/info/alternates`, so no
  network and near-zero disk. `Remove` drops the canonical-side
  worktree-prune and just `os.RemoveAll`s the dir — a plain clone
  has no registration to clean up. Idempotency check switched from
  `git worktree list --porcelain` to "does the destination have a
  `.git/` already?".
- **`internal/agent/codex/codex.go`** — `resolveCwd` flips for
  code-bearing stages: cwd = bureaucracy session worktree, clone is
  reached via `--add-dir`. `commonArgs` emits the clone as an
  additional add-dir when `clonePath != ""`.
- **`internal/agent/claude/executor.go`** — symmetric flip: `cmd.Dir`
  for both `Execute` and `ExecuteOneShot` lands at `r.Root`; the
  clone joins the add-dir list.
- **`internal/cli/stage_prompt.go`** — `operationalCore` drops the
  `./.moe-run/` indirection; canvas, followups, and twin feedback
  revert to absolute bureaucracy paths (the same paths
  document-only stages used). The "your working directory is the
  clone" framing flipped to "the clone is exposed via add-dir; your
  cwd is the bureaucracy session worktree."
- **`internal/cli/stage.go`** — removed the `syncRunIntoClone` /
  `excludeCloneRun` pre-turn calls and the `syncRunFromClone`
  post-turn call. The agent writes the canvas directly at its
  natural path; commitTurn reads it back from the same location.
- **`internal/cli/clone_canvas.go`** + tests — deleted. The shuttle
  no longer has a reason to exist.
- **`internal/cli/follow.go`** — `resolveFollowTarget` now points
  `Canvas` at `<sess.WorktreePath>/projects/<p>/runs/<r>/documents/
  <doc>/content.md` for sandbox stages, matching where the agent
  actually writes. `Dir` still points at the sandbox clone so
  source-tree diffs anchor to the clone's branch.
- **`internal/cli/stage_prompt.go` (continued)** — added
  `projectAgentsGuidance`: reads `<clonePath>/AGENTS.md` and
  `<clonePath>/CLAUDE.md` when present and injects them into the
  system prompt as a "## Project guidance" section. Replaces
  codex's / claude's native cwd-walk discovery, which no longer
  reaches the clone-rooted files under the inverted shape.

## Tests

- Rewrote `internal/sandbox/sandbox_test.go` to assert "the clone
  has a real `.git/` directory" (`isPlainCloneAt`) instead of "the
  clone is registered as a worktree of the canonical." Same
  end-to-end behavior (writes don't leak back to source, canonical
  main isn't advanced, Remove is idempotent) under the new
  primitive.
- Updated `internal/agent/codex/codex_test.go` to expect
  `--add-dir <clonePath>` alongside `--add-dir <root>`.
- Updated `internal/agent/claude/executor_test.go` with a new test
  (`TestExecuteArgsAddsClonePathForCodeStages`) pinning the new
  add-dir order.
- Rewrote `internal/cli/stage_prompt_test.go`
  (`TestOperationalCoreCanvasPathIsAbsoluteAcrossStages`) to assert
  the prompt names absolute bureaucracy paths for *both* code and
  doc stages, and that no `./.moe-run/` shuttle paths leak back.
- Updated `internal/cli/follow_test.go` to expect canvas under the
  session worktree (not the clone's `.moe-run/`).
- `go test ./...` is green end to end.

## What's not in this diff

- **No deletion of the dev-env hook scripts.** The audit confirmed
  every script under `projects/moe/hooks/dev-env.d/*` reads
  `$MOE_SANDBOX` / `$MOE_HOME` rather than `pwd`, so the inversion
  doesn't break them. No files to change.
- **No `internal/push/*` changes.** With plain clones the run's
  branch lives in the clone's local ref-db; `EnsureOrigin`
  overrides the file:// clone-source origin with the project's
  GitHub remote before the push, and every subsequent operation
  works against that. Nothing in the bureaucracy depends on a
  `moe/<run-id>` ref existing in the canonical submodule's ref-db.

## Risky hunks for review

1. **`internal/sandbox/sandbox.go` `EnsureAt`** — the load-bearing
   primitive swap. `git clone --local --shared --no-checkout <src>
   <absDst>` then `git checkout HEAD` mirrors the worktree
   primitive's "fresh working tree at HEAD" shape, but the
   geometry's different: `.git/` is now a real dir with its own
   refs (the canonical doesn't see this run's branch anymore).
   Side-effect on the canonical's ref-db: gone. Side-effect on
   `git worktree list --porcelain` from the canonical: gone too.
   Anything still expecting the run's branch to be visible from
   the canonical would silently see nothing — none found in the
   walk, but worth a second eye.
2. **`internal/agent/{claude,codex}` cwd flip** — what every code
   turn lives in now. Misroute = the agent edits files in the
   bureaucracy thinking they're project source, or vice versa.
   The `--add-dir <clonePath>` keeps the clone reachable; the
   system prompt names both paths so the agent can tell them
   apart. Worth eyeballing the operationalCore prose for
   ambiguity.
3. **`projectAgentsGuidance` injection** — new code path that
   reads up to two files from the clone on every code-turn prompt
   build. Bounded I/O (one stat + one read per file), reads only
   from a directory the operator already trusts. If a project
   doesn't ship either file, no section is emitted — exists check
   is silent.

## Followups

- Architecture twin needs to reflect "per-run sandbox plain
  object-shared clone" as the load-bearing primitive — captured in
  `feedback/twin.md`.
- The `--shared` alternates link couples a run clone's lifetime to
  the canonical submodule's object store; MoE doesn't prune today
  so it's theoretical, but worth a `roadmap.md` note when prune is
  considered.

## Test plan

What to drive in test stage:

1. **Headless code-stage run on widget** — `moe sdlc code widget
   <run>` headless. Confirm: canvas, followups, and twin feedback
   land at their absolute bureaucracy paths post-turn, the agent
   doesn't hit "patch rejected" on either codex or claude, and
   `git status` in the clone shows no `.moe-run/` directory.
2. **Live re-runs of the parent run's failing repros** — `moe sdlc
   code moe shell-sources-dev-env-2026-05-15` and similar. These
   were the operator's load-bearing complaints; if they still hit
   apply_patch rejection the implementation is incomplete.
3. **Push end-to-end against a real project** — `moe sdlc push
   widget <run>`. Confirm: the run's branch on the clone makes it
   to GitHub via `EnsureOrigin` overriding origin, the rebase
   pre-flight finds origin/main fine, and the fast-forward into
   default succeeds. With the new primitive the run branch isn't
   in the canonical's ref-db, so a sharp-eyed reviewer should
   verify nothing in push depended on that visibility.
4. **Named workspace re-attach** — open a run that uses
   `--workspace foo`, code on it, close, open a second run on the
   same workspace, code, push. Confirms workspaces still work
   under the plain-clone primitive (they share `sandbox.EnsureAt`,
   per the architecture decision below).
5. **AGENTS.md / CLAUDE.md surfacing** — code-stage on the moe
   project, confirm the agent sees the moe AGENTS.md ground rules
   in its system prompt (look for "stdlib only", "internal/git is
   the sole seam" — the load-bearing rules). If the project
   guidance section is missing, the injection didn't fire.

Outside automated coverage:

- The "object-shared clone, hardlinked objects" property — visible
  in `objects/info/alternates` content, not asserted by tests.
- Concurrent runs sharing the canonical submodule's object store.
- Live codex / claude behavior under the inverted shape; the unit
  tests pin args + paths, not the agents' end-to-end behavior.
