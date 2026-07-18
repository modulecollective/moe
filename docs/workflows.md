# Workflows

A workflow is a small ladder of stages; a run is one pass through that
ladder. This page is how to drive each one. For what runs, canvases, and
sandboxes are, see [concepts.md](concepts.md); for the command catalog and
environment reference, see [reference.md](reference.md).

## SDLC

Designed, reviewed, tested changes used to cost enough that the discipline got
skipped under deadline. The bet here is that agents changed the price: when
each stage is one conversation and the handoff is a canvas the next stage
reads, the full lifecycle becomes the cheap default path rather than the
ceremony you cut first. The gates earn their place too — `review` and `test`
exist to kick work back to `code` or `design`, not to decorate the ladder.

`moe sdlc` is the main software-development workflow:

```sh
moe sdlc new [--workspace <name>] [--agent <name>] [--seed] [--park|--ship] <project>/<slug>
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
when a closed or merged topic still has more work behind it; reopen inherits
the prior run's agent and workspace, with `--workspace`/`--no-workspace` and
`--agent`/`--no-agent` to override or clear either.

A few tail flags shape how `new` opens the run. `--seed` pops `$EDITOR` on a
stub and opens the run with your edited body as the first-stage seed (mutually
exclusive with `--from-idea`, which already claims that seed). `--park` opens
the run and stops, printing the next-stage hint instead of prompting to run
the first stage — handy for minting a run to pick up later. `--ship` is the
opposite tail (= `!!` at the chain prompt): it opens the run and cascades every
stage headless through push, then ships — fire-and-forget. `--park` and
`--ship` are mutually exclusive. Both compose with either seed: `--seed --park`
mints from a typed seed and walks away, `--from-idea --ship` promotes an idea
and rides it to the ship. All ride the shared `new` facade, so every workflow's
`new` that takes `--from-idea` takes these too (`--ship` needs the workflow to
have a cascade dispatcher, which it refuses to mint a run without). `--park`
also reaches past `new`: `moe chore open`, `moe twin reflect`, and
`moe sdlc reopen` — the other creators that end at the chain prompt — take it
with the same meaning.

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
before. `!` and `!<stage>` take no recovery turn of their own: they stop at the
blocked gate and fall through to that same chain prompt (headless, a
back-pointing `kick back to fix` nudge prints instead).

### Chains

Chains are the batch version of that same forward motion for active SDLC runs.
Why they exist: Claude Code and Codex run on flat-rate dev subscriptions, so
the capacity you don't use while sleeping or at dinner is already paid for.
Chains turn that idle capacity into throughput. Shape work into designed runs
during the day, `moe chain edit` them into a sequence, fire `!!!` once as you
step away, and the queue codes, reviews, tests, and ships unattended — each run
still gated, journaled, and revertible in the morning.

This is deliberately not scheduling. Every chain is rooted in one operator
trigger; MoE ships no cron and nothing starts on a clock. The story is "you
pull the trigger at 6pm and the work outlasts your attention", not "MoE runs at
night".

`moe chain edit` opens every active SDLC run across projects in `$EDITOR`,
grouped into blocks that mirror the dash's chains. A blank line is a chain
boundary: each contiguous block of run lines becomes one linear chain (each
line chains-to the one below it in its block; the block's last line chains-to
nothing). The editor is WYSIWYG — the blocks you see are the chains you get.
Move a line into another block to fold it into that chain, and isolate a run in
its own block (or delete its line) to unchain it. Saving unchanged is a no-op.

`moe dash` shows a `chained -> <project>/<run>` hint for active parents with a
live child. When a `!!!` cascade reaches the end of a chained parent, MoE
starts the child at its first pending stage: a fresh child starts at `design`,
while a partly completed child resumes where it is parked. (`!!` ships the
parent and stops — it does not ride into the child.) `moe chain clear` drops
every currently-live chain edge in one commit.

When you type an older idea or run slug into an SDLC command, MoE follows
promotion and reopen trailers where it can. In an interactive shell it can ask
whether you meant the current descendant; in non-interactive use it prints a
hint.

## Chat

Half the value of an agent is thinking, not editing. Chat exists for the half
that shouldn't touch code: a durable, resumable thinking thread that is
read-only against the project, so exploration can't drift into unreviewed
edits. Its single write surface is grooming the idea backlog. And the run
persists across sittings — one long conversation the next `chat` continues, not
a fresh amnesiac session each time.

`moe chat` is the read-only project-review surface: a thinking partner that
reads project source through a per-run sandbox clone, never edits it, and
grooms the idea backlog on your behalf.

```sh
moe chat new [--agent <name>] [--from-idea <project>/<slug>] <project>/<slug>
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

## Knowledge Base (kb)

Background research otherwise decays into scattered chats you re-ask every few
weeks. kb exists to make the answer durable: research once (a vetted
bibliography with the gaps named), distill once (a wiki article), and future
runs read the answer as context instead of re-earning it.

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

Capture has to be cheaper than the thought is fleeting, or you lose the thought.
An idea is inert — nothing executes it — so jotting one commits you to nothing;
promotion into a run preserves the lineage in the journal. That is what lets
backlog grooming feed runs without ever becoming automation.

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

Recurring maintenance otherwise lives in your memory or in a cron job you don't
trust an agent to run unattended. A chore is standing intent instead: it turns
recurring project maintenance into runs you open on demand. A chore definition
says what maintenance is due, when it becomes due, and which workflow run to
open for it. MoE evaluates chores against the journal and surfaces the due ones
— but nothing fires on its own. A due chore is a seeded run waiting in
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
moe chore open [--now] [--park] <project>/<chore>  # open the seeded run for a due chore
moe chore skip <project>/<chore>              # clear a due chore until it is next triggered
```

`moe chores …` edits definitions under `projects/<project>/chores/*` through a
journaled run. `moe chore open` refuses if the chore isn't due, already has an
open run, or is cooling down. Pass `--now` to open it anyway when it's cooling
down or not yet due — it still refuses if a run is already open.

## Pulse

A pulse is a read-only sweep of one project that feeds the backlog and ranks
what to pull from it. "Work just landed — what's next?" is a reflex worth
automating, but only inside consent bounds: a pulse fires at the tail of the
operator-rooted run-traffic verbs (`moe sdlc close`, `moe sdlc push`,
`moe twin close`, and the cascades' auto-close), never on its own clock and
never from `moe sync`. Every fire rides an action you took. Scope is always the
driven run's project.

Every pulse does two things:

- **Chore auto-open (always).** Every due chore for the project gets its run
  opened — the same seeded run `moe chore open` would mint — and nothing more.
  No stage executes; the opened runs wait in `moe dash` like any other. This is
  the one sanctioned auto-mint: automation acts on a chore definition you
  authored, but never makes a fresh decision.
- **The survey (every fire).** A headless, read-only agent sweep — it reads
  the journal since the last pulse, the twin, and the open backlog; files
  followups; and writes a short report whose last section, `## Pull next`,
  ranks the top few open ideas to pull next with a one-line why. `moe dash`
  floats those picks to the top of BACKLOG, each carrying its reason. A clean
  sweep auto-closes its own run: the filed followups harvest straight into
  ideas (review them by scrapping on the dash). Every fire runs a fresh sweep
  unconditionally — a lingering open pulse run means a failed or abandoned
  sweep, sitting visible on the dash's ACTIVE list until you inspect and
  close it, but it never blocks the next survey.

```sh
moe pulse new <project>                  # run the whole pulse by hand (chore auto-open + survey)
moe pulse pulse <project>/<run>           # reopen a sweep to inspect or re-run it
moe pulse close [--no-edit] <project>/<run>  # close a failed or interrupted sweep by hand
```

The survey blocks with a `Ctrl-C to skip` banner; interrupting it abandons the
sweep and leaves the run open for a manual sitting or close. `moe pulse new` is
also the verb an external cron would call — the primitives are cron-safe, but
MoE ships no scheduler of its own. Nothing auto-executes: filed followups
promote to ideas at the auto-close, but an idea is an inert backlog entry —
the Pull next list is advisory and you hold every execution trigger.

## Twin

Code records what was built; it never records what was intended. So agents
re-derive intent from the code and get it subtly wrong, run after run — the
same misread of a boundary or a non-goal, rediscovered every time. The digital
twin exists to write that intent down once. It is recorded intent in five fixed
documents — `vision`, `architecture`, `patterns`, `operations`, `glossary` —
that every stage reads before substantive work.

What makes it steering rather than documentation is the precedence rule: when
the code and the twin disagree, the twin wins until a deliberate edit updates
it. A run that would contradict a recorded decision has to name the conflict
first, not quietly diverge. Intent leads; implementation follows.

`moe twin reflect <project>` is the maintenance verb. It walks each document
against the journal events since the last pass and folds observed drift in
under a rising edit bar: fill a genuine gap freely, tighten wording only on
repeated sightings, and reverse a stated bet only loudly and on strong
evidence. A closing `finalize` stage then seals the pass — clearing hygiene
findings and rolling events into a history summary, older horizons compressed.
Runs don't edit the twin directly; they leave observations as feedback notes,
and reflect is where those get adjudicated. See
[concepts.md §Feedback Channels](concepts.md#feedback-channels) for how the
twin steers future runs.

## Hooks

Some project guidance has to be deterministic rather than prose an agent
interprets: bringing up a dev environment, tearing it down, gating a push.
Hooks are that layer — drop-in executable scripts with no manifest, discovered
by directory. Two loops maintain them: a journaled edit loop
(`hooks new/code/close`) for durable changes, and a fast one-shot fire loop for
iterating on a script until it works.

`moe hooks new`, `moe hooks code`, and `moe hooks close` are the journaled
loop for project hook scripts. `moe hook fire <project> <event>` is the fast
loop: it creates a transient sandbox, runs one event's scripts once, prints
the sandbox path, and exits. The hook events themselves (`dev-env.d`,
`dev-env-teardown.d`, `pre-push.d`) and per-project dev secrets are covered
in [reference.md §Hooks And Environments](reference.md#hooks-and-environments).
