# ARCH-CLEANUP

High-level architecture review of the moe CLI. Big structural issues only —
no small cleanup items.

> **Prompt that produced this review:**
> *"Let's do a high level architecture review as if you are a principal
> engineer. I don't want small cleanup stuff just big structural things."*

---

## 1. `internal/cli` is becoming a god package

Roughly **21k of 32k Go LOC (65%) live in `internal/cli`**. The other "engine"
packages (`run`, `wiki`, `session`, `executor`, `repolock`, `git`, `project`,
`sandbox`) are small, focused, and clean — that part is healthy. But `cli/`
now contains substantial business logic that isn't really CLI dispatch:

- `cli/push.go` (710 LOC): rebase-onto-default, GitHub PR creation,
  merge-then-delete.
- `cli/sync.go` (590 LOC): submodule pointer bumping, PR-state reconciliation,
  status finalization.
- `cli/dash.go` (770 LOC): bucket classification, journal-index mining,
  twin/idea aggregation.
- `cli/idea.go` (610 LOC), `cli/queue.go` (638 LOC): full workflows
  implemented inline.

The `wiki` package shows what the right shape looks like — a domain package
with `Config`, `FinalizeIngest`, `Scan`, `RenderFindings`, etc. There's no
equivalent for `push`, `sync`, `dash`, `idea`, `queue`. **Pull each of those
into its own `internal/<name>` package and leave thin handlers in `cli/`** —
that's the single biggest structural lever available.

## 2. The `Workflow` abstraction is overloaded

`Workflow` is doing two unrelated jobs:

- **UX grouping**: a top-level verb with subcommand dispatch.
- **Stage DAG**: prereqs, successors, "what's next?" via `Next()`.

Most of the things registered as workflows aren't DAGs:

- `queue`, `project`, `session` aren't workflows at all — they're command
  groupings using `RegisterFacade`.
- `idea` *registers* a Workflow but bypasses its dispatcher (`runIdea` does
  its own switch in `idea.go:67-90`); the Workflow exists only so `run.Load`
  and `dash` can resolve it. The single stage is a `Hidden` stub that errors
  when invoked.
- `quick` is a one-stage "DAG" — same shape as idea.

This makes the abstraction lie about itself. Two cleaner options:

- Split into `Workflow` (DAG, stage progression) and `CommandGroup` (UX
  nesting). Today they're conflated and you can feel it in `idea.go` and
  `queue.go`.
- Or accept that everything is a `CommandGroup` and move the stage-DAG bits
  into a small data structure that lives next to `run.Metadata` — closer to
  where Next/satisfaction is actually needed.

## 3. The git boundary is not enforced

There's `internal/git` with index-lock retry, consistent error wrapping, and
a parsed `Status`. But **37 raw `exec.Command("git", ...)` calls live outside
that package** (push, dash, init, sync, run, project, sandbox, wiki/finalize,
wiki/claim, wiki/reflect, bureaucracy). Concretely that means:

- `git.Run`'s `index.lock: File exists` retry — silently skipped by anything
  bypassing it. That race exists for moe specifically (concurrent worktree
  access) and bypassing callers will flake.
- Error formatting is ad-hoc per site.
- Tracing/timing/debug instrumentation has no single chokepoint.

This is a bug factory in slow motion. Either expand `internal/git` to cover
the real shapes (worktree ops, log queries, push/pull, ls-remote, submodule)
and require everyone go through it, or accept the scattering and delete
`internal/git` so there's no false invariant. The current half-measure is
the worst of both.

## 4. Dual state model: `run.json.Status` vs git-trailer derivation

A run's truth lives in two places:

- `Metadata.Status` field in `run.json`
  (StatusInProgress/Pushed/Merged/Closed/Promoted).
- Stage progression *derived* from `MoE-*` trailers via log scans
  (`LatestWorkTurnSHA`, `BuildJournalIndex`, `Workflow.stageSatisfied`).

`sync.reconcilePushedRuns` exists precisely to reconcile drift between the
two — every "the dashboard says X but git says Y" bug will live in this
seam. It's not wrong to have both (status is cheap to read; trailers are
the journal of record), but it's worth deciding which one is canonical and
treating the other as a derived cache. Right now it's not clear which is
which, and the reconciliation logic is buried in `sync.go` rather than being
a first-class operation.

A related concern: stage progression scans `git log` on every dash render
and every `Workflow.Next` call. `JournalIndex` amortizes this within one
command, but at hundreds of runs this becomes noticeable. Worth deciding now
whether to materialize an index file or keep it scan-on-demand.

## 5. Stage orchestration is a callback ball

`runStageSession` in `cli/stage.go` is the central seam every stage flows
through. As new stage shapes have arrived, they've been bolted onto
`stageSessionOpts` as optional fields:

- `NeedsSandbox` (code stages)
- `Headless` (oneshot)
- `SkipNextStage` (oneshot chains)
- `WikiBuilder` (kb summarize, twin reflect)
- `ExtraStagePaths` (meta-moe)

Each one is "this stage type does something different." Per `soul.md`
("three similar lines is not a pattern worth abstracting"), holding off on a
`StageEngine` interface for now is defensible — but the *next* knob is the
one that justifies extracting an engine. Probably the moment a stage needs
to compose two of these behaviors (e.g. wiki + extra-paths + headless) the
callback soup will be unreadable. The wiki package is half-extracted;
finishing that extraction (giving code/design their own `Engine` analog) is
the principled move.

## 6. Test seams via mutable globals

Several core entry points are package-level `var`s so tests can swap them:

- `runStageSession` (cli/stage.go)
- `openCodeSessionForHookFailure` (cli/hooks.go)

That works but it's a code smell that says *"this function has no clean DI
surface"*. The fix isn't `interface StageRunner`; it's #1 above — once
push/hook-failure logic moves to engine packages, those packages can take
dependencies as struct fields and tests construct them with stubs, no
globals needed.

---

## Priority order

1. **Extract push/sync/dash/idea/queue into engine packages** (#1). Single
   largest reduction in `cli/` mass and unlocks #6.
2. **Decide canonicality between `run.Status` and trailer-derived state**
   (#4); document which is the source of truth and treat the other as cache.
3. **Pick a position on `internal/git`** (#3): expand it to cover all real
   call shapes and forbid raw `exec.Command("git", ...)` outside it, or
   delete it.
4. **Split `Workflow` into `Workflow` + `CommandGroup`** (#2) once a third
   non-DAG grouping shows up — today's two (queue, project) are arguably
   tolerable.
5. **Defer the StageEngine extraction** until a stage actually needs to
   compose two of the existing knobs (#5) — but watch for it.

## Tradeoffs

- #1 is a multi-week refactor with no user-visible win; the payoff is that
  the next two features don't pile more into `cli/`.
- #3 is small but invasive (touches every git callsite).
- #2 has no urgency; the lie is annoying but not breaking anything.
- #4 is the one with the most latent bug surface — that's where the next
  architectural day is best spent.
