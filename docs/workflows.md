# Workflows

A workflow is a small ladder of stages; a run is one pass through that
ladder. This page is how to drive each one. For what runs, canvases, and
sandboxes are, see [concepts.md](concepts.md); for the command catalog and
environment reference, see [reference.md](reference.md).

## SDLC

`moe sdlc` is the main software-development workflow:

```sh
moe sdlc new [--workspace <name>] [--agent <name>] <project>/<slug>
moe sdlc design [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc code   [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc review [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc test   [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc push [--pr] <project>/<run>
moe sdlc shell  <project>/<run>
```

A full pass spelled out by hand:

```sh
moe sdlc new my-project/add-batch-support
moe sdlc design my-project/add-batch-support
moe sdlc code my-project/add-batch-support
moe sdlc review my-project/add-batch-support
moe sdlc test my-project/add-batch-support
moe sdlc push my-project/add-batch-support
```

The `new` command opens the run and writes the first files into the
bureaucracy. `design` shapes the request into a reviewable plan. `code` gives
the agent write access inside an isolated clone of the target project and
requires it to commit the implementation there. `review` gives the committed
diff an independent code-review pass before verification — trivial zero-risk
findings (a typo, comment drift) it fixes and commits in place; anything bigger
blocks the gate and kicks the run back. `test` verifies the behavior and records
what was run. `push` fast-forwards the target project's
default branch, or opens a PR with `--pr`.

`moe sdlc new --from-idea <project>/<slug>` promotes an idea into a run and
seeds the design canvas from the idea body. `moe sdlc reopen <project>/<slug>`
starts a new run seeded with a terminal prior run's design canvas, useful
when a closed or merged topic still has more work behind it.

### Cascades: the bang vocabulary

At the end of a stage, MoE prints a chain prompt. More bangs go further.
Every cascade is headless — the axis is *how far*, not *how*:

- `!` runs exactly the next stage headlessly and then parks at the next gate.
- `!<stage>` runs headlessly up to that named gate, without shipping.
- `!!` runs every remaining stage headlessly and ships **this run** (or
  auto-closes, for workflows without a push gate), then stops.
- `!!!` is the same as `!!`, but after this run ships it **rides the whole
  chain** — cascading into the next live chained run.

The cascade mode flags on `design`/`code`/`review`/`test` mirror the chain
prompt's bang vocabulary at the CLI: `--once` (= `!`) dispatches one stage
headless and parks at the next gate; `--to=<stage>` (= `!<stage>`) walks
headless to a named gate; `--ship` (= `!!`) cascades headless through push
and ships this run; `--chain` (= `!!!`) does the same and then rides the
whole chain. The four cascade flags are mutually exclusive; `--agent` combines
with them by switching the run's persisted agent before the cascade walks the
stages, so every cascaded stage runs on the switched agent.

**Blocked gates.** When a `review` or `test` session closes blocked, the gate
kicks the run back rather than parking. Interactively the chain prompt becomes a
kickback offer `[Y/n/d/x]`: `Y` (default) reopens `code` seeded with the
blocking canvas, `d` kicks back to `design`, `n` parks, and `x` scuttles the
run; after the fix, MoE re-offers the gate that blocked instead of walking
forward. Headless ship cascades (`!!` / `!!!`, and serve's ship) take one
bounded `code` kickback carrying the blocking canvas, then re-dispatch the
blocked stage and re-check its gate once — if the fix doesn't stick, it parks as
before. `!` and `!<stage>` park on a blocked gate without recovery.

### Chains

Chains are the batch version of that same forward motion for active SDLC runs.
`moe chain edit` opens every active SDLC run across projects in `$EDITOR`;
reorder the lines to make a sequence, delete lines you want left unchained,
and save. `moe dash` shows a `chained -> <project>/<run>` hint for active
parents with a live child. When a `!!!` cascade reaches the end of a chained
parent, MoE starts the child at its first pending stage: a fresh child starts
at `design`, while a partly completed child resumes where it is parked. (`!!`
ships the parent and stops — it does not ride into the child.)

When you type an older idea or run slug into an SDLC command, MoE follows
promotion and reopen trailers where it can. In an interactive shell it can ask
whether you meant the current descendant; in non-interactive use it prints a
hint.

## Chat

`moe chat` is the read-only project-review surface: a thinking partner that
reads project source through a per-run sandbox clone, never edits it, and
grooms the idea backlog on your behalf.

```sh
moe chat new [--workspace <name>] [--agent <name>] [--from-idea <project>/<slug>] <project>/<slug>
moe chat chat [--agent <name>] <project>/<run>
moe chat close [--no-edit] <project>/<run>
```

`new` opens the run, `chat` opens or resumes the session, and `close` archives
it. The agent never drives coding or shipping: if the conversation lands on
"this needs building", it captures an idea and you start the SDLC ladder
yourself. The run stays open across sittings, so re-running `chat` continues
the same thread. The canvas is a moe-written session log; the conversation
transcript is the record, read back with `moe chat log`. Grooming the idea
backlog (`moe idea new|edit|close|reopen`) is the one state change a chat
session makes on your behalf.

## PDLC

`moe pdlc` is the product-planning workflow — a robo-PM that plans once and
reconciles forever:

```sh
moe pdlc new [--from-idea <project>/<slug>] [--agent <name>] <project>/<slug>
moe pdlc frame <project>/<run>
moe pdlc prd   <project>/<run>
moe pdlc chunk <project>/<run>
moe pdlc close [--no-edit] <project>/<run>
```

A plan is a run that stays open for the life of a product goal. `frame` shapes
the goal conversationally; `prd` compresses the framing into a durable PRD
under a fixed heading set; `chunk` diffs the PRD against current reality —
prior followups, the journal's harvested-idea lineage, and the project source —
and emits followups for the work that remains. After a chunk sitting, the
chain prompt offers to harvest those followups into ideas (the same editor
gesture `close` uses), so the operator tailors what reaches the backlog. As
harvested ideas run through `sdlc` and land, re-running `chunk` reconciles the
plan against the new reality. Like `chat`, the agent reads project source
through a per-run sandbox clone but never edits it; `close` means the goal
shipped or died, not that a sitting ended.

## Knowledge Base (kb)

`moe kb` is the research companion: research a topic once with an agent, and
keep the distilled answer where future runs read it.

```sh
moe kb new <project>/<slug>
moe kb research <project>/<run>
moe kb summarize <project>/<run>
moe kb lint <project>
```

`research` builds a vetted bibliography in conversation: primary sources,
abstracts in the agent's own words, gaps named instead of papered over.
`summarize` compresses that bibliography into a durable article in the
project wiki, which future runs read as context. The point is to research
once and keep the answer, instead of re-asking an agent the same background
question every few weeks. `moe kb lint <project>` checks wiki hygiene without
opening a run.

## Ideas

`moe idea` is the cheap backlog surface:

```sh
moe idea new <project>/<slug>
moe idea edit <project>/<slug>
moe idea list <project>
moe idea move <project>/<slug> <to-project>
moe idea close <project>/<slug>
moe idea reopen <project>/<slug>
```

Idea capture and editing use `$EDITOR`. Use `moe chat` when you want an agent
to groom the backlog or help shape notes. Every other workflow's `new` accepts
`--from-idea <project>/<slug>`, promoting the idea into a run and preserving
lineage in the journal. `idea reopen` is for a promoted idea whose destination
run was abandoned and should become backlog again.

## Chores

Chores turn recurring project maintenance into runs you open on demand. A chore
definition says what maintenance is due, when it becomes due, and which workflow
run to open for it. MoE evaluates chores against the journal and surfaces the due
ones — but nothing fires on its own. A due chore is a seeded run waiting in
`moe dash` until you choose to open it.

A chore is a directory under `projects/<project>/chores/<name>/` holding a
`chore.json` of scheduler scalars and a `prompt.md` seed:

    projects/my-project/chores/bump-deps/
      chore.json   # {"cadence":"720h","cooldown":"48h"}  -> due monthly, 48h cooldown
      prompt.md                                           -> the seed prompt the opened run starts from

    projects/my-project/chores/regen-docs/
      chore.json   # {"trigger":"go.mod","workflow":"sdlc"} -> due when merged work touches go.mod
      prompt.md                                            -> "Regenerate the dependency table; go.mod changed."

`chore.json` keys are all optional: `trigger` (path glob, or `*` for any merged
project change), `cadence` and `cooldown` (duration strings like `"720h"` or
`"30d"`), and `workflow` (the run to open; defaults to `sdlc`). `prompt.md`
stays a markdown sibling — the opened run reads it verbatim. A chore directory
must contain a parseable `chore.json`.

A chore needs a `trigger`, a `cadence`, or both. `trigger` is a path glob (or
`*` for any merged project change); `cadence` makes it due on a clock. A chore
goes due when its trigger matches new merged work, its cadence elapses, or its
own definition changes — unless it is cooling down or already has an open run.

Two command families, mirroring hooks:

```sh
moe chores new|code|close <project>/<run>     # edit chore definitions (journaled)
moe chore list [--project <p>]                # show what's due
moe chore check [--project <p>] [<project>/<chore>]  # dry-run validation and due-state
moe chore open [--now] <project>/<chore>      # open the seeded run for a due chore
moe chore skip <project>/<chore>              # clear a due chore until it is next triggered
```

`moe chores …` edits definitions under `projects/<project>/chores/*` through a
journaled run. `moe chore open` refuses if the chore isn't due, already has an
open run, or is cooling down. Pass `--now` to open it anyway when it's cooling
down or not yet due — it still refuses if a run is already open.

## Twin

`moe twin reflect <project>` walks the fixed digital-twin documents —
`vision`, `architecture`, `patterns`, `operations`, `glossary` — and folds
new observations into recorded intent. See
[concepts.md §Feedback Channels](concepts.md#feedback-channels) for how the
twin steers future runs.

## Hooks

`moe hooks new`, `moe hooks code`, and `moe hooks close` are the journaled
loop for project hook scripts. `moe hook fire <project> <event>` is the fast
loop: it creates a transient sandbox, runs one event's scripts once, prints
the sandbox path, and exits. The hook events themselves (`dev-env.d`,
`dev-env-teardown.d`, `pre-push.d`) and per-project dev secrets are covered
in [reference.md §Hooks And Environments](reference.md#hooks-and-environments).
