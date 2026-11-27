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
ceremony you cut first. The gates earn their place too — build's evidence gate
keeps unverified work from reaching review, and `review` exists to kick work
back to `build` or `design`, not to decorate the ladder.

`moe sdlc` is the main software-development workflow:

```sh
moe sdlc new [--workspace <project>/<name>] [--agent <name>] [--seed] [--park|--ship] <project>/<slug>
moe sdlc design [--workspace <project>/<name>] [--agent <name>] [--once | --to=<stage> | --ship] <project>/<run>
moe sdlc build  [--workspace <project>/<name>] [--agent <name>] [--once | --to=<stage> | --ship] <project>/<run>
moe sdlc review [--workspace <project>/<name>] [--agent <name>] [--once | --to=<stage> | --ship] <project>/<run>
moe sdlc push [--pr|--merge] <project>/<run>
moe sdlc shell  <project>/<run>
```

A full pass spelled out by hand:

```sh
moe sdlc new my-project/add-batch-support
moe sdlc design my-project/add-batch-support
moe sdlc build my-project/add-batch-support
moe sdlc review my-project/add-batch-support
moe sdlc push my-project/add-batch-support
```

The `new` command opens the run and writes the first files into the
bureaucracy. `design` shapes the request into a reviewable plan with a
single-file template and an explicit gate. `build` gives the agent write
access inside an isolated clone of the target project, requires it to commit
the implementation there, and has it exercise and repair the change in the
same session. Build keeps two canvases, `content/code.md` (behavior,
decisions, PR draft, test plan) and `content/test.md` (what was and wasn't
verified), each with its own gate; the run moves on only when both are stamped
ready, and every build entry resets both to blocked. `review` is the last
judgment before push: an independent pass over the diff *and* build's
evidence, ending in a ship verdict — trivial zero-risk findings (a typo,
comment drift) it fixes and commits in place; anything bigger blocks the gate
and kicks the run back to build. `push` ships the branch by the project's ship
route. New registrations use a fast-forward merge; existing projects keep
their settings, with an absent ship field still meaning PR. `moe project ship
<id> pr` selects PRs, and `--pr` / `--merge` override the route for one
invocation.

`moe sdlc code` and `moe sdlc test` are retired: they print the build command
and start nothing. A run started before build existed keeps its code and test
canvases readable (`moe sdlc cat`, `moe sdlc log`, the web run page); its first
build turn seeds `content/code.md` and `content/test.md` from them and must
restamp both before the run can reach review.

Closing a run and marking a stage advanced both refuse while any session branch
remains for that run, even after its claimant dies. This applies to terminal
commands, automatic closes and stale web requests; the web returns a conflict
without changing the run. Finish or recover the session before retrying. See
[Cleanup And Recovery](reference.md#cleanup-and-recovery) for interrupted turns
and the conditions for resuming one.

A session branch lasts only as long as its stage turn. The invocation that ran
the turn keeps its own hold on the run from admission through the next-stage
prompt and whichever choice you make there. While it is held, a dynamic pulse
or heartbeat skips the run, and close, advance, push and sync from anywhere
else refuse or wait: the web returns a conflict, and sync leaves the run
`pushed` for its next pass. Declining, Ctrl-D, Ctrl-C or exiting releases the
hold, so ordinary automatic pickup applies again. A crashed invocation releases
it at once, though any session branch it left still blocks as above. A prompt
whose run finished elsewhere in the meantime reports the run's current status
and launches nothing.

A run whose work landed entirely as bureaucracy commits — project hooks,
chores, knowledge topics, the bureaucracy's own docs — can declare `ship: none`
on build's test.md gate, and `push` then closes the run rather than refusing an
empty branch, so a ride carries on instead of stalling. It honors that only
when the branch verifiably has zero commits ahead of the default branch: a gate
and a branch that disagree refuse loudly, because the alternative is a
mis-written gate deleting a sandbox that still held reviewed work.

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
`--ship` (= `!!`), which opens the run and cascades every stage headless through
push, then ships — fire-and-forget. `--park` excludes it. Either tail composes
with either seed: `--seed --park` mints from a typed seed and walks away,
`--from-idea --ship` promotes an idea and ships it. All ride the shared `new`
facade, so every workflow's `new` that takes `--from-idea` takes these too (a
cascade tail needs the workflow to have a cascade dispatcher, which it refuses
to mint a run without). These tails reach past `new`: `moe chore open` and `moe
sdlc reopen` — the other creators that end at the chain prompt — take `--park`
and `--ship` with the same meaning.

### Cascades: the bang vocabulary

At the end of a stage, MoE prints a chain prompt. More bangs go further.
Every cascade is headless — the axis is *how far*, not *how*:

- `!` runs exactly the next stage headlessly and then parks at the next gate.
- `!<stage>` runs headlessly up to that named gate, without shipping.
- `!!` runs every remaining stage headlessly and ships **this run** (or
  auto-closes, for workflows without a push gate), then stops.

The cascade mode flags on `design`/`build`/`review` mirror the chain
prompt's bang vocabulary at the CLI: `--once` (= `!`) dispatches one stage
headless and parks at the next gate; `--to=<stage>` (= `!<stage>`) walks
headless to a named gate; `--ship` (= `!!`) cascades headless through push
and ships this run. The three
cascade flags are mutually exclusive; `--agent` combines
with them by switching the run's persisted agent before the cascade walks the
stages, so every cascaded stage runs on the switched agent.

**Leaving a prompt.** At every chain prompt, Ctrl-C and Ctrl-D both decline,
whatever the default is — a `[Y/n]` gate does not run the next stage, kick
back, or close the run just because you walked away from it. Both keys decline
at `moe init`'s "commit now?" prompt too, leaving the scaffold staged. Text
typed before the Ctrl-D still counts as the answer.

**Reading the work.** Each chain prompt prints the canvas it is asking about
above its menu. Review source changes with ordinary tools — `git diff`, an
editor, a pager.

**Blocked gates.** When a `review` session closes blocked, the gate kicks the
run back rather than parking. Interactively the chain prompt becomes a kickback
offer `[Y/n/d/x]`: `Y` (default) reopens `build` seeded with the blocking
canvas, `d` kicks back to `design`, `n` parks, and `x` scuttles the run; after
the fix, MoE re-offers the gate that blocked instead of walking forward.
Headless ship cascades (`!!`, and serve's ship) kick back to `build` headlessly
with the blocking canvas, re-dispatch review, and repeat until review passes,
the reviewer writes `{"status":"blocked","escalate":true}` (another build round
won't help), or the run's journal holds ten review turns; then it parks and asks
you a question on the run, which shows wherever open questions do:
`moe input list`, serve's input page, the dash and Rex. A sweep won't re-ride a
run parked that way until you answer it (or push any note), kick it back by hand
(uncapped), or close it; an answer buys one more review turn with your reply in
its prompt. `!` and `!<stage>` take no recovery turn of their own: they stop at
the blocked gate and fall through to that same chain prompt (headless, a
back-pointing `kick back to fix` nudge prints instead).

Design starts with Problem, Decision, Scope, Verification and Open questions
headings, plus a fenced JSON gate initially `blocked`. The headings guide
authorship; readiness requires an explicit `ready` judgment. Every admitted
design entry resets the first gate to `blocked` while preserving existing prose,
including promoted, baked and reopened seeds. Seeding/reset alone never counts
as a work turn. A blocked, missing or malformed gate parks automatic progression
at design, including recovery overrides; resume with `moe sdlc design`. Old
gate-less unfinished designs need a design pass too. A ready verdict still needs
the ordinary work-turn and advance/successor evidence; explicit stage verbs
retain their gate bypass and prior-canvas checks.

A build that ends with either canvas unstamped parks at build on every exit —
the chain prompt, a cascade, a review kickback, a push-hook or rebase recovery,
and a CI repair alike — and names each unstamped file and why. It never kicks
back to itself; resume it with `moe sdlc build`.

**Nominated closes.** A blocked gate says "this diff isn't ready"; a stage
that concludes the *run* is moot — the change already landed, the premise
doesn't survive the code — says so instead by writing its canvas normally
and editing the existing first gate to `close` (add it below the title only
when absent). For build, either content file can nominate close; the prompt
asks for code.md. The complete format is:

````markdown
## Gate

```json
{"status":"close"}
```
````

Bare JSON is unready. A valid close works even with unfinished template
sections. MoE closes without another question through the same `moe sdlc close`
every other path uses, in the cascade and at the interactive chain prompt
alike. The reasoning is durable (it is the committed canvas) and the run stops
being re-offered on every sweep; `moe sdlc reopen` mints a successor seeded from
that canvas if the operator disagrees. A close that fails — lock contention, a
dirty tree — warns and leaves the run open. sdlc only.

### Seeing and steering a running run

Every invocation that runs workflow work — a stage, the next-stage prompt, a
kick, a CI repair, a push — serves a small live endpoint for its run while it
owns it. `moe dash` appends what that owner reports to the run's row, in
brackets: `[agent running build 4m]`, `[waiting for operator]`,
`[pause pending]`, `[handed off]`, `[owned · no live status]`.
`moe control show <project>/<run>` prints the long form: owner and generation,
phase and since when, why it is waiting, its continuation (`prompt`, `!`, `!!`),
where it came from (a terminal, serve's local pty, Rex; the parent sweep), its
Rex block, any hold, the request log, and the exact command that returns to the
last stage that ran. A reply proves the owner is alive, not that the agent is
making progress, and an owner that does not answer is shown as unreachable —
never as idle, and never as leave to start a replacement.

The shared board shows `blocked` beside a stage after an authored turn leaves
its gate unready. Fresh templates and ready stages parked before their
successor stay quiet. The badge describes automatic advancement, not who must
act; explicit stage commands and controls keep their usual behavior. A
confirmed live retry hides the previous badge until the owner returns to its
next-stage decision or exits. An unreachable owner preserves the last authored
result. UI/web run details and `moe control show` name the stage, gate problems
and the canvas command for the explanation.

Four controls steer a stageful run (SDLC today), from the terminal or the
run's web page:

```
moe control pause   moe/change-auth   # hold it once its current work settles
moe control handoff moe/change-auth   # the same hold, for you to pick up
moe control park    moe/change-auth   # have its current owner yield, once
moe control finish  moe/change-auth   # ship it (!!) from its next boundary
moe control resume  moe/change-auth   # release a hold; starts nothing
```

`moe ui` puts all of this on one screen: the board with a selection, the
selected run's `control show` facts below it, updated as the owner publishes,
and the same controls as `:pause`, `:finish` and so on, alongside notes, answers
and the run's Rex terminal (`t`). A third pane keeps recent command outcomes and
recovery messages. Tab and Shift-Tab cycle the three panes; the pane receiving
input has a bright amber heading with a heavy rule (heavy even without color).
The selection's `>` still marks the target when another pane has input. End in
the console follows new messages after scrolling. The shared dash banner stays
at the top, clock on the right; the activity histogram stays below it, one day
per column across the terminal, while board lines scroll. When space is short,
the whole overview collapses and returns on resize. The board heading's `lines …
of …` counts scrollable display lines, including section headings and blanks,
rather than runs. Pane separators have a blank row on each side, and the console
keeps six message rows even on short terminals. The minimum size is 40 columns
by 20 rows. Moving to the first board item reveals the headings above it.

The Console heading quietly labels an active temporary default when space
allows. `:agent codex` or `:agent claude` chooses that backend for 24 hours;
`:agent reset` returns to the current stylesheet, and `:agent` shows full
status and local expiry at every width. These work without a selected row,
serve or Rex. Flags and run bindings still win. Status refreshes with the
normal three-second discovery pass. The web dashboard offers the same controls
and status inside its hamburger menu, including when serve is unarmed.

The UI board carries no service rows. Service exceptions are logged to the
Console instead; for routine sweep status, use `moe dash` or the web `/serve`
page. See [Re-Entry And Supervision](reference.md#re-entry-and-supervision).

Press `/` from any pane to find a board item by a case-insensitive literal
substring of `project/run`. Surrounding query spaces are ignored. Search
includes off-screen rows, captures, completed items, chores and kept drafts
within the current filters; it does not search titles or prose. The board
keeps its order and groups. `*` marks matches; `>` marks the selected match.
Up/Down move among matching occurrences, stopping at either end. Enter accepts
the selection and returns to board navigation without starting an action.
Esc or Ctrl-C cancels and restores the previous selection, pane and scroll
positions against the latest board. Empty queries include all items without
match marks. `No matches` clears the preview and keeps search open until you
edit or cancel. The footer shows the query, cursor, position/count and hints.
Use the single-line editor's movement, deletion and word keys; paste is
literal, with newlines becoming spaces. Printable shortcuts remain query
text; Tab, page and save/discard keys do nothing in search. Ctrl-L redraws
and Ctrl-Z suspends. In editors and the command line, `/` remains text.

Use `:new-idea <project>/<slug>` or `:new-intent <project>/<slug>` to open a
capture draft. Slugs use lowercase letters, digits and hyphens, e.g.
`:new-idea p/my-idea`; uppercase is refused before a draft opens. In
multiline editors, Enter or Shift+Enter inserts a newline. Nothing is
created until Ctrl-S. Ideas offer save only, save and tag sdlc, or save and
tag design only. The draft names its target even on an empty or filtered
board. Alt-Backspace or Ctrl-W deletes a word; Alt-arrows or Alt-b/f moves
by words. Words are whitespace-delimited, including newlines, so
project/run tokens stay together. Esc keeps a draft, Ctrl-X discards it.

On the selected open idea, press `g` to set the SDLC licence, `d` to set a
design-only licence, or `u` to clear either licence. These are explicit
settings: repeating one creates no extra commit. They keep the selection,
work from any pane in normal navigation, and launch nothing; a later sweep
may pick up a tagged idea. Use `j`/`k` or arrows to continue to the next row.
The command equivalents are `:tag-sdlc`, `:tag-design`, and `:untag`.
In editors and the command line, the letters remain text.

In the command prompt, Tab completes a command name and cycles matches in
lexical order; Shift-Tab cycles backward. Type a space before arguments and
press Enter to submit. `new-i` matches `new-idea` and `new-intent`; `tag-`
matches `tag-design` and `tag-sdlc`. Project IDs, slugs and other values are
typed in full. Completion replaces the word at the cursor and preserves
surrounding text. For example, `:new-i<Tab> p/example` becomes
`:new-idea p/example`.

The board still gathers every three seconds and follows owner status
subscriptions. While a run has an open session, a separate 500 ms timer twinkles
the star on today's histogram bar from cached frames, without extra observation
work; an idle board does not repaint.

None of them interrupts an agent. The owner finishes the turn it is on — commit,
transcript, input stamps, any recovery that turn needs — and acts at its next
boundary: before the next stage, a gate kickback, a ship attempt, a nominated
close, or while it waits for quota or at the next-stage prompt (both wake on a
control). A yield exits 77, which a sweep reads as the operator's decision: no
failure backoff. A pulse ride that parks at a stage's gate is clean the same
way: the stage's turn ran and committed, and the run waits on what its canvas
says. So is a CI repair whose turns ran cleanly and found nothing to ship: no
repair commit, or a nominated close.

**Pause and handoff hold the run** from the moment they are accepted until
an explicit resume — across sweeps, restarts and disconnects, and against
CI repair on a pushed run. Handoff prints the command for the last stage that
ran, e.g. `moe sdlc build moe/change-auth --agent codex`. Two ways out:
`moe control resume` (or the web's resume) releases the hold for a later
eligible sweep and starts nothing; typing that stage command at a terminal
opens the stage now, and clears the hold once it is admitted. Claude can
resume its conversation; Codex starts fresh from the canvas and current
guidance. See [Cleanup And Recovery](reference.md#cleanup-and-recovery) for
backend session behavior. Headless, machine and nested entries never clear a
hold, and a resume that loses to another owner or an unresolved session leaves
it in place.

**Park** asks only the current owner to yield, holds nothing, and is retired
if that owner is gone before it reaches a boundary. It is refused when no
owner answers, since there is nothing to address. **Finish** is `!!` for this
run alone: through the same gates and ship route. It applies at the next-stage
prompt, at the next ride (widening a `!`), from the stage after the one a
running `!` ride is on, or on an open CI repair's existing PR; it does not start
an idle run. A walk through the run that fails — a ride, a typed cascade, a
stage entered on its own, or a CI repair — or a ride that meets a blocked gate
or stops short of shipping is the finish's outcome: it is recorded as failed, not kept for a
later ride. If that receipt cannot be committed, it is kept under
`.moe/control-unrecorded/` and the finish grants nothing; the next owner
records it before starting work, and yields while it still cannot. Until then
the dash, `control show`, the run page and a repeated request report that
known outcome — `[finish failed · receipt not recorded]` — with the error that
refused the commit, never as pending. A hold wins over finish, and an applied
finish whose owner died is reconciled — shipped, or "recovery required" until
a resume, never replayed.

`moe input add` starts no agent and does not clear a pause or handoff.

## Chat

Half the value of an agent is thinking, not editing. Chat exists for the half
that shouldn't touch code: a durable, resumable thinking thread that is
read-only against the project, so exploration can't drift into unreviewed
edits. Its single write surface is grooming the idea backlog. And the run
persists across sittings — one long conversation the next `chat` continues, not
a fresh amnesiac session each time. Codex is the exception: each sitting there
starts a new conversation.

`moe chat` is the read-only project-review surface: a thinking partner that
reads project source through a per-run sandbox clone, never edits it, and
grooms the idea backlog on your behalf.

```sh
moe chat new [--agent <name>] [--from-idea <project>/<slug>] <project>/<slug>
moe chat chat [--agent <name>] <project>/<run>
moe chat close <project>/<run>
```

`new` opens the run, `chat` opens or resumes the session, and `close` archives
it. The agent never drives coding or shipping: if the conversation lands on
"this needs building", it captures an idea and you start the SDLC ladder
yourself. The run stays open across sittings, so re-running `chat` continues
the same thread. When an interactive sitting ends, moe asks whether to close
the run, and the default is yes — most sittings are finished when you leave
them, and close is cheap because re-entry reopens; Enter runs the same
operator-driven close as `moe chat close`, while `n` leaves the run open for a
later sitting and Ctrl-C or Ctrl-D does the same. Every successful sitting
harvests its filings; close harvests again as a backstop, without an editor.
Close is a soft archive rather than a one-way door: running `chat` against a
closed run reopens and continues it in one step, which is why there is no
separate reopen verb. The canvas is a moe-written session log; the conversation
transcript is the record, read back with `moe chat log`. Grooming the idea
backlog (`moe idea new|edit|close|reopen`) is the one state change a chat
session makes on your behalf.

## Knowledge

Background research otherwise decays into scattered chats you re-ask every few
weeks. `projects/<project>/knowledge/` exists to make the answer durable:
research once, write it down, and future runs read it as context instead of
re-earning it.

It's a plain doc tree — `index.md` as the catalog, one file per topic flat under
`topics/` — with no workflow of its own. Two ways in:

- **A research excursion**: an sdlc run whose design stage researches the
  question and writes the topic files, then closes mid-ladder
  (`moe sdlc close`). Build, review, and push are unnecessary for this
  excursion.
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
`moe idea new` captures an untagged note without launching an agent. Untagged
ideas await operator triage; a workflow tag has the next dynamic sweep promote
the idea and start the resulting work, preserving lineage in the journal.
Tagging itself launches no agent; without dynamic consent a tagged idea waits.

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
note. `moe sdlc new` and `moe chat new` both accept
`--from-idea <project>/<slug>`, promoting the idea into a run and preserving
lineage in the journal. `idea reopen` is for a promoted idea whose destination
run was abandoned and should become backlog again.

`idea tag` is the other way in, and the cheaper one: it stamps a workflow tag
(`sdlc` by default), which lets a dynamic sweep pick the idea as its `next`; the
harness then promotes it under its own slug and rides it. The survey decides
when a tagged idea's turn comes, one pick per sweep, but not whether it may
start; if the work turns out not to be ready, the destination's design gate
holds or closes it. The tag is the whole fence — **the machine starts only
tagged ideas**, whoever filed them — so `idea untag` is the per-idea pause, and
an untagged idea stays operator-only forever. Agents can tag at capture time
with the followups grammar (`` - [ ] `slug` (sdlc) — Title ``); `idea tag` is
how the operator stamps one that was filed without it. Tagged ideas say so on
the dash, and both verbs have a chip on the idea's page.

`--design-only` narrows that licence to one headless design turn: the promoted
run rides design and then holds until you read the canvas and advance it, which
is the middle rung between "ship it" and "wait until I'm at a terminal". The
plain tag is the ship licence, so re-tagging without the flag clears the
narrowing, and `untag` clears both. The idea page carries a chip for each rung.

## Intents

An **intent** is a goal: a short statement of where a project is going — a
theme, a bet, a "we're heading here" — or of an outcome larger than one run,
parked while it's relevant and closed when it's done. It is not a task: an
intent is never promoted, never executed, never handed to an agent to advance.
The operator writes the direction; the pulse keeps where each one stands.

```sh
moe intent new <project>/<slug>            # park a new intent in $EDITOR
moe intent edit [--chat] <project>/<slug>  # sharpen it — intents are living docs
moe intent list <project>                  # the open intents
moe intent cat <project>/<slug>            # dump one to stdout
moe intent log <project>/<slug>            # read back a --chat session
moe intent runs <project>/<slug>           # the work linked to it, by state
moe intent close <project>/<slug>          # satisfied or abandoned (status -> closed)
moe intent reopen <project>/<slug>         # undo a close; links stay
```

Intents are just runs in a single-stage `intent` workflow, the same shape as
ideas, and capture stays cheap the same way — `intent new` launches no agent.
The web dashboard's `new intent` action reaches the same capture seam from a
phone or browser: it writes the intent and returns without starting an agent.
`intent edit --chat` opens the same document-only session `idea edit --chat`
does, with one difference that is all prose: the agent is a scribe there. It
types what the conversation converges on and asks the question that tightens
the bet; it never originates direction. The canvas body is freeform markdown:
no fields, no priority, no until-date. Parked = open.

Where intents reach the robots:

- **Stage prompts.** Every agent-facing stage on a project with open intents
  gets their bodies inlined between the digital twin and the lore catalog, each
  under a `slug — title` heading, framed as where the project is going. They
  aim the discretionary calls (what to propose, what to prioritise); they don't
  fence what's allowed. Intents are short by design; one too long to inline is
  a reason to shorten it. The pulse, in the `_cabinet` root project, gets every
  project's open intents, each project under its own heading.
- **The pulse.** The pulse keeps three things about intents. It rewrites each
  intent's `status.md` (plan, landed, blocked, needs you; a few lines, dated
  when it last changed), a companion file beside `run.json` that every reader
  shows under the canvas. The canvas stays the operator's; `status.md` is the
  only other run document the pulse's turn commits, and any other edit is
  dropped. It links runs and ideas to the intent each serves through its gate's
  `serves` key, which writes `intent` on `run.json`; the kickoff lists the
  week's unlinked work that moved since the last pulse, so mapping is cheap and
  nothing is backfilled. A run with an open session is skipped (the link would
  conflict its close) and offered again once it lands. And through the gate's
  `intents` key it may open an intent for an outcome several runs serve that
  nobody named, or close one whose done condition it can show met, clause by
  clause. Both bars are high and held in its stage guidance. A pulse never
  closes the same intent twice: after `moe intent reopen`, only the operator
  closes it again.
- **The dash.** Open intents render in a standing `INTENTS` section above the
  backlog, on both the CLI dash and `moe serve`, with the counts of linked work
  open and landed and a `pulse` mark on one a pulse opened. The heading renders
  even at zero: an empty list is itself a signal that the robots are running
  unaimed. A closed intent is a finished goal and gets a `COMPLETED` row, noted
  `closed by pulse` when it was; the web intent page lists its linked work and
  offers reopen once it is closed.

A link follows its work when an idea is promoted and to an `sdlc reopen`
successor. Followups harvested from a linked run start unlinked: some serve the
goal and some are tidying, and the next pulse judges each.

## Chores

Recurring maintenance otherwise lives in your memory or in a cron job you don't
trust an agent to run unattended. A chore is standing intent instead: it turns
recurring project maintenance into a run MoE knows how to open. A chore
definition says what maintenance is due, when it becomes due, and which workflow
run to open for it. MoE evaluates chores against the journal; `moe chore open`,
or a pulse's chore auto-open, mints the seeded run. What happens after that is
what you armed: with no `moe serve --dynamic` up, the run waits in `moe dash`
like any other until you start it, while under an armed serve the
[heartbeat](#the-heartbeat) notices the chore coming due, sweeps, and that sweep
both opens the run and rides it on through build, review, and ship. A chore
coming due is a clock event on an otherwise quiet journal, so the heartbeat
gate watches for it directly rather than waiting for something else to move.

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

All registrations appear in CHORES between INTENTS and BACKLOG on `moe dash`,
`moe ui`, the web dashboard and each project hub. Due chores come first; the
rest sort by project/name. Rows show an open run, a local cooldown eligibility
time, a due reason, `judged — pulse decides`, or `not due`, alongside the
condition or schedule. A judged row records no recent LLM verdict, and an
eligibility time promises no execution time. A linked run keeps its ordinary
run row; registrations count neither as active runs nor as backlog ideas.
Project filters apply, while workflow filters omit registrations.

Select a chore in `moe ui` to inspect its full condition or trigger/cadence,
cooldown, last completion, cooldown eligibility, linked run, workflow and seed
prompt. Tab to the details pane and scroll to reach long text. Web rows link to
the chore detail page and its open run. `moe chore list` remains due-only and
omits judged chores; `moe chore check` dry-runs all families.

Manage definitions and open runs from the CLI:

```sh
moe chore new <project>/<name> [--cadence D] [--cooldown D]  # create in the editor
moe chore edit <project>/<name> [--cadence D]  # edit those fields, or open the editor with no flags
moe chore rm <project>/<name>                 # remove the definition
moe chore list [--project <p>]                # show what's due
moe chore check [--project <p>] [<project>/<chore>]  # dry-run validation and due-state
moe chore open [--now] [--park|--ship] <project>/<chore>  # open the seeded run for a due chore
moe chore skip <project>/<chore>              # clear a due chore until it is next triggered
```

`new` opens the definition form in your editor with any flags prefilled. `edit`
with flags changes only those fields; without flags it opens the form. See
[Chore definitions](reference.md#chore-definitions) for all flags, the form and
recovery after a refused save. Removing a definition leaves an existing run
intact; it retires the registration.

On the web, use the project's `new chore` link to create a definition. A chore's
page offers edit, delete from the edit page, and open-now. In `moe ui`, use
`:new-chore <project>/<name>` to create one, or select a CHORES row and use `e`
or `:edit` to edit, `:delete` to remove, and `o` or `:open` to open its run now.
Save the console form with `Ctrl-S`; see the
[console reference](reference.md#re-entry-and-supervision) for details. Skip
stays CLI-only with `moe chore skip`.

`moe chore open` refuses if the chore isn't due, already has an open run, or is
cooling down. Pass `--now` to open it anyway when it's cooling down or not yet
due — it still refuses if a run is already open. Web and console open-now also
waive due-ness and cooldown, refuse an already-open run, and start no agent.
Work starts through a later eligible [armed sweep](#the-heartbeat) or an
explicit terminal stage command.

You can also edit a definition's files directly or use an sdlc run: a stage's
per-turn commit picks up `projects/<project>/chores/`, so
`moe sdlc design → build → close` is the journaled route. For a mechanical
chore, subject to the open-run and cooldown guards, a definition edit committed
by any means makes the chore due as "definition changed." A judged chore's
edited criterion remains for the survey to evaluate; editing it does not make
the chore mechanically due.

## Pulse

The pulse is the bureaucracy's one read-only survey. It covers every project,
feeds the backlog, and names the one run that should happen next. "Work just
landed — what's next?" is a reflex worth automating, but only inside consent
bounds: **the only pulse that fires on its own is the clock's**, and the only
clock is one you started — `moe serve --dynamic` carries a resident heartbeat
(see [the heartbeat](#the-heartbeat) below). No verb tails a sweep. A ship is
quiet whatever bang drove it, and `moe sync` never pulses. `moe pulse new` is
the manual valve; it takes no project, and naming one is refused.

A pulse lives in **`_cabinet`**, the root project that stands for the
bureaucracy itself. `_cabinet` has no submodule or remote and is created the
first time something needs it. Besides the pulse it holds top-level intents,
which reach every project's prompts above that project's own, and top-level
ideas: work you can log without choosing a project (`moe idea new
_cabinet/disk-ran-out`). Source stages refuse it, so `moe sdlc new _cabinet/x`
is an error. The survey reads each project's canonical checkout at
`projects/<p>/src` by absolute path, so a pulse needs no clone.

The server is serial: an armed serve has one working agent across the whole
bureaucracy. Each sweep picks one run, that run rides, and the next sweep picks
again, so order is recomputed every time and never stored. A dependency between
runs is prose on the blocked run's canvas, and the survey is its reader. What
that costs is throughput and latency: a survey before every run, and up to a
five-minute tick between them. What it buys is that nothing has to be
hand-sequenced, and a run that was waiting on another project becomes visible
the moment that project's work lands.

The LLM supplies content judgment: what warrants a followup, a bounded fix,
a design investigation, a judged chore, a repair or the next pick. The harness
validates those nominations and enforces consent, deduplication, readiness and
execution. A heartbeat tick first checks mechanically whether a sweep is
warranted; it does not automatically spend an LLM turn.

Every pulse does three things:

- **Chore auto-open (always).** Every *mechanically* due chore in a non-paused
  project gets its run opened — the same seeded run `moe chore open` would mint — and
  nothing more. No stage executes; the opened runs wait in `moe dash` like any
  other. This is automation acting on a chore definition you authored, never a
  fresh decision. Judged chores are not opened here: their condition is the
  survey's to evaluate, below.
- **PR reconcile (always).** Each non-paused project's `pushed` runs are
  checked against GitHub — the same walk `moe sync` does — so a PR you merged from your phone reads as `merged` before the survey looks at the
  delta. Pointer bumps stay `moe sync`'s job. Offline or without `gh`, this
  warns once and the sweep carries on.
- **The survey (every fire).** A headless, read-only agent sweep. Its kickoff
  lists each project's source and carries one section per project with a delta
  since the last pulse or live work; paused and quiet projects are named and
  skipped. It reads the journal since the last pulse, the twin where work
  landed, and the open backlog; files followups with `<project>/` routing; and
  writes a short report ending in a machine-readable `## Gate`. It also judges
  **judged chores**: does what landed meet the condition the operator wrote?
  `status` says the survey concluded. `loose` opens runs, each slugged
  `<project>/<slug>`; an entry may set `"design_only": true`, the rung between
  filing a followup and proposing a fix. `next` names the one run to ride, with
  a reason. `serves` links work to intents, and `intents` opens or closes them;
  see [Intents](#intents). A clean sweep auto-closes its own run: the filed
  followups harvest straight into ideas (review them by scrapping on the dash).
  Every fire runs a fresh sweep unconditionally — a lingering open pulse run
  means a failed or abandoned sweep, sitting visible on the dash's ACTIVE list
  until you inspect and close it, but it never blocks the next survey.

```sh
moe pulse new                            # run the whole pulse by hand (chore auto-open + survey)
moe pulse cat _cabinet/<run> pulse       # read a sweep's report
moe pulse close _cabinet/<run>            # close a failed or interrupted sweep by hand
```

A sweep is machine-paced and has no re-open verb: a failed one is read with
`cat` / `log`, ended with `close` (the filings still harvest), and retried by
running another. The survey blocks with a `Ctrl-C to skip` banner. Interrupting
before a pulse run opens leaves no run to clean up. If the run has been minted
but the survey agent has not started, MoE closes the unused run with a skip
note. Once the agent has started, interrupting leaves the run open for
inspection and closing. An interrupted survey applies no gate proposals and
starts nothing, and the next survey starts fresh; chores already opened by the
pre-survey pass are not rolled back.

`moe pulse new` is also the verb an external cron would call — the primitives
are cron-safe, but MoE ships no cron of anyone else's. The one clock it does
carry is the armed serve's own heartbeat, described below.

The survey's first turn carries a GitHub context block the harness gathered:
PRs merged since the last pulse (marking the ones that landed outside moe, which
the journal never saw) and the latest CI verdict per workflow on the default
branch. It's what makes "what landed" honest when work reaches the repo without
going through a run.

Current checks on recorded PRs belonging to pushed sdlc runs appear in a
separate block, including the head, check URLs and prior repair claims. A survey
can pick `{"run":"<project>/<slug>","why":"…","repair":"ci"}` as its `next`.
Under dynamic consent, build first passes quota admission; the harness then
rechecks the current PR and claims an attempt in the journal before
implementation. That build turn implements and verifies the repair, stamping
both build canvases ready before a fresh review follows. Then that PR is updated
without merging. Safe-mode heartbeats still need an operator mark;
add a note with `moe input add <project>/<run>` to authorize repair. One
automatic attempt is allowed per head across restarts. A fresh note can
authorize a retry on the same head, and each note can license only one claim.
Quota deferral before the claim spends no attempt. Deferral after the claim
stops the repair, keeps the claim spent, and requires operator recovery or fresh
input; it creates no automatic repair continuation. Unavailable checks are
unknown, and successful, pending or cancelled checks do not nominate repair. The
survey judges whether failure warrants a repair; the heartbeat never starts
build directly.

### Proposed work and the next run

Most of what a survey finds becomes a followup. When it finds work that is
mechanical, bounded, and verifiable — a red check with a named failing test,
documentation the code plainly contradicts — it may open or nominate an
ordinary `sdlc` or chore run in `loose`. Opening a run does not start it.

One more rung sits between those two. A `loose` spec carrying
`"design_only": true` opens a run that, when picked, rides exactly one stage — a
headless design turn — then parks at design. The `design` body is the survey's
*brief* rather than a baked design, so the bar is lower: a finding worth a
designer's hour, not a fix worth a ride. An idea the operator tagged design-only
(`moe idea tag --design-only`) reaches the same rung from the other side: the
tag carries the bit, and the promoted run gets the identical one-stage ride with
the idea canvas as its brief. The run is then held the way any unadvanced run is
held, and your exits are the run page's advance chip (a full ride follows when a
later sweep picks it), a pushed note (one more design turn), or close.

A waiting note buys one stage on an otherwise-unsettled run still at its first
stage, including a design-only one. It does not cap a run already eligible for
a full ride. Once the note is delivered, the run is held again only if it is
still unsettled and has no other admission.

An otherwise-unsettled, operator-opened design that parked itself with an
authored blocked first gate on a prerequisite it filed, naming that followup's
slug in backticks inside its `## Gate` section, is admitted for the full ride
once the prerequisite's run merges or closes (or its idea closes unpromoted)
after the design's last turn. Design-only runs are excluded from this
exception. The survey sees such a run marked `due` in its parked list; a
dynamic pulse must pick it, and other holds still apply. The resumed design
re-checks the evidence and stamps its own gate; a ready gate rides on to build.
Closure may mean the prerequisite was dropped, so any code still needed must
have landed on default; a pushed PR is not settled. One settlement buys one
resume: a re-check that stays blocked cannot reuse that older settlement, but a
newer settlement of a named filing or a waiting note may admit it again.

A spec's `design_only` is skipped on a slug that already names live work, on a
chore entry, and on a spec with no `design` body; on an already-design-only
idea the flag agrees with the tag and is ignored. The bit stays in `run.json`
afterwards as provenance.

`next` is the only way anything starts:

```json
{"status": "ok",
 "next": {"run": "moe/fix-red-main", "why": "every other push waits on main"}}
```

It may name a parked run in any project, a run this gate's `loose` just opened,
or a tagged idea, which is promoted only when picked. An untagged idea stays
yours. A hand-typed `moe pulse new` starts nothing. Under `--dynamic`, the
harness rides the pick and nothing else. The shared floor holds seed-only runs
unless admitted as described above, and holds any run with a live session or
execution owner, an operator hold, or a dead machine turn nobody has touched
since (the reap's tombstone, below). Clock-started rides also obey project
mode. A hand-typed `moe pulse new --dynamic` follows its typed consent. A
machine-baked, chore-authored, or past-first-stage run has a settled design; a
design-only spawn rides its one design stage and is then held unless admitted
again as above. A held pick prints the floor's reason and starts nothing. Human
activity on a sibling run and fresh operator commits impose no project-wide
hold. Keep ideas untagged while arranging them; project mode and the global
pause provide broader holds.

The sweep stamps its pick onto its own canvas as a closing `## Next` section, in
the same commit as the close, so a sweep's report says what it was about to run
and not only what it filed. It records a plan, not a promise, because the floor
is asked again when the ride starts. Curation sweeps stamp nothing.

### Prose, both directions

You can push a chunk of prose at any in-progress or pushed run, and its next
agent turn receives it verbatim. Open ideas also accept notes; when an idea is
promoted, its undelivered notes carry to the destination run. Closed, merged,
and promoted records refuse new input.

```
moe input add moe/change-auth-defaults "The failing test is a known flake — skip it and ship."
moe input add moe/change-auth-defaults          # no text: reads stdin
```

That is the stuck-run loop: from the terminal or from the web's `/input` queue
on a phone, write a sentence, and the next turn on that run acts on it. The
note starts nothing — it writes one journal commit, so an armed serve's next
heartbeat offers the board to a pulse, and the survey is told to pick a parked
run with a waiting note first unless the floor holds it. `moe pulse new
--dynamic` is the expedite path. A note can steer further work on an open PR or
accompany an idea into its eventual run. A pending note counts as an operator
mark under `safe` mode, so eligible work can continue on a project that
otherwise starts nothing on its own. It neither tags an untagged idea nor
advances a stage.

Delivery is once. A pending note reaches exactly one *successful* turn, then
lives on the web run page and in the selected item's details in
[`moe ui`](reference.md#re-entry-and-supervision) as saved history. The canvas
that turn writes is where durable direction is supposed to persist. A turn that
fails marks nothing, so the next attempt redelivers.

The other direction: a run can ask *you* something. It shows up on the dash as
`· ask?`, in `moe input list`, and on the web's `/input` queue with a reply box;
`moe input answer moe/change-auth-defaults "<prose>"` fills it, and the run's
next turn gets the question and your answer as a pair. One open question per
run. Asking holds nothing by itself: turns are told the question was asked and
no reply came, and they proceed on their best judgment and note the call on the
canvas.

A stopped review loop opens such a question on the run. Its hold is separate from the question; answering it or adding
any note releases that hold for one more review turn. See
[Blocked gates](#cascades-the-bang-vocabulary) for the recovery path. Input
starts no agent and does not clear an explicit pause or handoff.

There is no dismiss verb. A question you'd rather not answer is discharged by
answering it — "not relevant, proceed" is prose the next turn can read.

### Notes to the pulse

A run note steers one run. To steer the survey that decides what runs next,
write to the pulse itself:

```
moe pulse note "do the lightpool fix next"
moe pulse note                                   # no text: reads stdin
```

The web dash's PULSE section has the same composer (Ctrl/Cmd+Enter sends), and
`moe ui` opens one with `p`. The next sweep that concludes reads every pending
note in full, once. An ordering hint outranks the survey's own ranking unless
the floor holds the named work; a piece of work becomes a filing or a `loose`
spec; standing direction ("prefer lightpool this month") becomes an intent in
your words; anything the survey cannot do comes back as the verb you need. The
report's `## Operator notes` section says what happened to each, and the PULSE
section links every read note to that report. A note is shown once: what must
outlast the sweep has to leave as one of those artifacts.

### The heartbeat

`moe serve --dynamic` is the standing consent rung, and it licenses exactly one
thing: a resident heartbeat that looks at each project's board every five
minutes and runs one `moe pulse new --dynamic` when any of them has something
worth looking at. While a sweep or the ride it started is running, no other
starts. That heartbeat is the only automatic pulse there is, and the
only thing in the process that starts an agent at all — the web writes licences
and marks, and the clock spends them. Stopping serve stops new sweeps. An
existing Rex-hosted sweep and the rides it carries survive serve shutdown; an
armed restart with a usable Rex host adopts them, and Ctrl-C in the sweep's Rex
block cancels them. Local sweeps stop with serve. See
[Re-Entry And Supervision](reference.md#re-entry-and-supervision) for finding
the session, checking the host, and reading failed blocks.

`moe serve pause` pauses new automatic sweeps across the bureaucracy for 24
hours; `moe serve unpause` clears it early, and `moe serve status` reports its
expiry in local time. The commands work with serve stopped. Pausing an active
pause is a no-op and does not extend the window. The web dashboard's PULSE
section and `/serve` offer **Pause for 24 hours** / **Unpause**; `moe ui` offers
`:serve pause`, `:serve unpause`, and `:serve status` without a selected run.
Active pause or unreadable state appears in the PULSE status line; refresh
observes remote writes and expiry. Stale opposing web/UI controls refuse and
show current state.

A pause caps admission of new automatic sweeps: an already-admitted survey and
its selected ride may finish. It does not stop current work, arm serve, clear
project modes or reset failure/quota history. Unpause and expiry permit the next
otherwise-eligible five-minute tick; neither starts anything immediately. Human
stage commands and typed dynamic pulses remain usable, as do HTTP, journal
pushing and reap/GC hygiene. The tracked root `serve-pause.json` stores a revision
and optional UTC expiry; malformed or unreadable state holds automatic starts
and reports an error. Expiry is read-time only and creates no commit. There is
no migration from the ignored legacy `.moe/snooze` file.

An unarmed `moe serve` is a reader with a capture door.

Per project, `moe project ship <id> pr|merge` picks how a finished run lands
there. New registrations store `merge`: fast-forward the default branch,
delete the remote branch, drop the sandbox. Existing projects keep their
settings; an absent ship field still means `pr`. To select PRs, run
`moe project ship <id> pr`. The PR route pushes the branch and opens a PR,
leaving the run `pushed` with its sandbox until the PR merges (the pulse's
reconcile then flips it to merged or closed). Every unflagged ship — bare
`moe sdlc push`, `!!`, and the heartbeat's rides —
follows the project setting. `--pr` / `--merge` on push, and `m` / `p` at the
chain prompt, override it for one ship. The project hub carries the switch
beside mode's.

Per project, `moe project mode <id> paused|safe|auto` caps what that clock may
do. `paused` means the survey leaves the project alone and a pick there starts
nothing. `safe` means it is surveyed as ever but a pick starts only a run you
marked — a stage you advanced, a chore's standing intent, a workflow tag on the
idea a run came from, a note or answer you left on it that no turn has
delivered yet — holding anything else with a named reason. `auto` is the
default. The mode binds the clock, not you: bangs, stage verbs and a hand-typed
`moe pulse new --dynamic` run in every mode, because the typed word is the
consent. Serve's **advance mark** is that same consent from the phone: it
records the stage you just read as done — one journal commit, no agent — and
`safe` starts on exactly such a mark. The project hub carries the same
three-way switch. The PULSE status line counts paused projects; safe mode
remains visible in project controls.

Both surfaces show what the heartbeat is doing, in the PULSE section that opens
the board. `moe dash` reads the snapshot serve keeps on disk and carries its
status on the section's first line — armed or not, how long it has been up, when
the next tick lands — spending a line on a project only when that project is
sweeping, cooling off after a failure, or its last sweep died. The same line
reports `N unpushed` while commits are waiting and `push failing 12m · 3
unpushed` with a row carrying git's reason when origin is refusing. An empty
queue stays quiet. The count comes from git at render, so it is honest about a
serve that has crashed or was never running; the rest comes from the pusher's
own record. The web dash's PULSE section carries the same line and refreshes
every five seconds while visible, plus on return to the tab; other pages' top
line is the logo and the local date and time. Without JavaScript, reload
refreshes status. The line links to `/serve`, which holds the trace: what each
recent tick decided and the output tail of any sweep that failed.
Repeated identical tick decisions can share one row, dated by the latest tick
and showing the count and first occurrence (for example,
`48 ticks, first 4h ago`). Quiet ticks add no rows, and a sweep event separates
groups. That trace is the running process's memory rather than history — a
restart starts it over, and the durable record of a sweep is still the run it
opened.

The tick decides nothing. It only asks the question you used to ask by typing a
verb — everything about what a sweep may *do* is unchanged, including the
settled-design floor, the occupancy guard, the build-and-review walk on every
ride, and the journal marks on every machine turn. A tick sweeps when the
project's journal moved since its last heartbeat sweep; when a chore's clock
says so (a mechanically due chore, or a judged chore whose cooldown has expired
without a sweep since — one probe per expiry, not one per tick); or when
startable work is parked with nobody inside it *and no sweep has looked at that
board since it last changed* — a settled run, or an open idea you tagged, since
the tag is the licence to start it. A run a survey saw and did not pick is not
re-offered until something moves; every release from a hold is a journal commit.
A human session or owner on one run holds that run, while another run in the
same project can be surveyed and ridden. Fresh human commits require no quiet
window. Human and ride commits arriving during a survey remain unsurveyed and
wake a later eligible tick. So does a run the survey itself opens: if it is
startable and the survey did not pick it, the next tick sweeps once to look at
it. A quiet board costs no agent turn, run or journal line. Recorded pushed PRs
are polled at this cadence outside the journal lock. A merged or closed owned PR
wakes a pulse to reconcile its still-pushed run before the survey reads the
board. It keeps waking eligible ticks until reconciliation succeeds, including
after a restart. This follows the five-minute cadence and existing holds and
failure backoff; it is not an immediate refresh. Run `moe sync` for immediate
reconciliation. A new actionable failure can also wake a survey even when the
journal has not moved. Stable failure observations are acknowledged only after a
successful sweep, against the exact snapshot handed to that survey; failures
arriving during its rides remain visible to the next tick. Healthy and pending
changes alone spend no turn. No recorded pushed PR means no added network read.

Known survey quota exhaustion records a retry before opening a pulse run.
Until its deadline, every automatic wake waits for the same effective survey
backend/model, including new notes, journal changes, chores and PR changes.
Held ticks make no provider calls or journal commits. A changed selection can
recheck on the next eligible tick. Ride deferrals remain scoped to existing
runs and do not hold other survey wakes. The survey rejudges the current board;
it never resumes an old pick. Project mode, global pause, ownership and failure
backoff still bind. Stopping or unarming serve leaves retry dates inert. Pure
quota deferral neither acknowledges the board or PR checks as fully surveyed
nor increases the failure streak, and sends no routine exit webhook. Existing
failure history stays until a fully clean sweep.

PULSE on web, dash and UI shows the recorded backend/model, waiting reason and
local, zone-marked eligibility time, even after restart or with serve stopped.
At expiry it says eligible for quota recheck. The tick countdown is a check
time, not a promised survey start. Pending notes remain waiting, and the last
real pulse link stays available. Paused or unarmed serve remains the primary
explanation of why nothing starts; quota is additional context. The existing
web status refresh updates and clears this evidence without reloading.

Failure cools itself off: consecutive failed sweeps back the one sweep off
exponentially, so a dead network or unavailable quota interface does not mint a
pile of pulse runs. Unavailable survey quota and cancellation during admission
also create no run. Ordinary failures after successful admission keep their
recovery evidence. An agent that started and died leaves its real artifact open
on ACTIVE; the heartbeat sweeps past that open run rather than letting one
failed sweep wedge the board. In a mixed outcome, real failures take precedence
over quota deferral; failure backoff and any recorded quota retry both apply.

The heartbeat also recovers machine sessions whose matching claim proves the
owner dead — same host and PID namespace, process gone, heartbeat stale. It
checks ownership again before acting and leaves a session resumed meanwhile
alone. Recovery first tries normal session close: if that succeeds, committed
work lands in the bureaucracy journal. The run stays open at its stage;
recovery itself neither advances nor ships it, and leaves no reap tombstone.

If close refuses because there is no committed canvas change, recovery stamps
a tombstone before abandoning the session so the run re-parks. The tombstone
names the unfinished stage, the reap time (not the agent's exit time), and the
abandoned branch tip. `moe dash` marks the row `· unfinished`; the run page and
reap commit point to `moe <workflow> log <project>/<run> <stage>` for the recorded
outcome, when available. Reaping alone does not establish why the turn stopped.
Older turns may lack archived diagnostics; the abandoned tip may contain
session evidence readable until Git GC prunes it. Archived diagnostics are
retained separately. Other close failures leave the session in place for
inspection and recovery; the log names `moe session resolve` and
`moe session abandon`.

For an abandoned session, the tombstone is a brake as well as a record, and that
is what keeps one refusal from costing a stage turn every sweep: a headless
stage that refuses because the work needs you can leave no committed canvas
change, so with no brake a later pick could run the same stage again. While the
note stands the floor holds the run and the heartbeat's pre-ask skips it. Any
touch of your own on the run — a `moe input` note, an advance, anything landing
a journal commit the machine didn't stamp — releases it for one retry,
and opening a stage there clears it outright. If that retry refuses too, its
fresh tombstone re-arms the hold: each thing you do buys one attempt and no more.

Claim-based recovery leaves operator sessions and sessions with unknown
ownership alone. Separately, the heartbeat runs the same orphan rules as
`moe session gc`: registered sessions on terminal or missing runs/projects,
and branches without worktrees, remain eligible when their claim is genuinely
absent. A present claim must name that exact branch and prove its owner dead,
whether machine or operator. Unreadable, malformed, mismatched or uncertain
claims hold the session for inspection, even on terminal runs. Release alone
does not authorize GC. `moe session list` shows held branches too, marking those
without a registered worktree as `(no registered worktree)` in the path column.
Reported paths describe Git's registrations, not directory health. Inspect and
preserve unfinished work and stop its owner before explicit recovery; listing
alone establishes no permission to discard it. `moe session resolve` can clean
up a landed branch without a worktree, but an unlanded branch needs a worktree
restored before resolve can land it. `moe session resolve` / `moe session abandon`
are still yours. See [Cleanup And Recovery](reference.md#cleanup-and-recovery)
for manual recovery conditions.

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
harvests into an idea after each successful turn, with close as a backstop. See
[concepts.md §Feedback Channels](concepts.md#feedback-channels) for how the
twin steers future runs.

## Hooks

Some project guidance has to be deterministic rather than prose an agent
interprets: bringing up a dev environment, tearing it down, gating a push.
Hooks are that layer — drop-in executable scripts with no manifest, discovered
by directory.

`moe hook fire <project> <event>` is the iteration loop: it creates a transient
sandbox, runs one event's scripts once, prints `sandbox: <path>`, and exits. Each
fire gets a unique wrapper at `.moe/hook-fire/<project>/hook-fire-<unique>/`, with
the clone in its `sandbox/` child. The next fire removes only wrappers with a
valid harness completion record written after successful dispatch. Failed,
interrupted, ambiguous and legacy fires under `.moe/clones/` remain for manual
inspection/removal; run clones are never scanned. Firing `dev-env` alone still
leaves its resources running; completion is scratch-disposal permission, not a
teardown receipt.

No run is needed to test a 30-line bash change. Landing a change is a plain file
edit, or an sdlc run whose per-turn commit picks up `projects/<project>/hooks/`
(`moe sdlc design → build → close`) when you want the edit journaled with a canvas.

The hook events themselves (`dev-env.d`, `dev-env-teardown.d`, `pre-push.d`,
`session-cleanup.d`) and per-project dev secrets are covered in
[reference.md §Hooks And Environments](reference.md#hooks-and-environments).

## Subscription quota cooldown

Before each top-level agent stage, including the first stage of a ride,
MoE checks the selected backend's subscription usage without running a
model turn. At 95% used, operator-invoked stages and rides wait for all
applicable blocking windows to reset, then read usage again. The wait has a
six-hour total budget per stage; a reset beyond that budget stops immediately.
Unsupported quota interfaces and malformed readings stop clearly. Transport
errors retry twice, one minute apart. While waiting, configuration is checked
once a minute and at temporary-default expiry. An effective backend or model
change reconciles the session and checks the new selection's quota under the
original budget. This does not increase provider polling. Explicit choices
that mask a default change keep their selection; executing agents stay put.

Heartbeat-invoked stages instead read once and yield immediately when blocked.
A survey admits before minting its run, then carries that exact selection into
its stage without a second quota read. Its deferral journals only survey
admission evidence, preserving outstanding ride obligations until a real survey
rejudges them. For rides, the pulse journals the earliest retry among deferred
roots. Both use each stage's latest blocking reset plus five seconds. If any
blocking reset is missing or past, the next ordinary tick can retry. Weekly
resets may defer for days without retaining a process. Unavailable quota,
including unresolved high model scope, refuses without transport sleeps and
uses failure backoff. Every later stage admission reads fresh quota; a retry
date never admits work on its own. A different effective backend/model on a
deferred target offers a fresh survey on the next otherwise eligible tick,
including after restart; project mode, the global pause, same-run ownership and
failure backoff still apply. Configuration errors preserve the obligation
rather than claiming a changed choice. Typed dynamic pulses keep the ordinary
wait policy.

At an interactive stage's attached terminal, `c` + Enter continues that stage
now, `a` + Enter skips checks for the rest of this invocation, and `q` + Enter
or Ctrl-C cancels. Headless and serve-hosted stages read no terminal input. Add
`--no-cooldown` anywhere before `--` in the launch command to skip checks for
the entire invocation, including its cascade and rides. For example:

```sh
moe sdlc build --no-cooldown --ship myproject/my-run
```

A quota refusal returns 75 and leaves the stage unstarted; a clock pulse with
only quota deferrals returns 76; Ctrl-C returns 130. Re-run the printed stage
command to resume an operator-invoked stage. Existing resumed work is
preserved. Five percent headroom cannot reserve quota or guarantee that a
stage finishes. Bypassing may consume paid credits if the agent's existing
account settings allow it; MoE does not enable overage.

The adapters target Claude Code 2.1.316's experimental `get_usage`
control interface and Codex CLI 0.167.0's app-server
`account/rateLimits/read`. They use the executor's authentication filter
and dev-env overrides. An incompatible CLI or unsupported login stops
with the bypass route rather than assuming zero usage.

Claude's experimental response must establish freshness. Model-specific
windows with no usable model identity remain unresolved: low readings
can admit, while a reading at or above 95% stops with the bypass route.
MoE never identifies a model from its display label. Codex snapshots
from an unknown quota pool likewise stop rather than being treated as
the selected model's allowance.
