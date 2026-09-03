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
ceremony you cut first. The gates earn their place too — `test` and `review`
exist to kick work back to `code` or `design`, not to decorate the ladder.

`moe sdlc` is the main software-development workflow:

```sh
moe sdlc new [--workspace <name>] [--agent <name>] [--seed] [--park|--ship|--chain] <project>/<slug>
moe sdlc design [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc code   [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc test   [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc review [--agent <name>] [--once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc push [--pr|--merge] <project>/<run>
moe sdlc shell  <project>/<run>
```

A full pass spelled out by hand:

```sh
moe sdlc new my-project/add-batch-support
moe sdlc design my-project/add-batch-support
moe sdlc code my-project/add-batch-support
moe sdlc test my-project/add-batch-support
moe sdlc review my-project/add-batch-support
moe sdlc push my-project/add-batch-support
```

The `new` command opens the run and writes the first files into the
bureaucracy. `design` shapes the request into a reviewable plan. `code` gives
the agent write access inside an isolated clone of the target project and
requires it to commit the implementation there. `test` verifies the behavior
and records what was run. `review` is the last judgment before push: an
independent pass over the diff *and* the test evidence, ending in a ship
verdict — trivial zero-risk findings (a typo, comment drift) it fixes and
commits in place; anything bigger blocks the gate and kicks the run back.
`push` ships the branch by the project's ship route — a PR
by default, a fast-forward merge where `moe project ship <id> merge` says
so — and `--pr` / `--merge` override it for one invocation.

A run whose work landed entirely as bureaucracy commits — project hooks,
chores, knowledge topics, the bureaucracy's own docs — can declare `ship: none`
on its test gate, and `push` then closes the run rather than refusing an empty
branch, so a ride carries on instead of stalling. It honors that only when the
branch verifiably has zero commits ahead of the default branch: a gate and a
branch that disagree refuse loudly, because the alternative is a mis-written
gate deleting a sandbox that still held reviewed work.

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
the first stage — handy for minting a run to pick up later. Opposite it sits
the cascade ladder, the same one the chain prompt spells in bangs: `--ship`
(= `!!`) opens the run and cascades every stage headless through push, then
ships — fire-and-forget; `--chain` (= `!!!`) does that and rides the chain the
run heads. The rungs are a ladder, not modifiers — pick one, and `--park`
excludes both. Every tail composes with either seed: `--seed --park` mints from
a typed seed and walks away, `--from-idea --chain` promotes an idea and rides
it. All ride the shared `new` facade, so
every workflow's `new` that takes `--from-idea` takes these too (a cascade tail
needs the workflow to have a cascade dispatcher, which it refuses to mint a
run without). These tails reach past `new`: `moe chore open` and `moe sdlc
reopen` — the other creators that end at the chain prompt — take `--park` and
the whole `--ship`/`--chain` ladder with the same meaning. (`--chain`'s ride
is usually a no-op on a freshly minted run, which heads no chain yet; it's
offered because the ladder is one vocabulary and the rung's consent is what
matters — same as `new --chain` on a fresh run.)

### Cascades: the bang vocabulary

At the end of a stage, MoE prints a chain prompt. More bangs go further.
Every cascade is headless — the axis is *how far*, not *how*:

- `!` runs exactly the next stage headlessly and then parks at the next gate.
- `!<stage>` runs headlessly up to that named gate, without shipping.
- `!!` runs every remaining stage headlessly and ships **this run** (or
  auto-closes, for workflows without a push gate), then stops.
- `!!!` is the same as `!!`, but after this run ships it **rides the whole
  chain** — cascading into the next live chained run. It is the top rung: what
  is chained when you type it is what runs, because nothing sweeps mid-ride.

The cascade mode flags on `design`/`code`/`test`/`review` mirror the chain
prompt's bang vocabulary at the CLI: `--once` (= `!`) dispatches one stage
headless and parks at the next gate; `--to=<stage>` (= `!<stage>`) walks
headless to a named gate; `--ship` (= `!!`) cascades headless through push
and ships this run; `--chain` (= `!!!`) does the same and then rides the
whole chain. The four
cascade flags are mutually exclusive; `--agent` combines
with them by switching the run's persisted agent before the cascade walks the
stages, so every cascaded stage runs on the switched agent.

**Blocked gates.** When a `test` or `review` session closes blocked, the gate
kicks the run back rather than parking. Interactively the chain prompt becomes a
kickback offer `[Y/n/d/x]`: `Y` (default) reopens `code` seeded with the
blocking canvas, `d` kicks back to `design`, `n` parks, and `x` scuttles the
run; after the fix, MoE re-offers the gate that blocked instead of walking
forward. Headless ship cascades (`!!` / `!!!`, and serve's ship) take
one bounded `code` kickback carrying the blocking canvas, then re-dispatch the
blocked stage and re-check its gate once — if the fix doesn't stick, it parks as
before. `!` and `!<stage>` take no recovery turn of their own: they stop at the
blocked gate and fall through to that same chain prompt (headless, a
back-pointing `kick back to fix` nudge prints instead).

**Nominated closes.** A blocked gate says "this diff isn't ready"; a stage
that concludes the *run* is moot — the change already landed, the premise
doesn't survive the code — says so instead by writing its canvas normally
and ending it with `{"status":"close"}` in the gate. MoE closes the run
through the same `moe sdlc close` every other path uses, in the cascade and
at the interactive chain prompt alike. The reasoning is durable (it is the
committed canvas) and the run stops being re-offered on every sweep;
`moe sdlc reopen` mints a successor seeded from that canvas if the operator
disagrees. A close that fails — lock contention, a dirty tree — warns and
leaves the run open. sdlc only.

### Chains

Chains are the batch version of that same forward motion for active SDLC runs.
Why they exist: Claude Code and Codex run on flat-rate dev subscriptions, so
the capacity you don't use while sleeping or at dinner is already paid for.
Chains turn that idle capacity into throughput. Shape work into designed runs
during the day, `moe chain edit` them into a sequence, fire `!!!` once as you
step away, and the chain codes, tests, reviews, and ships unattended — each run
still gated, journaled, and revertible in the morning.

Every chain roots in operator consent, which is either typed — the bang you
fire — or standing, an armed `moe serve --dynamic` whose
[heartbeat](#the-heartbeat) is the one clock MoE carries and whose retraction is
stopping the process. MoE ships no cron of anyone else's. Typed, the story is
"you pull the trigger at 6pm and the work outlasts your attention", not "MoE
runs at night"; standing, it is "the clock you armed keeps asking", under every
guard a typed ride runs under.

`moe chain edit` opens every active chainable run across projects in `$EDITOR`
— every operator-cascade workflow (SDLC today) plus chain heads — grouped into
blocks that mirror the dash's chains. A blank line is a chain boundary: each
contiguous block of run lines becomes one linear chain (each line chains-to
the one below it in its block; the block's last line chains-to nothing). The
editor is WYSIWYG — the blocks you see are the chains you get.
Move a line into another block to fold it into that chain, and isolate a run in
its own block (or delete its line) to unchain it. Saving unchanged is a no-op.

`moe dash` shows a `chained -> <project>/<run>` hint for active parents with a
live child. When a `!!!` cascade reaches the end of a chained parent, MoE
starts the child at its first pending stage: a fresh child starts at `design`,
while a partly completed child resumes where it is parked. (`!!` ships the
parent and stops — it does not ride into the child.) `moe chain clear` drops
every currently-live chain edge in one commit.

A chain's handle moves every time its head ships, which is awkward when you want
to collect work under a stable name. `moe chain new <project>/<slug>` mints a
**chain run**: a placeholder head that registers no stages, so it is done the
moment it exists and no agent ever opens it. Name it for the topic it collects,
and keep as many live at once as you have topics.

```sh
moe chain new moe/perf-cleanups     # mint a head to collect under
moe chain note moe/perf-cleanups    # why this batch exists
moe chain edit                      # move runs beneath it
moe chain kick moe/perf-cleanups    # ride the batch as it stands
moe chain close moe/perf-cleanups   # drop the head without riding
```

A head's canvas is its **purpose note**: what ties the batch together, in your
words. It's the one thing the chain edges can't tell you, and it's optional —
`moe chain new` returns immediately with an empty note, `--seed` pops `$EDITOR`
at mint, and `moe chain note` writes it whenever. Membership is not written
there. The head's run page in `moe serve` renders the batch live from the
edges, one row per member with the dash's own status vocabulary — the batch a
kick would actually ride, so you can read it before typing one. The kick itself
stays a terminal verb: a hand-staged head is a deliberate staging fence, and
staging is something you do at a keyboard.

`moe chain kick` is a programmatic `!!!` aimed at one named head: it cascades
that run to its ship, then rides on into each chained run in order. A chain-run
head has no stages to walk, so it just closes and the ride carries on. An
ordinary run works too — it ships first, then rides its children — and a run
with no children is a chain of one, so kick is also how you fire a single parked
run headlessly. Kick refuses a run that is itself chained under another; kick
the head instead.

Kick exits non-zero if any run in the chain stalls — the stalled stage's own
exit code, with a `chain ride into <run> exited N` line on stderr naming where.
A cron or script driving kick can treat the exit code as "the whole chain rode
clean", and read stderr only when it didn't. `!!!` behaves the same way.

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
the same thread. When an interactive sitting ends, moe asks whether to close
the run, and the default is no — a chat is meant to be perpetual, so leaving it
open is the safe answer; `y` runs the same operator-driven close as `moe chat
close`, editor and harvest included. Close is a soft archive rather than a
one-way door: running `chat` against a closed run reopens and continues it in
one step, which is why there is no separate reopen verb. The canvas is a
moe-written session log; the conversation transcript is the record, read back
with `moe chat log`. Grooming the idea backlog
(`moe idea new|edit|close|reopen`) is the one state change a chat session makes
on your behalf.

## Knowledge

Background research otherwise decays into scattered chats you re-ask every few
weeks. `projects/<project>/knowledge/` exists to make the answer durable:
research once, write it down, and future runs read it as context instead of
re-earning it.

It's a plain doc tree — `index.md` as the catalog, one file per topic flat under
`topics/` — with no workflow of its own. Two ways in:

- **A research excursion**: an sdlc run whose design stage researches the
  question and writes the topic files, then closes mid-ladder
  (`moe sdlc close`). Review, test, and push are meaningless for a
  bureaucracy-only diff.
- **A ride-along**: any sdlc run that learned something durable about the
  project's domain writes it up as part of the turn. Agents are told to do this
  on their own initiative, not only when asked.

Either way the stage's per-turn commit lands the edits, and the stage refuses to
close if the turn left the tree structurally broken — a topic missing from
`index.md`, a broken relative link, an empty doc. The agent that broke the index
fixes it in the same turn. Hand edits bypass the gate; the tree is yours.

`moe serve` browses the tree read-only, which is where most reading of it
happens.

## Ideas

Capture has to be cheaper than the thought is fleeting, or you lose the thought.
An idea is inert — nothing executes it — so jotting one commits you to nothing;
promotion into a run preserves the lineage in the journal. That is what lets
backlog grooming feed runs without ever becoming automation.

`moe idea` is the cheap backlog surface:

```sh
moe idea new <project>/<slug>
moe idea edit [--chat] <project>/<slug>
moe idea list <project>
moe idea log <project>/<slug>
moe idea move <project>/<slug> <to-project>
moe idea tag <project>/<slug> [workflow] [--design-only]
moe idea untag <project>/<slug>
moe idea close <project>/<slug>
moe idea reopen <project>/<slug>
```

Capture uses `$EDITOR` and launches no agent — jotting stays cheap. Refinement
is the one place an agent may hold the pen: `idea edit --chat` opens an
interactive session on the idea's own document, so the thread persists in
`run.json` and `moe idea log` reads it back. It is deliberately smaller than
`moe chat` — no clone of the project's source, so the agent sharpens your
framing rather than checking claims about the code. Reach for `moe chat` when
you want to think *with* the project; reach for `edit --chat` to sharpen one
note. Every other workflow's `new` accepts
`--from-idea <project>/<slug>`, promoting the idea into a run and preserving
lineage in the journal. `idea reopen` is for a promoted idea whose destination
run was abandoned and should become backlog again.

`idea tag` is the other way in, and the cheaper one: it stamps a workflow tag
(`sdlc` by default) that licenses a pulse to promote the idea itself, under its
own slug, when the survey judges it ready. The tag is the whole fence — **the
machine starts only tagged ideas**, whoever filed them — so `idea untag` is the
per-idea pause, and an untagged idea stays operator-only forever. Agents can
tag at capture time with the followups grammar (`` - [ ] `slug` (sdlc) — Title ``);
`idea tag` is how the operator stamps one that was filed without it. Tagged
ideas say so on the dash, and both verbs have a chip on the idea's page.

`--design-only` narrows that licence to one headless design turn: the promoted
run rides design and then holds until you read the canvas and advance it, which
is the middle rung between "ship it" and "wait until I'm at a terminal". The
plain tag is the ship licence, so re-tagging without the flag clears the
narrowing, and `untag` clears both. The idea page carries a chip for each rung.

## Intents

An **intent** is a short, operator-authored statement of where a project is
going — a theme, a bet, a "we're heading here" — parked on the project while
it's relevant and closed when it stops being so. It is not a task: an intent is
never promoted, never executed, never handed to an agent to advance. Agents
*read* intents; the direction is the operator's.

```sh
moe intent new <project>/<slug>            # park a new intent in $EDITOR
moe intent edit [--chat] <project>/<slug>  # sharpen it — intents are living docs
moe intent list <project>                  # the open intents
moe intent cat <project>/<slug>            # dump one to stdout
moe intent log <project>/<slug>            # read back a --chat session
moe intent close <project>/<slug>          # satisfied or abandoned (status -> closed)
```

Intents are just runs in a single-stage `intent` workflow, the same shape as
ideas, and capture stays cheap the same way — `intent new` launches no agent.
`intent edit --chat` opens the same document-only session `idea edit --chat`
does, with one difference that is all prose: the agent is a scribe there. It
types what the conversation converges on and asks the question that tightens
the bet; it never originates direction. An operator driving a session is still
the operator authoring — what stays banned is an *autonomous* agent writing
where a project is headed. The canvas body is freeform markdown: no fields, no
priority, no until-date. Parked = open.

Where intents reach the robots:

- **Stage prompts.** Every agent-facing stage on a project with open intents
  gets a short catalog section (slug — title, canvas path) between the digital
  twin and the lore catalog, framed as "where this project is going — read the
  ones that bear on what you're deciding." It aims the discretionary calls
  (what to propose, what to prioritise); it doesn't fence what's allowed.
- **The pulse.** The pulse is the one consumer whose job includes the intents,
  so its fragment makes the read mandatory: open intents join the survey floor,
  speculative proposals aim at an open intent (`intent: <slug>` in the Why),
  Pull-next why-now reasons may cite one ("serves `north-star`"), and backlog
  hygiene may flag an intent that looks satisfied or stale — advisory only, the
  operator closes intents.
- **The dash.** Open intents render in a standing `INTENTS` section above the
  backlog, on both the CLI dash and `moe serve`. The heading renders even at
  zero: an empty list is itself a signal that the robots are running unaimed.

No agent mints or edits intents. If a theme looks missing, an agent names it in
a report; the operator decides whether to park it. Deliberately no `move`, no
`reopen`, no `log`, and no run↔intent linkage in v1 — an intent's effect shows
up in what gets filed and ranked, not in an edge table.

## Chores

Recurring maintenance otherwise lives in your memory or in a cron job you don't
trust an agent to run unattended. A chore is standing intent instead: it turns
recurring project maintenance into a run MoE knows how to open. A chore
definition says what maintenance is due, when it becomes due, and which workflow
run to open for it. MoE evaluates chores against the journal and surfaces the
due ones; `moe chore open`, or a pulse's chore auto-open, mints the seeded run.
What happens after that is what you armed: with no `moe serve --dynamic` up, the
run waits in `moe dash` like any other until you start it, while under an armed
serve the [heartbeat](#the-heartbeat) notices the chore coming due, sweeps, and
that sweep both opens the run and rides it on through test, review, and ship.
A chore coming due is a clock event on an otherwise quiet journal, so the
heartbeat gate watches for it directly rather than waiting for something else
to move.

A chore is a directory under `projects/<project>/chores/<name>/` holding a
`chore.json` of scheduler scalars and a `prompt.md` seed:

    projects/my-project/chores/bump-deps/
      chore.json   # {"cadence":"720h","cooldown":"48h"}  -> due monthly, 48h cooldown
      prompt.md                                           -> the seed prompt the opened run starts from

    projects/my-project/chores/regen-docs/
      chore.json   # {"trigger":"go.mod","workflow":"sdlc"} -> due when merged work touches go.mod
      prompt.md                                            -> "Regenerate the dependency table; go.mod changed."

    projects/my-project/chores/readme-update/
      chore.json   # {"when":"a landed change altered user-facing behavior the README describes","cooldown":"7d"}
      prompt.md                                            -> "Bring README.md back in line with what shipped."

`chore.json` keys are all optional: `trigger` (path glob, or `*` for any merged
project change), `cadence` and `cooldown` (duration strings like `"720h"` or
`"30d"`), `when` (a one-line prose due-condition), and `workflow` (the run to
open; defaults to `sdlc`). `prompt.md` stays a markdown sibling — the opened run
reads it verbatim. A chore directory must contain a parseable `chore.json`.

A chore's due-ness comes from one of three families. **Trigger** is a path glob
(or `*` for any merged project change). **Cadence** makes it due on a clock.
Those two are mechanical, compose with each other, and go due when the glob
matches new merged work, the clock elapses, or the chore's own definition
changes. **Judged** (`when`) is the third: some maintenance is due only when a
judgment holds — "a landed change made this artifact lie" — which neither a glob
nor a clock expresses. Write that condition as one line of prose and the pulse
survey evaluates it against what actually landed, nominating the chore when it
holds. A judged chore is never mechanically due; `when` is exclusive with
`trigger` and `cadence`, while `cooldown` composes with all three (on a judged
chore it is the anti-flap on a judgment that over-fires).

Every family shares the same guards: a chore never goes due while it is cooling
down or already has an open run.

Judged chores don't appear in `moe chore list` (it is due-only) or on the dash
until their run opens — `moe chore check` is where you see the registration,
reported as `judged` rather than due/not-due.

The runtime, mirroring hooks:

```sh
moe chore list [--project <p>]                # show what's due
moe chore check [--project <p>] [<project>/<chore>]  # dry-run validation and due-state
moe chore open [--now] [--park|--ship|--chain] <project>/<chore>  # open the seeded run for a due chore
moe chore skip <project>/<chore>              # clear a due chore until it is next triggered
```

`moe chore open` refuses if the chore isn't due, already has an open run, or is
cooling down. Pass `--now` to open it anyway when it's cooling down or not yet
due — it still refuses if a run is already open.

Editing a definition is a plain file edit, or an sdlc run: a stage's per-turn
commit picks up `projects/<project>/chores/`, so `moe sdlc design → code →
close` is the journaled route. Due-ness notices either way — a definition edit
committed by any means makes the chore due as "definition changed."

## Pulse

A pulse is a read-only sweep of one project that feeds the backlog and grooms
queued work into lanes. "Work just landed — what's next?" is a reflex worth
automating, but only inside consent bounds: **the only pulse that fires on its
own is the clock's**, and the only clock is one you started — `moe serve
--dynamic` carries a resident heartbeat (see [the heartbeat](#the-heartbeat)
below). No verb tails a sweep. A ship is quiet whatever bang drove it, a `moe
chain kick` rides the batch you kicked and nothing else, and `moe sync` never
pulses. `moe pulse new <project>` is the manual valve.

Growth is therefore clock-paced rather than recursive. A ride's own commits
move the journal tip, so the next tick's sweep sees the generation it landed
and can start the next one; nothing starts work in the middle of a ride. What
that costs is latency — a finding after a ship waits for the next tick (≤20
minutes) or a hand-run pulse. What it buys is one walker: a sweep's picture of
the board stays the board it walks, so what it says it is starting is what
starts.

Every pulse does three things:

- **Chore auto-open (always).** Every *mechanically* due chore for the project
  gets its run opened — the same seeded run `moe chore open` would mint — and
  nothing more. No stage executes; the opened runs wait in `moe dash` like any
  other. This is automation acting on a chore definition you authored, never a
  fresh decision. Judged chores are not opened here: their condition is the
  survey's to evaluate, below.
- **PR reconcile (always).** The project's `pushed` runs are checked against
  GitHub — the same walk `moe sync` does, scoped to this project — so a PR you
  merged from your phone reads as `merged` before the survey looks at the
  delta. Pointer bumps stay `moe sync`'s job. Offline or without `gh`, this
  warns once and the sweep carries on.
- **The survey (every fire).** A headless, read-only agent sweep — it reads
  the journal since the last pulse, the twin, and the open backlog; files
  followups; and writes a short report ending in a machine-readable `## Gate`.
  It also judges the project's **judged chores**: for each, does what landed
  meet the condition the operator wrote? When it does, the gate nominates the
  chore and the harness opens its ordinary chore run. The gate may also open
  parked runs and order queued work into chained lanes, in one grammar: a run
  with no ordering opinion goes in `loose`, a run whose position the sweep is
  sure of is written inline at that position in `threads`. A `loose` entry may
  set `"design_only": true`, which opens the run, rides it one headless design
  turn and parks it — the sweep's rung between filing a followup and proposing
  a fix. See "Grooming lanes" in the stage guidance. A clean sweep auto-closes
  its own run: the filed followups harvest straight into ideas (review them by
  scrapping on the dash). Every fire runs a fresh sweep unconditionally — a
  lingering open pulse run means a failed or abandoned sweep, sitting visible
  on the dash's ACTIVE list until you inspect and close it, but it never
  blocks the next survey.

```sh
moe pulse new <project>                  # run the whole pulse by hand (chore auto-open + survey)
moe pulse cat <project>/<run> pulse      # read a sweep's report
moe pulse close [--no-edit] <project>/<run>  # close a failed or interrupted sweep by hand
```

A sweep is machine-paced and has no re-open verb: a failed one is read with
`cat` / `log`, ended with `close` (the filings still harvest), and retried by
running another. The survey blocks with a `Ctrl-C to skip` banner; interrupting
it abandons the sweep and leaves the run open until you close it — the next
sweep runs fresh either way. `moe pulse new` is also the verb an external cron
would call — the primitives are cron-safe, but MoE ships no cron of anyone
else's. The one clock it does carry is the armed serve's own heartbeat,
described below.

The survey's first turn carries a GitHub context block the harness gathered:
PRs merged since the last pulse (marking the ones that landed outside moe, which
the journal never saw) and the latest CI verdict per workflow on the default
branch. It's what makes "what landed" honest when work reaches the repo without
going through a run.

### Proposed work and grooming

Most of what a survey finds becomes a followup. When it finds work that is
mechanical, bounded, and verifiable — a red check with a named failing test,
documentation the code plainly contradicts — it may open or nominate an
ordinary `sdlc` or chore run. It can place new and existing queued runs into
ordinary chain threads, leave them loose when it has no ordering opinion, or
add a chain head when a stable name helps tell the thread's story. A head is a
naming convenience, not a container every batch receives.

Between those two there is one more rung. A `loose` spec carrying
`"design_only": true` opens the run and rides it exactly one stage — a
headless design turn — then parks it at design. The `design` body is the
survey's *brief* rather than a baked design, so the bar is lower: a finding
worth a designer's hour, not a fix worth a ride. An idea the operator tagged
design-only (`moe idea tag --design-only`) reaches the same rung from the other
side: the tag carries the bit, and the promoted run gets the identical
one-stage ride with the idea canvas as its brief. The run is then held the way
any unadvanced run is held, and your exits are the run page's advance chip (a
full ride follows on the next sweep), a pushed note (one more design turn), or
close. A spec's `design_only` is skipped on a slug that already names live
work — a run in flight, or an idea whose licence is the operator's to write —
at a thread position, on a chore or twin entry, and on a spec with no `design`
body; on an already-design-only idea the flag agrees with the tag and is
ignored rather than skipped. The bit stays in `run.json` afterwards as
provenance.

In a hand-typed `moe pulse new`, grooming changes recorded placement and not
execution: newly placed work parks for a later kick. Under `--dynamic`,
placement *is* execution, and the candidate set is the whole board — every
structurally kickable parked thread is kicked when the sweep finishes, the ones
this sweep groomed first and then the rest, deduped by root. Kicking is the
default; the survey's way out is a `"park"` line naming why the operator should
look first, and that reason is mandatory. Either way the harness holds a root
that has only a seed, a live session, or a dead machine turn nobody has touched
since (the reap's tombstone, below); a machine-baked, chore-authored, or
past-first-stage root has a settled design and is ready to start. A
design-only spawn is the exception to "machine-baked": its seed is a brief, so
it rides its one design stage and is held from then on like any unadvanced
run. The sweep
stamps that order onto its own canvas as a closing `## Kick` section — each root
queued, parked with the survey's reason, or held by the floor and why — in the
same commit as the close, so a sweep's report says what it was about to run and
not only what it filed. The section records a plan, not a promise: the floor is
re-checked as each root is reached, so a root can still be held past its
"queued" line. Curation sweeps stamp nothing — they start nothing.

Grooming may move a queued run out of one thread and into another — that is how
stray threads consolidate. One unit is off limits: any unit under a **chain
head you minted yourself**. The head is your staging fence, so a batch you are
composing by hand is never reshaped under you. Machine-minted heads and
headless threads stay fully groomable. Want something held, name a head; want
it gone, close it. A ride needs no fence of its own — nothing sweeps while one
is walking, so what you kicked is what runs.

### Prose, both directions

You can push a chunk of prose at any in-progress run and its next agent turn
receives it verbatim:

```
moe input add moe/change-auth-defaults "The failing test is a known flake — skip it and ship."
moe input add moe/change-auth-defaults          # no text: reads stdin
```

That is the stuck-run loop: from the terminal or from the web's `/input` queue
on a phone, write a sentence, and the next turn on that run acts on it. The
note starts nothing — it writes one journal commit, so an armed serve's next
heartbeat offers the project to a pulse and the ordinary kick carries the
thread on; `moe pulse new --dynamic <project>` is the expedite path. A pending
note counts as an operator mark under `safe` mode, so the loop works on a
project that otherwise starts nothing on its own. It is not an advance marker:
it licenses the kick and says nothing about whether a stage was read.

Delivery is once. A pending note reaches exactly one *successful* turn, then
lives on the run page as history — the canvas that turn writes is where durable
direction is supposed to persist. A turn that fails marks nothing, so the next
attempt redelivers.

The other direction: a dynamic survey can ask *you* something, at the run whose
future agent needs the answer.

```json
{"runs": [{"run": "change-auth-defaults",
           "ask": "Which compatibility policy should this use — preserve the old default or adopt the new one?"}]}
```

That opens a durable question on that run. It shows up on the dash as `· ask?`,
in `moe input list`, and on the web's `/input` queue with a reply box;
`moe input answer moe/change-auth-defaults "<prose>"` fills it, and the run's
next turn gets the question and your answer as a pair. One open question per
run; a question on a run the survey is minting in the same gate, on a tagged
idea, or on a chain head is refused with a line.

**Nothing here holds anything.** An unanswered question does not stop the run —
its turns are told the question was asked and no reply came, and they proceed
on their best judgment and note the call on the canvas. Stillness stays the
park's job: a survey that needs the answer first parks the thread and names the
question in the reason. That keeps ask and hold separate, so a survey can also
ask a question the work need not wait for.

There is no dismiss verb. A question you'd rather not answer is discharged by
answering it — "not relevant, proceed" is prose the next turn can read.

### The heartbeat

`moe serve --dynamic` is the standing consent rung, and it licenses exactly one
thing: a resident heartbeat that looks at each project's board every twenty
minutes and runs `moe pulse new --dynamic <project>` when it finds something
worth looking at. That heartbeat is the only automatic pulse there is, and the
only thing in the process that starts an agent at all — the web writes licences
and marks, and the clock spends them. Stopping the process retracts it; an
unarmed `moe serve` is a reader with a capture door.

Per project, `moe project ship <id> pr|merge` picks how a finished run lands
there. `pr` is the default: every unflagged ship — bare `moe sdlc push`, `!!`
and `!!!`, `moe chain kick`, and the heartbeat's rides — pushes the branch and
opens a PR, leaving the run `pushed` with its sandbox until the PR merges (the
pulse's reconcile then flips it to merged or closed). `merge` is the exception
you opt a project into: fast-forward the default branch, delete the remote
branch, drop the sandbox. `--pr` / `--merge` on push, and `m` / `p` at the
chain prompt, override the setting for one ship. The project hub carries the
switch beside mode's.

Per project, `moe project mode <id> paused|safe|auto` caps what that clock may
do. `paused` means the heartbeat never sweeps the project at all. `safe` means
it sweeps and grooms as ever but starts only threads you marked — a stage you
advanced, a chore's standing intent, a workflow tag on the idea a run came
from, a note or answer you left on any run in the thread that no turn has
delivered yet — holding everything else with a named reason. `auto` is the
default and today's behaviour. The mode binds the clock, not you: bangs, stage
verbs, chain kicks and a hand-typed `moe pulse new --dynamic` run in every
mode, because the typed word is the consent. Serve's **advance mark** is that
same consent from the phone: it records the stage you just read as done — one
journal commit, no agent — and `safe` starts on exactly such a mark. The
project hub carries the same three-way switch, and the boards' serve cluster
counts the projects that deviate.

Both surfaces show what the heartbeat is doing. `moe dash` reads the snapshot
serve keeps on disk and carries its status in the banner's tail — armed or not,
how long it has been up, when the next tick lands — spending a line on a project
only when that project is sweeping, cooling off after a failure, or its last
sweep died. The web boards carry the same one-line cluster, linked to a `/serve`
page holding the trace: what each recent tick decided, and the output tail of
any sweep that failed. That trace is the running process's memory rather than
history — a restart starts it over, and the durable record of a sweep is still
the run it opened.

The tick decides nothing. It only asks the question you used to ask by typing a
verb — everything about what a sweep may *do* is unchanged, including the
settled-design floor, the occupancy guard, the review-and-test walk on every
ride, and the journal marks on every machine turn. A tick sweeps when the
project's journal moved since its last heartbeat sweep; when a chore's clock
says so (a mechanically due chore, or a judged chore whose cooldown has expired
without a sweep since — one probe per expiry, not one per tick); or when
startable work is parked with nobody inside it *and no sweep has looked at that
board since it last changed* — a settled thread, or an open idea you tagged,
since the tag is the licence to start it. A thread a survey saw and deliberately parked with a
reason is not re-offered until something moves. That parked leg looks one step
past a held door: settled work queued behind a thread head that is itself
waiting on your design still counts, because a sweep grooms before it kicks and
the groom is what can move that work out from behind the head. A `chain` head
you minted yourself is the exception — it fences its whole batch, since staging
one by hand is the point. The heartbeat stands down while anything is live in
the project — a ride mid-hop, you sitting in a stage, a survey mid-turn — and
for one full tick after anything you did by hand. A quiet board costs nothing —
no agent turn, no run, no journal line.

Failure cools itself off: consecutive failed sweeps back a project's tick off
exponentially, so a night of exhausted plan limits leaves a couple of open
pulse runs on the dash rather than a pile. The first failure's run, sitting on
ACTIVE, is the tell — a sweep that died leaves it open forever, and the
heartbeat deliberately sweeps straight past it rather than letting one dead
vendor night wedge the project until you notice.

The heartbeat also reaps: a session branch whose machine walk died — same host,
pid gone, heartbeat stale — is abandoned so the run re-parks. A robot half-turn
is regenerable. Before the branch goes, the reap stamps a tombstone on the
run — which stage died, when, and the abandoned branch tip — so a run whose turn
was dropped stops reading like one the loop never reached; `moe dash` marks the
row `· died` and the run page names the sha the transcript is still readable at.

The tombstone is a brake as well as a record, and that is what keeps one refusal
from costing a stage turn every sweep: a headless stage that refuses because the
work needs you exits with nothing written, so with no brake the next sweep kicks
the same stage again. While the note stands the kick floor holds the whole
thread and the heartbeat's pre-ask skips it. Any touch of your own on the
thread — a `moe input` note, an advance, a `moe chain edit` or `moe chain clear`
that names one of its runs, anything landing a journal commit the machine
didn't stamp — releases it for one retry, and opening a stage there clears it
outright. If that retry refuses too, its fresh tombstone re-arms the hold: each
thing you do buys one attempt and no more.

Sessions you started, sessions whose claimant might still be alive, and
sessions with no record at all are never touched; they surface on the dash's
ACTIVE row and `moe session resolve` / `moe session abandon` are still yours.

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

The twin has no maintenance verb. An `sdlc` run that establishes something
the twin should record edits the five documents in place, and the edit rides
that stage's own commit — the same route hooks, chores, and knowledge topics
take. The `moe-twin` skill carries the writing contract: fill a genuine gap
freely, tighten wording on evidence, reverse a stated bet only loudly, and
treat compression as a valid edit. A stage that leaves the tree structurally
broken — a dangling cross-reference, an empty doc — doesn't get to close on it.

A twin edit a run can see but shouldn't make goes to `feedback/twin.md`, which
harvests into an idea at termination. See
[concepts.md §Feedback Channels](concepts.md#feedback-channels) for how the
twin steers future runs.

## Hooks

Some project guidance has to be deterministic rather than prose an agent
interprets: bringing up a dev environment, tearing it down, gating a push.
Hooks are that layer — drop-in executable scripts with no manifest, discovered
by directory.

`moe hook fire <project> <event>` is the iteration loop: it creates a transient
sandbox, runs one event's scripts once, prints the sandbox path, and exits — no
run needed to test a 30-line bash change. Landing a change is a plain file edit,
or an sdlc run whose per-turn commit picks up `projects/<project>/hooks/`
(`moe sdlc design → code → close`) when you want the edit journaled with a
canvas.

The hook events themselves (`dev-env.d`, `dev-env-teardown.d`, `pre-push.d`) and
per-project dev secrets are covered in
[reference.md §Hooks And Environments](reference.md#hooks-and-environments).
