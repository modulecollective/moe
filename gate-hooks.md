# Gate hooks

Project-specific scripts MoE runs at gate points (e.g. before
`commitTurn`). Failures abort the turn, surface the captured output,
and stash it so the next session resumes already knowing why.

## Why

Every project has its own definition of "is this turn shippable":
`go fmt -l`, `go vet`, `go test`, `npm run lint`, schema validators,
license checkers. These are project-shaped, not workflow-shaped, so
they don't belong in moe's embedded fragments — they belong next to
the project they gate, in the bureaucracy.

The current code only has implicit gates (canvas-presence check in
`commitTurn`, dirty-tree refusals in `requireCleanTree`). A real gate
framework formalises the seam.

## Where hooks live

SysV-style drop-in dirs, no config file:

- `<root>/projects/<id>/hooks/<gate>.d/*` — project-scoped (the common
  case; `go fmt` etc.).
- `<root>/hooks/<gate>.d/*` — bureaucracy-wide (org licensing checks,
  trailer linters, anything that should fire across all projects).

Files are executable scripts; MoE runs them in lexical order. No
discovery beyond "is the file executable." Names like `10-gofmt`,
`20-vet`, `30-test` give the operator ordering control without a
config layer.

## Gate points (in priority order)

1. **`pre-commit`** — between Claude session exit and `commitTurn`.
   Most pressing gate. Cwd is the sandbox clone for code stages, the
   bureaucracy root for canvas stages. This is the gate that catches
   "agent edited the wrong way" before it lands as a turn commit.
2. **`pre-push`** — before `ffPushToDefault` / `openPRPath` in
   `mergePath` / `openPRPath`. Heavier checks (full test suite, race
   detector). Cwd is the sandbox clone.
3. **`pre-merge`** — before the `--ff-only` push to default. Same as
   pre-push but split out so an org can require, say, signed commits
   only on direct merges and not PRs.
4. **`pre-close`** — before `runClose` lands its commit. Runs at
   bureaucracy root. Likely niche; defer until a use case shows up.

Implement `pre-commit` first; the rest are the same shape.

## Execution model

For each script in lexical order:

- Spawn with stdio captured (stdout+stderr combined).
- Cwd: sandbox clone for code stages, bureaucracy root for canvas
  stages. Pass `MOE_PROJECT`, `MOE_RUN`, `MOE_DOC`, `MOE_GATE` as
  env vars so scripts that want context don't need to parse args.
- Per-script timeout (default 60s, overridable later). Hung script
  is killed, treated as failure with output "(killed after 60s)".
- Exit 0 → next script. Non-zero → stop, capture output, fail the
  gate.
- Hook itself failing to execute (file not found, permission denied)
  is a hard error to the operator — *not* fed to the agent, since
  the agent can't fix it.

## Failure handoff (no auto-loop)

Hook fails:

1. Print the captured output to stderr, prefixed with the hook name.
2. Print a retry hint matching the existing `promptNextStage` shape:
   `retry: moe <wf> <stage> <project> <run>`.
3. Write the failure record to
   `<root>/.moe/runs/<project>/<run>/<doc>.last-hook-failure`.
   Already gitignored (`.moe/` is). Single record per (project, run,
   doc); overwrites a prior one.
4. Abort the commit (return non-zero from `commitTurn` / the stage
   helper).

Operator drives the retry. No retry budget, no loop, no auto-resume.
The boring path earns its complexity later if it doesn't carry its
weight.

## Resume

`stageSessionOpts.InitialPrompt` already exists as the auto-sent
first-user-message slot. On stage session open, before building
the prompt:

1. Look for `<root>/.moe/runs/<p>/<r>/<d>.last-hook-failure`.
2. If present, read it, then delete it (read-and-clear, so a single
   re-run consumes the failure once).
3. Inject as the `InitialPrompt`, framed as system feedback, not
   user direction:

   ```
   MoE ran <hook> after your last edits and it exited <code>.
   Output:

   <captured output>

   Fix the issue. If it isn't actionable from inside this session,
   stop and ask the operator.
   ```

The agent walks back into the session via `--resume` already knowing
why. The conversation context still has its prior turns, so the
synthetic message fits cleanly as the operator's "here's what
happened" framing.

If the operator runs a *different* doc next (e.g. switches from
`code` back to `design`), the failure file for `code` stays put
until they come back to it — it's keyed on the doc, not the run.

## Non-goals

- **Auto-resume on failure.** Strictly more powerful but adds three
  classes of complexity (loops, framing hygiene, per-hook opt-in).
  Not until the manual loop is shown to be too slow.
- **A `post-commit` gate.** Once the commit lands, the artifact is
  in git history; whatever ran after it is just a separate command,
  not a gate.
- **Configurable timeouts in v1.** Default 60s, hard-coded. Add
  `MOE_HOOK_TIMEOUT` if it bites.

## Implementation seams

- New package `internal/hooks` (or fold into `internal/executor` if
  it stays small): `Run(gate, projectID, runID, docID, cwd) error`
  walks the drop-in dirs and runs scripts.
- `runWikiSession` (stage.go): call `hooks.Run("pre-commit", …)`
  between `executor.Execute` and `spec.CommitStager`. Failure
  short-circuits the commit and writes the failure file.
- Stage open (BuildSpec in `runStageSession`): read-and-clear
  `.last-hook-failure` for this doc; if present, override
  `opts.InitialPrompt` with the failure framing (or compose with
  the existing kickoff prompt — TBD which reads better).
- `.gitignore` already covers `.moe/`; nothing to add there.

## Testing

- Unit: `hooks.Run` against a tempdir with crafted scripts (success,
  failure, hang, missing exec bit, ordering).
- Integration: a stage-session test that drops a failing hook in
  `projects/<id>/hooks/pre-commit.d/`, runs the session, asserts
  the commit didn't land and the failure file exists.
- Resume test: pre-seed `.last-hook-failure`, open the stage,
  assert `InitialPrompt` carries the failure framing and the file
  is gone after.
