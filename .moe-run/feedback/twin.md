architecture.md:155-156 and :365-366 describe the per-run sandbox
as a `git worktree` linked off the canonical submodule. The design
in this run recommends switching to `git clone --local --shared`
(option B) because codex's apply_patch enforces a
submodule/superproject boundary that the worktree shape triggers
but a plain clone does not. If (B) ships, both passages need to
change — the sandbox primitive becomes "a plain clone of the
canonical submodule with hardlinked / shared objects," not a
worktree. Also affects the "Per-run sandbox worktree" lifecycle
entry at :365 and the auto-init copy at :159-160 (which is about
submodule init, not worktree add, so probably stays).

---

(B) shipped this run. Concrete updates the next reflect should
fold into architecture.md:

- ## Components → `**sandbox**` bullet: replace "implemented as a
  `git worktree` linked off the canonical submodule" with
  "implemented as a `git clone --local --shared --no-checkout`
  against the canonical submodule path, with objects hardlinked
  or referenced via `objects/info/alternates`." Drop the "OS-
  level sandbox / gitdir lives outside the working tree" sentence
  — gitdir is now inside the clone at `<dst>/.git/`. The
  `executor` bullet's `--settings` widening for the gitdir was
  removed: `sandboxSettingsJSON` is now a single constant emitting
  `{"sandbox":{"enabled":true}}`. The bullet's "additionally widens
  `sandbox.filesystem.allowWrite` to the worktree's gitdir"
  sentence should come out — under the plain-clone primitive the
  clone's gitdir lives inside the clone at `<clonePath>/.git/`
  (reachable via `--add-dir <clonePath>`), and the bureaucracy
  session worktree's gitdir at `<root>/.git/worktrees/<uuid>/` is
  reachable via the `--add-dir <root>` claude already passes. No
  per-payload widening is needed for index-mutating git operations.
- ## Decisions → `**Per-run sandbox worktree.**` bullet: rename
  to `**Per-run sandbox plain clone.**`. Replace the "git
  worktree linked off the canonical" line with the plain-clone
  framing. Rationale shifts from "per-run index isolation" to
  "removes the moe-submodule ↔ bureaucracy gitdir boundary that
  codex's apply_patch enforces." Keep the "two runs never touch
  each other's index, refs, or objects" claim — still true under
  the new shape (separate clones, not separate worktrees of one
  canonical).
- ## Decisions → `**Named workspaces for dev-server warm state.**`
  bullet: still accurate (workspaces still share `sandbox.EnsureAt`,
  which is now the plain-clone primitive). One sentence noting
  "warm state survives the branch switch via the same plain-clone
  primitive sandboxes use" would tighten the connection.
- Add a new ## Decisions bullet for cwd-direction:
  `**Code-stage cwd is the bureaucracy session worktree.**` —
  agents run with cwd = bureaucracy session worktree and reach the
  project clone via `--add-dir`. The canvas / followups / twin
  feedback write at their natural absolute bureaucracy paths;
  source-tree edits land under the clone path. The previous
  shuttle in `internal/cli/clone_canvas.go` was the workaround for
  the worktree-primitive's gitdir boundary; the plain-clone +
  inverted-cwd combination makes the workaround unnecessary.

The shuttle code (`clone_canvas.go` + tests) was deleted with the
primitive swap. If any patterns.md anti-pattern named "the
`./.moe-run/` shuttle" as the answer to cross-boundary writes, the
entry should be removed (or rewritten to point at the
plain-clone primitive instead).

---

`projects/moe/AGENTS.md` (and any per-project CLAUDE.md) is now
loaded into the system prompt explicitly via
`projectAgentsGuidance` in `internal/cli/stage_prompt.go` rather
than via codex / claude's native cwd-walk discovery. Worth a
patterns.md or operations.md note: the canonical home for
project-specific agent guidance is still
`projects/<p>/src/AGENTS.md` and `projects/<p>/src/CLAUDE.md`, and
MoE surfaces both regardless of where cwd lands. The cwd-walk
mechanism the codex docs describe doesn't fire under the inverted
shape; MoE substitutes an explicit read instead.
