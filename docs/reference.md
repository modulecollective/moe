# Reference

Look-things-up material: the command catalog, backend setup, shell completion,
hooks and environments, and cleanup. `moe help` and per-command usage are the
source of truth for the exact command surface; this page is a map.

## Command Catalog

Global `--no-cooldown`, accepted anywhere before `--`, skips subscription quota
checks for the entire invocation, including cascades and rides. See
[Subscription quota cooldown](#subscription-quota-cooldown) for the wait policy,
controls, exit codes, and consequences of choosing to bypass it.

### Re-Entry And Supervision

- `moe run list [--project <id>] [--workflow <name>] [--all]` prints the
  board's runs as text, for agents and scripts: one line per run,
  `<project>/<slug>`, workflow and note separated by tabs, with no header,
  colour or ages. It lists active runs newest first; `--all` adds every
  completed run after them. The notes and filters are the board's own. An
  unregistered `--project` is an error; an unknown workflow lists nothing.
  Open ideas and intents have their own `list` verbs.
- `moe agent [codex|claude|reset]` reports or sets the global default backend
  for 24 hours. Flags, persisted run bindings and `MOE_FORCE_AGENT` still win.
  Re-selecting an active backend keeps its expiry; choosing the other starts a
  fresh window. Reset returns to the current stylesheet. The web dashboard menu,
  even when serve is unarmed, and `moe ui`'s `:agent` command offer the same
  control. The status describes defaults; stage banners identify executing
  backends.
- `moe ui [--project <id>] [--workflow <name>]` is the dashboard as a console:
  the whole board, every completed run included, above the selected item's
  details and a console of recent outcomes. See [The
  console](#the-console-moe-ui).
- `moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]` runs the moe
  server: the web UI, journal pushing, and an optional heartbeat. See [The
  server](#the-server-moe-serve).
- `moe serve pause|unpause|status` controls a global pause of new automatic
  sweeps. Pause lasts 24 hours; repeating it while active does not extend it.
  These commands work with serve stopped. See [The
  heartbeat](workflows.md#the-heartbeat) for what a pause holds.
- `moe chore list|check|open|skip` lists due project chores, dry-runs a chore
  definition, opens the run a due chore configures, or clears a due chore until
  it is next triggered. `moe chore new|edit|rm` registers, changes or removes
  a definition; see [Chore definitions](#chore-definitions).
- `moe control show <project>/<run>` prints a stageful run's live owner,
  phase, hold and request log. It submits nothing.
- `moe control pause|handoff|park|finish|resume [--id <id>] <project>/<run>`
  steers a stageful run at its next boundary: pause and handoff hold it until an
  explicit resume; park has the current owner yield once; finish ships it with
  `!!`; resume releases a hold and starts nothing. A retry with the same `--id`
  reads back the first outcome. See [Seeing and steering a running
  run](workflows.md#seeing-and-steering-a-running-run).
- `moe input add <project>/<run> [text ...]` pushes a chunk of
  [prose](workflows.md#prose-both-directions) at a run; with no text arguments
  it reads stdin, so a paragraph can be piped. The run's next *successful* agent
  turn receives it verbatim in its prompt, once, and then it lives on the run
  page as history — the canvas that turn writes is where durable direction
  persists. `moe input answer <project>/<run> [text ...]` fills the run's open
  question, asked by a stopped review loop (one open per run, so the run is the
  address). `moe input list [project]` shows notes for the pulse no sweep has
  read (board-wide only), then both halves, oldest first: what has asked you,
  then what you have given and no turn has picked up.
- `moe usage [--project <id>] [--since <dur>]` sums the token usage recorded in
  every mirrored stage transcript. See [Usage And Context
  Reports](#usage-and-context-reports).
- `moe context-use [--project <id>] [--since <dur>] [--by-backend | --sessions]`
  reports which steering context agents actually read. See [Usage And Context
  Reports](#usage-and-context-reports).
- `moe where` prints the resolved bureaucracy path.
- `moe version` prints the moe version.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context. Both `cat` and `log` accept `@latest` in the
  `<run>` slot to mean the workflow's most-recent run.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy in the explicit
  directory, otherwise `$MOE_HOME` if set, otherwise the current directory.
- `moe project add <repo-url>` registers a target project with `merge` as its
  ship route. Existing projects keep their settings.
- `moe project list` lists registered projects with their mode and ship route.
- `moe project mode <id> [paused|safe|auto]` reads or sets a project's mode, the
  standing cap on what the heartbeat may start there. See [Project Mode And Ship
  Route](#project-mode-and-ship-route).
- `moe project ship <id> [pr|merge]` reads or sets how a finished run lands in
  the project. See [Project Mode And Ship Route](#project-mode-and-ship-route).
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe <workflow> close <project>/<run>` closes a run; for `sdlc` it abandons
  the run instead of shipping it through `sdlc push`. Harvest reads the
  followups and feedback files as they are; no editor opens. Capture workflows
  (`idea` and `intent`) skip harvesting.

### Workflows

- `moe sdlc new|design|build|review|push|close|harvest|shell|reopen|cat|log`
  drives designed code work.
- `moe chat new|chat|close|harvest|cat|log` drives thinking-partner sessions.
- `moe idea new|edit|close|list|move|tag|untag|reopen|cat|log|harvest` manages
  backlog notes. `tag` stamps the workflow tag that licenses a pulse to start
  the idea (`untag` clears it), and `tag --design-only` narrows that licence to
  one design turn before the run holds; untagged ideas are operator-only.
  `moe idea list [--project <id>]` prints every open idea as `<project>/<slug>`,
  sorted; `--project` narrows it to one project.
- `moe intent new|edit|close|reopen|runs|list|cat|log|harvest` manages
  intents: standing direction and goals. `runs` lists the work linked to one,
  and `reopen` undoes a close, the pulse's included, with links intact.
  `moe intent list [--project <id>]` prints open intents the same way.
- `moe pulse note [text ...]` writes a note to the pulse itself — a piece of
  work to get done, or what should come next; with no text arguments it reads
  stdin. The next sweep whose gate concludes reads every pending note in full,
  once, and its report's `## Operator notes` section says what it did with each:
  a `next` pick, a filing, an intent for standing direction, or the verb you
  need. See [Notes to the pulse](workflows.md#notes-to-the-pulse).
- `moe pulse new|close|cat|log` runs and inspects the bureaucracy's one survey.
  It lives in the `_cabinet` root project and takes no project argument. The
  survey is read-only; opening due chore runs does not execute their stages.
  `moe pulse new --dynamic` rides the one run the survey names as `next`, which
  may be a CI repair on an existing pushed PR. Without `--dynamic`, nothing
  starts. [The server](#the-server-moe-serve) describes how the heartbeat calls
  it and reads its exit status.

`moe <workflow> harvest <project>/<run>` re-runs a run's `followups.md`,
`feedback/lore.md`, and `feedback/twin.md` harvests — into ideas, `lore/`, and
twin-edit ideas respectively — without closing it. It is the recovery verb for
a turn whose own harvest failed partway, and it picks up captures regenerated
after the run was already closed.

Every successful stage turn harvests all three files when it ends: SDLC design,
build and review turns (even when the gate stays blocked), CI repair and
hook-failure turns, and conversational sessions (`moe idea edit --chat`,
`moe intent edit --chat`, `moe chat`). A project-document fix turn does not
harvest; its outer turn does. Failed and refused turns extract nothing. No
harvest opens an editor, and a terminal transition harvests again as a
backstop. A tagged followup becomes an idea the pulse may start; an untagged
one becomes an idea only the operator promotes.

Every followup extraction uses a journal receipt (`MoE-Followup-Key`) keyed by
source project/run and target project/original slug. Retry reuses the original
idea after promotion or closure, including a collision-allocated name. Without
a receipt, one exact or dated live owner is linked without changing its tag or
contents; multiple owners fail for explicit triage. A checked line means
extracted, never completed. Title/body edits do not request another
extraction; use another slug for a separate request. Twin notes and lore have
no receipt.

A design can file a prerequisite and resume when it settles; [Proposed work and
the next run](workflows.md#proposed-work-and-the-next-run) has the rules.

### Project Mode And Ship Route

`moe project mode <id>` sets the standing cap on what the heartbeat may start
in one project:

- `paused` — the survey leaves the project alone and a pick there starts
  nothing.
- `safe` — it is surveyed as ever, but a pick starts only a run carrying an
  explicit operator mark: a valid advance marker, a chore's standing intent,
  a workflow tag on the idea a run was promoted from, or an undelivered note
  on the run — a `moe input add` note or an answered question, consumed once
  a turn delivers it. Anything else is held with a named reason.
- `auto` — the default: a pick starts whenever it clears the floor.

The mode is stored in `projects/<id>/project.json` (absent means `auto`) and is
settable from the project hub's switch as well. [Project mode and ship
route](workflows.md#project-mode-and-ship-route) explains what the mode does and
does not bind.

`moe project ship <id>` sets how a finished run lands in the project:

- `pr` — push `moe/<run>`, open (or re-use) a PR, mark the run `pushed`, and
  keep the sandbox until the PR merges. `moe sync`'s reconcile, or the next
  pulse's, flips it to merged or closed on its own.
- `merge` — the default for new registrations: fast-forward the target repo's
  default branch, delete the remote branch, drop the sandbox, mark the run
  `merged`.

`--pr` and `--merge` on push, and `m` / `p` at the chain prompt, override the
setting for one invocation; giving both flags is a usage error. It is stored in
`projects/<id>/project.json` (absent means `pr`) and settable from the project
hub's switch as well. New registrations store `merge`; existing projects keep
their settings. To select PRs for a new project, run `moe project ship <id> pr`;
this removes the field.

## The Console (moe ui)

`moe ui [--project <id>] [--workflow <name>]` is the dashboard as a console: the
whole board, every completed run included, above the selected item's details and
a console of recent outcomes. It needs a terminal on stdout (Linux or macOS) and
restores it on every exit; quitting mid-save waits for and prints that save's
outcome. The console retains 100 events in memory, with bounded, labelled
message truncation; it never logs note or capture bodies.

Arrows or `j`/`k` select; `Tab` / `Shift-Tab` cycle board, details and console.
A heavy, bright amber pane heading identifies where keys go; without color the
heavy rule remains. The selected target retains its `>` marker. In the console,
arrows, page keys and Home scroll history; End follows new messages. `?` lists
keys. Moving to the first board item reveals the section headings above it. A
board heading's `lines … of …` counts scrollable display lines (including
headings and blanks), not runs.

The shared dash banner stays visible, its clock at the right edge. The activity
histogram is pinned above the scrolling board when it fits, one day per column
across the terminal (up to a year), and collapses on short or narrow terminals.
Resizing restores it. While a run has an open session, today's bar wears a star
that twinkles every 500 ms from cached frames, without scans, socket reads or
status reports; nothing else on the board moves, and an idle board does not
repaint. Pane separators have a blank row on either side, and the console shows
six message rows even on short terminals. The minimum terminal size is 40
columns by 20 rows.

Runs update as their owners publish, not only on the 3s refresh; a lost owner's
facts stay, labelled stale. The board opens with a PULSE section: the serve
status line — armed or not, uptime, next sweep, the global pause, and the last
pulse run — then your pending pulse notes and the last one a sweep read. [The
heartbeat](workflows.md#the-heartbeat) describes what the status line reports.

`n` writes a note to the selected run or idea, `p` writes a note to the next
pulse with no selection needed, `a` answers its open question, `e` edits an open
idea or intent, and `t` reveals the run's existing Rex terminal. On an open
idea, `g` sets the SDLC licence, `d` sets design only, and `u` clears either
licence. Repeating a setting leaves it unchanged and creates no extra commit.
Setting a licence launches nothing; a later sweep may pick up a tagged idea.
These keys keep the selection and work from any pane in normal navigation; in
editors and the command line they remain text.

`:` opens a closed command line — `pause|handoff|park|finish|resume` for the
selected run, `pulse-note`, `new-idea <project>/<slug>`, `new-intent
<project>/<slug>`, `edit`, `close`, `tag-sdlc`, `tag-design`, `untag`, the chore
commands below, `agent`, `serve`, `terminal`, `help` and `quit` — never a shell.
These go through the same writers as `moe input`, `moe control` and `moe
idea|intent`, so they mean the same thing: a note reaches the run's next prompt,
resume and tagging start nothing, and only submitting `finish` ships. `:serve
pause`, `:serve unpause` and `:serve status` control the global pause without a
selected run.

In the command prompt, Tab completes command names and cycles matches in lexical
order; Shift-Tab cycles backward. Type a space before arguments; Enter submits.
`new-i` matches `new-idea|new-intent` and `tag-` matches `tag-design|tag-sdlc`.
`agent` completes `codex`, `claude` and `reset`, and `serve` completes `pause`,
`unpause` and `status`; other values are typed in full. Completion replaces the
word at the cursor, preserves surrounding text and starts no work. For example,
`:new-i<Tab> p/example` becomes `:new-idea p/example`.

Use `:new-idea <project>/<slug>` or `:new-intent <project>/<slug>` to open a
capture draft. Creation requires a lowercase slug using letters, digits and
hyphens, e.g. `:new-idea p/my-idea`; uppercase is refused before a draft opens.
The draft names its target even on an empty or filtered board. Text is edited
inline (`Ctrl-S` saves, `Esc` keeps the draft, `Ctrl-X` discards); nothing is
created until Ctrl-S. In multiline editors, Enter or Shift+Enter inserts a
newline. Alt-Backspace or Ctrl-W deletes the previous word; Alt-Left/Right or
Alt-b/f moves by whitespace-delimited words, including newlines, so project/run
tokens stay together. Saving an idea offers save only, save and tag sdlc, or
save and tag design-only. An edit refuses to overwrite a canvas someone else
changed meanwhile.

The Console heading quietly labels an active temporary default when space
allows. `:agent codex` or `:agent claude` chooses that backend for 24 hours;
`:agent reset` returns to the current stylesheet, and `:agent` shows full status
and local expiry at every width. These work without a selected row, serve or
Rex. Status refreshes with the Status refreshes with the normal three-second
discovery pass.

`/` opens temporary board search from any pane. Type a case-insensitive literal
substring of `project/run`; surrounding spaces are ignored. Search covers all
selectable rows within the current filters, including off-screen rows, captures,
completed items, chores and kept drafts. It preserves board order and groups:
`*` marks matches, and `>` marks the selected match. Up/Down step through
matching occurrences without wrapping. Enter accepts the selection and returns
to board navigation without taking an action; Esc or Ctrl-C cancels and restores
the previous selection, pane and scroll positions against the latest board. An
empty query includes all items without match marks. `No matches` clears the
preview; edit or cancel to recover. Query editing uses the single-line editor,
including word keys and literal paste (newlines become spaces). Printable
shortcuts remain text; Tab, page and save/discard keys have no search action.
Search covers labels, not titles or bodies. The footer shows the query, cursor,
position/count and hints. Ctrl-L redraws and Ctrl-Z suspends. In editors and the
command line, `/` remains text.

On a CHORES row, `e` (or `:edit`) opens the chore's definition in the text form
described under [Chore definitions](#chore-definitions) and `Ctrl-S` saves it,
refusing if the definition changed meanwhile. `:new-chore <project>/<name>`
opens a blank form and adds the chore on save. `:delete` asks, then removes the
definition. `o` (or `:open`) opens the chore's run now, ahead of its schedule,
and selects it; it starts nothing. Skip stays `moe chore skip`.

Closed ideas and intents remain selectable with their full capture text
scrollable in the details. Closed intents appear under `COMPLETED` like any
other finished work; inspection remains available after discarding a draft or
restarting the console. Closed captures accept no new edits.

Saved input appears in the selected run or idea's details: notes, open questions
and answered pairs. Notes and answered pairs are labelled `pending` or
`delivered to <stage>`. Focus details with Tab and scroll with arrows or page
keys. Saved history remains readable after delivery, console restart and
completion; completed items remain available for inspection only.

Close and discard confirmations show their question on a separate screen,
independent of console history. Press `y` to accept or `Esc` to cancel; arrows,
page keys, Home and End scroll long questions.

In a Rex terminal, UI reports live sweeps, outstanding operator questions,
control state and observation health through OSC 7501. Activity and questions
follow the board filters; health explicitly includes serve-wide failures. An
open session does not prove an agent is thinking, and a question does not stop
its run. Reports update only when their content changes and clear on orderly
exit. They require UI to keep running; restarting it clears its report subtree
and reseeds current state. Nested tmux/screen terminals and serve children do
not emit reports. These are current observations, not an event feed or a
complete account of stage failures.

The UI board carries no service rows. Cooling, failed or quota-deferred projects
and host/origin errors are logged once to the Console, with clock times, when
they appear or change; the banner's `N failing` count and push item are the
standing signal. For routine `sweeping` status and standing detail, use the web
`/serve` page.

## The Server (moe serve)

`moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]` runs the moe server:
the web UI, journal pushing, and an optional heartbeat. It binds to
`127.0.0.1:4242` by default; `--port` is ignored when `--addr` includes a port.
Beyond runs and canvases, its read-only surface browses lore, a projects index
with per-project hubs, project knowledge topics, twin documents, the token-spend
report at `/usage`, and a dashboard with the same daily-activity chart (and a
project-scoped one on each project page).

**The web starts nothing.** Every action it offers writes a journal commit and
stops there: capture or edit an idea or intent, tag an idea for a workflow,
close or reopen a run, mark the current stage advanced, answer a run's open
question. Starting agents is the heartbeat's job alone. No request executes code
— but agents read those writes in their prompts, and an armed serve's heartbeat
starts agents because of them: whoever can reach the listener steers the
machine. Reach is the only auth, so whatever sits in front of the listener is
the security boundary. Interactive stage-driving happens in a terminal.

Separately from all that, and armed or not, serve drains the bureaucracy's local
main to origin every couple of seconds; this does not ship a project's code
branch. Journal verbs commit and return; the push is serve's. With no serve
running, commits sit on local main until `moe sync` or the next serve start
takes them — `git status` in the bureaucracy says how many are waiting, and
serve's log carries any push failure.

`--dynamic` (or a non-empty `MOE_SERVE_DYNAMIC`) is the standing consent rung:
it starts the resident heartbeat, a ticker that runs one `moe pulse new
--dynamic` at a time when any project's board warrants it. Unarmed, the ticker
never fires and what the web wrote simply waits. Stopping the process retracts
it: no new sweep starts. [The heartbeat](workflows.md#the-heartbeat) describes
when it sweeps.

At armed startup, serve looks for a local Rex server. With a usable server,
serve runs its sweeps in a Rex session named `moe`, reached from the Rex client
like any other session; the first sweep creates it. Serve places no UI there:
run `moe ui` wherever you like. Session creation runs a harmless bootstrap;
serve records its identity atomically before launching a labelled sweep. The
sweep runs in that session as a block labelled `heartbeat:_cabinet`, after the
root project the pulse lives in. Such a sweep finishes even if serve stops or
crashes; an armed restart with a usable Rex host picks it up rather than
starting another, and Ctrl-C in its block is how to stop it. After reporting an
exit, serve renames the block to `done:heartbeat:_cabinet`; `done` acknowledges
reporting, including a failed notification attempt. Successful blocks and quota
deferrals close; failed screens stay readable until the next sweep. Restart
skips done blocks; the next sweep cleans them up before launching. A crash
between reporting and renaming can still duplicate a report, and failure
cooldown remains process-local. With no recorded Rex identity and no reachable
Rex server, sweeps run on serve's own terminals and stop with it. A recorded or
unreadable identity holds new sweeps through discovery failures, even if the CLI
is missing. Serve retries discovery and adoption on each heartbeat tick,
including idle ticks, preserving known identity until replacement is recorded.
Only a valid session listing proving absence permits replacement. Session
establishment and reconciliation must succeed before sweeps. An incompatible Rex
discovery reply instead requires fixing the host and restarting serve. The
startup log, `moe ui` and `/serve` name which host is in use.

A `/serve` page owns that ticker's status and trace — what each recent tick
decided, and the output tail of any sweep that failed — reachable from the menu
and from the status line of the dash's PULSE section.

`MOE_SERVE_NOTIFY_URL` is the other env var: set it and serve POSTs a small JSON
body to that URL when a heartbeat sweep succeeds or fails, naming the sweep
(`heartbeat:_cabinet`) and whether it succeeded. Pure quota deferrals send
nothing. A sweep serve interrupted on its way down also sends nothing: an
`ok:false` there would report a failure the operator caused. A failed POST is
logged and blocks nothing.

The heartbeat runs each sweep as `moe pulse new --dynamic` with its own marker,
and reads the exit status — which describes the whole invocation, including the
ride. `moe pulse new` exits non-zero when the sweep died, concluded nothing, or
the ride failed, which drives the failure backoff. A ride that parks at a
stage's gate (the stage's turn ran and committed, but its gate still holds) is
not a failure and does not drive it, nor is a CI repair whose turns ran cleanly
and found nothing to ship (no repair commit, or a nominated close). A clock
pulse with only quota deferrals exits 76: it neither increases the failure
streak nor clears previous failure history. Real failures take precedence in
mixed outcomes; interruption exits 130. See [Subscription quota
cooldown](#subscription-quota-cooldown) for the quota policy. The
heartbeat also passes `--emit-run <path>`: the sweep writes the run it opens
there, so /serve can link a sweep to the pulse run it minted rather than leaving
the operator to hunt the dash for a matching `pulse-*` slug. It also passes its
exact owned-PR observation through the internal `--pr-checks` argument, so a
successful sweep acknowledges what its survey saw. New actionable failures on
recorded pushed PRs can wake a sweep without a journal change, unless that
head's repair attempt is spent and no fresh note is waiting.

## Subscription Quota Cooldown

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

## Usage And Context Reports

`moe usage [--project <id>] [--since <dur>]` sums the token usage recorded in
every mirrored stage transcript, grouped by workflow, stage and model, with a
selected rolling-window total plus per-run and by-day breakdowns. It reads state
that already exists — no collection is turned on and nothing is written. The
`NOTIONAL` column prices those tokens at API sticker rates so workflows can be
compared in a common currency; under a subscription it is not a bill. Totals
containing unpriced models are starred, while a wholly unpriced total shows `—`.
`--since` takes `7d`/`24h`-style windows and defaults to `30d`; a whole stage is
attributed to its last work turn's commit time, falling back to run activity. A
transcript with no journal time remains in aggregate, rolling-window, and
per-run totals, is omitted from `BY DAY`, and is called out in the report. `moe
serve` renders the same report at `/usage`: the totals and the
workflow/stage/model table on top, a table of runs below linking to each run
page, both sortable by any column and scoped by the same `project` and `since`
filters.

`moe context-use [--project <id>] [--since <dur>] [--by-backend | --sessions]`
reads the same mirrored transcripts for which steering agents actually read:
twin docs, lore entries, the knowledge index and topics, intents, and other
runs. The default view leads with hygiene — lore entries and knowledge topics no
session in the window opened, and each twin doc's size against the codex result
cap — after a table of the share of sessions reaching each kind of context, per
backend, with the pulse apart. `--by-backend` splits that table by month and
lists named path misses; `--sessions` emits one JSON line per session for hand
review. A mention counts only when a read or search verb carries the path. A
truncated result still counts as a read and is tallied in the TRUNC column; a
not-found error naming the path is a miss, not a read. Lore is bureaucracy-wide,
so its counts cover every reader only without `--project`. `--since` defaults to
`90d`, the cadence of the `context-use-audit` chore that reads it.

## Codex Setup

The `codex` backend needs no setup in `~/.codex/config.toml`. MoE defines its
own sandbox permissions profile (`moe-workspace-git`) inline on every Codex
turn and selects it — read-only root, writable tmp, and writable `.` plus
`.git` on the working directory and each `--add-dir` root. The profile also
enables direct command networking on every interactive and headless turn, so
development tools such as `gh` can use the operator's existing credentials to
reach their services. Codex's stock `workspace-write` mode leaves `.git`
read-only, which fails a `git commit` inside the per-run clone; shipping the
profile with MoE keeps that from depending on operator config.

Separately, MoE pins `GIT_EDITOR=true` and `GIT_SEQUENCE_EDITOR=true` for every
Codex turn (interactive and headless): Codex never has a TTY for an editor, so a
Git operation that would open one — `git rebase --continue` finalizing a rebase,
`git commit` with no `-m` — otherwise hangs on vim and can leave a clone wedged
mid-rebase. Claude is unaffected: its commit flow is already non-interactive.

## Model Stylesheet

By default every stage turn runs whatever model the backend CLI defaults to.
A **model stylesheet** — one checked-in file at the bureaucracy root,
`model-stylesheet.css` — lets you bind a model (and, optionally, a backend) to
each `(workflow, stage)` declaratively, so "design and review get the strongest
model, everything else stays cheap" takes a few rules instead of a per-command
flag.

```css
/* Retain each backend's model when switching with --agent or MOE_FORCE_AGENT. */
* { agent: codex; model-codex: gpt-6.1-sol; }

sdlc.design { model-codex: gpt-6-astra; model-claude: fable; }
sdlc.review { model-codex: gpt-6-astra; model-claude: fable; }
```

The file is checked into the bureaucracy and rides the same auto-sync as the
rest of it, so every entry point sees the same rules. A missing file means no
rules — today's behaviour. A file that fails to parse **refuses the stage turn
loudly** with the parse error rather than silently ignoring your rules. So does
one that parses but names something MoE doesn't know: a selector for an
unregistered workflow or stage, a property MoE never reads, or an `agent:` value
with no registered backend all refuse at load — with the offending line and the
set of known names — so a typo that would otherwise match nothing forever
surfaces on the next turn. Model values are the one exception (see below).

This file is global across projects, so SDLC stages don't edit it directly. A
stage proposes a change by writing a full replacement at
`projects/<p>/runs/<id>/model-stylesheet.css`, which lands with that turn's
canvas. `moe sdlc push` validates the proposal before any origin work — a sheet
that wouldn't load refuses the push — and copies it over the root file in the
commit where the run ships: the ff-merge record, the PR record when the PR
opens, or the `ship: none` close. Until then every turn, the proposing run's
included, keeps using the root file. A run closed by hand never applies its
proposal; the run copy stays as the record of what was proposed.

When a proposal uses newly supported properties, deploy a MoE binary that
understands them before running push. Older binaries reject those properties
during proposal validation and when loading the root stylesheet.

**Grammar.** CSS-ish `selector { property: value; ... }` rules plus `/* ... */`
comments. Properties:

- `agent` — the backend name (`claude` | `codex`), resolved through the same
  registry `--agent` uses.
- `model-claude`, `model-codex` — a model for the named backend. Valid suffixes
  come from the registered backend names. These properties never select an
  agent; only the resolved backend's choice is used. The old backend-agnostic
  `model` property is gone; a sheet that still uses it refuses at load with its
  line number.

All model values are handed verbatim to the backend's `--model`. MoE keeps no
model catalog, so a bad id fails as the backend CLI's own error. Family aliases
(`fable`/`opus`/`sonnet` on claude) and un-dated ids (`gpt-5-codex` on codex)
float with releases; full ids (`claude-fable-5`) pin. Empty values are invalid;
there is no reset keyword.

**Selectors** have two axes — workflow and stage. Because a bare stage name
could be ambiguous across workflows (`build` is a stage of `sdlc`, `chat` of
`chat`), stage-only selectors take a leading dot:

| Selector      | Matches                     | Specificity |
| ------------- | --------------------------- | ----------- |
| `*`           | every stage turn            | 0           |
| `sdlc`        | every stage of one workflow | 1           |
| `.review`     | that stage in any workflow  | 1           |
| `sdlc.review` | exactly one workflow stage  | 2           |

Highest specificity wins per property; equal specificity breaks to the last
rule in the file. Every property cascades independently: a `model-codex` rule
does not clear an inherited `model-claude` or `agent`.

A selector naming a retired stage — sdlc's `code` or `test`, both replaced by
`build` — refuses at load and names the replacement. Rewrite the rule; it never
silently selects build, because the two retired stages' settings may have
differed.

**Precedence.** Explicit choices beat defaults. A temporary default overrides
stylesheet agent declarations at every selector specificity:

- Agent: `$MOE_FORCE_AGENT` → `--agent` flag → `run.json` agent → active
  **temporary default** → **stylesheet** → `claude`.
- Model, after resolving the agent: internal explicit model →
  **`model-<resolved-agent>`** → backend CLI default. There is no public
  `--model` flag or `$MOE_MODEL`; internal curation callers may supply an
  explicit model.

Steer a run with `--agent`, the whole process with `$MOE_FORCE_AGENT`, or the
permanent default with a stylesheet `agent:` rule. `moe agent codex` temporarily
changes defaults across this bureaucracy without writing a run binding or
changing model IDs. The tracked root `agent-default.json` records `revision`,
`agent` and UTC `expires_at`; clear records retain only the revision. Missing
means stylesheet. Malformed records refuse selection and show observation
errors. Web and terminal controls refuse stale revisions; duplicate requests
make no commit and do not extend expiry.

Expiry is checked on read, with no cleanup commit. Reset and expiry expose the
current stylesheet, including changes made during the window. Commit hand
edits to the root stylesheet so session worktrees receive them. Subsequent
turns, including cascades and heartbeat children, read fresh policy without a
serve restart; executing agents keep their admitted backend and model. A
locally known `MOE_FORCE_AGENT` is labelled for that process only.

With the example file above, `MOE_FORCE_AGENT=claude moe sdlc design moe/<run>`
uses `fable`, and the same command with `MOE_FORCE_AGENT=codex` uses
`gpt-6-astra`. Missing choices never borrow another backend's model.

## Shell Completion

`moe completion <shell>` prints a completion script for `bash`, `zsh`, or
`fish`. Source it from your shell's startup file:

```sh
# bash — in ~/.bashrc
eval "$(moe completion bash)"

# zsh — in ~/.zshrc, after `autoload -U compinit && compinit`
eval "$(moe completion zsh)"

# fish — in ~/.config/fish/config.fish
moe completion fish | source
```

Completion covers verbs and subcommands (`moe sd⇥` → `sdlc`, `moe sdlc ⇥` →
`design build review …`) and the `<project>/<run>` slug for run-taking verbs
(`moe sdlc build ⇥`), plus idea slugs (including `--from-idea`) and named
workspaces. The script itself never changes as commands are added — all the
logic lives in `moe` and is best-effort, so completion stays silent outside a
bureaucracy rather than erroring.

## Hooks And Environments

Project hooks live under `projects/<project>/hooks/<event>.d/*` in the
bureaucracy:

- `dev-env.d/*` emits `KEY=VALUE` lines that MoE caches and supplies to agent
  sessions, workspace shells, and pre-push scripts. A run that commits a change
  in this directory refreshes its cache before its next stage or real push;
  unrelated hook edits leave the run's cache warm.
- `dev-env-teardown.d/*` defines disposal of its own resources and rolls back
  failed setup using collected exports. Hooks must tolerate absent or partially
  cleaned resources and exit zero only when cleanup is complete. Per-run cleanup
  runs in lexical order from the original worktree, with saved addresses, a
  five-minute total deadline and bounded output/descendant draining. Terminal
  preservation holds prevent teardown; named workspace caches remain warm until
  explicit refresh or removal.
- `pre-push.d/*` is an invocation-time ship gate; a failing script halts the
  push path and opens a recovery build session.
- `session-cleanup.d/*` makes a leftover session directory removable when
  session GC can't delete it — typically files a container wrote as root
  through a bind mount. When an unregistered `.moe/worktrees/<uuid>/` refuses
  removal with a permission error, `moe session gc` and the serve heartbeat
  run the hooks of each project whose `projects/<project>/` survives inside
  it, with cwd and `MOE_SESSION_DIR` set to that directory, then retry the
  removal once. Scripts should only repair ownership or permissions under
  `MOE_SESSION_DIR`; MoE does the deletion. Each script has a two-minute
  deadline, and a failure leaves the directory in place, reported by path.

Use `moe hook fire <project> dev-env|dev-env-teardown|pre-push` to exercise one
event in a transient sandbox without creating a run. Each clone lives at
`.moe/hook-fire/<project>/hook-fire-<unique>/sandbox`. Subsequent fires clean
only wrappers marked complete after successful dispatch. Failed/interrupted
fires and legacy scratch under `.moe/clones/` are retained for manual
inspection; this cleanup never touches run clones.

MoE publishes the reusable dev-env cache only after every setup script succeeds.
On setup execution, output parsing, or fresh-cache publication failure, MoE
attempts teardown once if any exports were collected. It uses those in-memory
exports in normal lexical order, stopping at the first teardown failure. The
invocation still fails; a rollback failure is reported alongside the original
error. Rollback does not fall back to inherited `MOE_HOME` or `MOE_DEV_TMPDIR`
values when setup has not emitted them. A later invocation can start setup
afresh after successful rollback.

Setup hooks should emit resource addresses promptly. Teardown hooks must
tolerate absent resources and partial initialization, and use this attempt's
exports as ownership evidence rather than unrelated inherited variables or
credentials. Hooks own cleanup of allocations they never emitted. MoE cannot
recover exports after a scanner error, overwritten addresses, or resources left
by missing or failing teardown hooks; it does not retain a durable partial-setup
journal or recover from process death.

Hooks, chore definitions, knowledge topics, and twin documents are
bureaucracy-side artifacts: edit them by hand, or let an sdlc run land them. An
sdlc stage's per-turn commit picks up `projects/<project>/hooks`, `chores`,
`knowledge`, and `digital-twin`, so `moe sdlc design → build → close` is the
journaled route with no push involved.

### Chore definitions

A chore lives at `projects/<project>/chores/<name>/`, holding a `chore.json` of
scheduler scalars and a `prompt.md` seed. All `chore.json` keys are optional;
durations are strings like `"720h"` / `"30d"`:

- `trigger` — path glob, or `*` for any merged project change.
- `workflow` — workflow to open; defaults to `sdlc`.
- `cooldown` — minimum duration between completed chore runs.
- `cadence` — stale-by-time duration.
- `when` — a one-line prose due-condition the pulse survey judges against what
  landed. Exclusive with `trigger` and `cadence`: a chore is due mechanically or
  by judgment, not both. `cooldown` still applies.

Reach for `when` when the chore is due only if a judgment holds ("a landed
change made this artifact lie"); `"trigger": "*"` plus a cooldown degrades into
a weekly timer. Keep the criterion to one line — one that needs paragraphs is
too vague to judge.

`prompt.md` is the seed for the opened workflow's first canvas: a markdown
sibling read verbatim, not folded into `chore.json`. Use `moe chore check` as
the dry-run loop rather than opening a run to test a definition.

Three surfaces write definitions through one writer, each change one commit
(`Add|Edit|Remove chore <project>/<name>`) touching only the chore's
directory:

```
moe chore new  <project>/<name> [--trigger G] [--cadence D] [--cooldown D] [--when TEXT] [--workflow W]
moe chore edit <project>/<name> [same flags]
moe chore rm   <project>/<name>
```

`new` opens `$EDITOR` on the text form below, flags prefilled. `edit` with flags
sets exactly those fields (an empty value clears one) and opens nothing; with
none it opens the editor. A save that fails to parse or validate keeps the
edited text in a temp file and prints its path. `rm` removes the directory; git
is the undo. The web chore page offers edit (delete is on the edit page) and the
project page a `new chore` link; `moe ui` offers the same on a CHORES row. An
unchanged edit of an existing definition commits nothing; saving a valid new
form creates the chore even if its prefilled text is unchanged. A real edit
makes a mechanical chore due ("definition changed") unless it is cooling down.
Rename is `rm` then `new`; a recreated name inherits its history.

The text form, used by `$EDITOR` and `moe ui` (never on disk), is five
`key: value` lines, each shown even when blank, then a line that is exactly
`---`, then the prompt verbatim:

```
trigger:
cadence: 90d
cooldown: 90d
when:
workflow:
---
# context-use-audit

Prompt text, verbatim to the end of the buffer.
```

The web chore page and `moe ui` can also open a chore's run now: due-ness and
cooldown are waived, an already-open run is not. It writes the open commit and
starts no agent; a later eligible armed sweep or an explicit terminal stage
command starts work. See [The heartbeat](workflows.md#the-heartbeat) for the
scheduling guards.

### Knowledge topics

`projects/<project>/knowledge/` is a plain doc tree: `index.md` at the top as
the catalog, one file per topic flat under `topics/`. `moe serve` browses it
read-only. Any sdlc stage may write it, and the stage refuses to close if the
turn's edits left the tree structurally broken — a topic missing from
`index.md`, a broken relative link, an empty doc. Hand edits bypass the gate;
it polices agent writers.

### Per-project dev secrets

Dev and test runs often need secrets (API keys, DB URLs, tokens) that must never
be committed and must not leak across projects. The `dev-env.d` hook is the
seam, no new subsystem required. A script decrypts a per-project file and emits
its `KEY=VALUE` lines; MoE caches them at the tree's gitignored
`.moe/dev-env.env` and sources them into the agent session and `moe workspace
shell`. Decryption runs operator-side at stage open, before the agent subprocess
exists, so the agent receives only the decrypted vars for its own project and
never reads the key. Per-project scoping is structural: only that project's
`dev-env.d` runs for its trees. The cache header is a comment, so sourcing the
file still exports only the hook's `KEY=VALUE` body.

Store the ciphertext as a sibling of the hook dir,
`projects/<project>/secrets.env.age`, encrypted with
[age](https://github.com/FiloSottile/age):

```sh
age-keygen -o /<volume>/age/keys.txt                             # one-time: prints age1... pubkey
age -r <pubkey> -o projects/<p>/secrets.env.age secrets.env  # encrypt, then git add the .age
```

```sh
# projects/<p>/hooks/dev-env.d/50-secrets.sh
age -d -i /<volume>/age/keys.txt \
  "$MOE_BUREAUCRACY/projects/$MOE_PROJECT/secrets.env.age"
# stdout: KEY=VALUE lines -> MoE caches and sources them
```

age decrypts with no passphrase, so the same hook survives the headless `!!`
cascade, which has no operator to answer a prompt. Keep the keyfile outside the
bureaucracy (e.g. on a persistent volume, with the secret line backed up in a
password manager); a leaked bureaucracy clone is then ciphertext only. A run
that commits the hook change re-decrypts before its next stage or push. Changes
to ciphertext or other inputs are not cache invalidators; use `moe workspace
refresh` for a named workspace, or reopen with a fresh per-run tree. If a
framework insists on reading a `.env` off disk, redirect the same `age -d`
output to `"$MOE_SANDBOX/.env"` instead — but only when the target repo already
gitignores that file, since `pre-push` refuses to ship with any untracked file
present.

## Session Checkout

New bureaucracy sessions use sparse checkout: other runs' mirrored transcripts
and saved prompts under `documents/` and `diagnostics/`, plus raw execution
streams under `diagnostics/`, stay in Git without being copied to disk. The
entire active run and normal project context remain present. Existing full
sessions resume unchanged. This bounds archive checkout cost without changing
auto-pull, session locking, or the complete commit tree.

`moe <workflow> log`, usage reports, and serve transcript pages read omitted
transcripts from the session's index without restoring files; present files
still win. To inspect a committed prompt snapshot, run
`git show HEAD:projects/<p>/runs/<run>/documents/<stage>/prompt-<agent>.md`
from the session worktree (or use the corresponding diagnostic path).

## Cleanup And Recovery

- `moe session list|abandon|resolve|gc` inspects or cleans leftover stage
  session worktrees and branches.
- `moe clone list` lists per-run sandbox clones.
- `moe clone gc [--apply] [--json] [<project>/<run>]` inspects reclamation;
  `--apply` performs eligible cleanup. The optional filter is an exact run,
  never a path or glob. Holds and an empty inventory exit 0; inventory, probe or
  application failures exit 1; argument errors exit 2. JSON includes a schema
  version, invocation ID, physical generation, terminal transition, deadline,
  outcome, reason and retained receipt path. Hook output goes to stderr in JSON
  mode.
- `moe clone recover <project>/<run>` is interactive host recovery for one
  stopped run's legacy enrollment, shell lifetime or resource cleanup debt.
- `moe workspace release` clears a stale named-workspace claim.
- `MOE_GIT_TRACE=1` (strict `=1`) prints one line per git invocation —
  dir, argv, duration, error — to stderr. First tool to reach for when a
  stage open or sync stalls inside git.

When a turn returns with a missing or empty canvas, it still fails: MoE saves
its prompt, available transcript, and execution result in an immutable
`diagnostics/<stage>/<attempt-id>/` directory under the run on main. Codex
headless attempts also save raw `stdout-codex.jsonl` and `stderr-codex.log`,
with capture errors recorded in `failure.json`. Raw streams are captured to
private scratch files while live progress and stderr continue; scratch files are
removed after successful turns or archival. These diagnostic commits never
satisfy a stage. The log command shows the latest saved failure until a later
normal turn supersedes it, using journal order rather than timestamps. Older
attempts remain in Git after retries and session cleanup. If archival fails,
stderr names the surviving worktree and raw capture directory holding the
evidence. Other unsuccessful turns retain their raw capture directory and print
its path.

On Linux, GC inventories both `.moe/clones/` and `.moe/clone-reclamation/`.
Entries with invalid project or run names, such as `.claude`, are skipped and
left untouched, including with `--apply`; invalid project entries are never
traversed. Each skipped entry emits a stderr warning with its quoted full path
and whether its project or run ID was invalid. Warnings stay on stderr in text
and JSON modes; skipped entries are not JSON candidates. Skipped names alone do
not fail the command: an inventory containing only skipped names succeeds with
no candidates. Directory access/read failures and inventory-limit failures
still stop the entire scan before any candidates are processed, exiting 1;
JSON reports `inventory_failed`. Warnings already emitted remain visible.
Valid names, including unfamiliar or unregistered entries, still undergo the
existing retention, ownership and deletion checks.

A session branch reserves its entire run across documents and backends. Close
and advance refuse while any such branch remains, including interrupted or
orphaned sessions. Another run is unaffected. Quota waiting holds a reservation;
a refused quota admission removes only a fresh untouched reservation and retains
resumed work with its claim.

Run ownership is separate from the session reservation. The invocation that
opens a stage locks `.moe/run-owners/<project>/<run>.lock` until its post-turn
prompt and the continuation chosen there return. Kicks skip an owned run; other
invocations' stage verbs, close, advance and push refuse it, and sync defers it.
Ordinary process exit releases the lock, which never overrides a surviving
session branch. A run shell, including entry through a claimed named workspace,
owns it exclusively. Its Linux supervisor retains the guard through background,
detached and adopted descendants and preserves the shell's exit status. An
unfinished durable start blocks stages, mutation and collection even when the
lock is free. Shells publish `shell running` and `waiting for shell jobs`;
exiting a shell starts no workflow continuation.

Nonterminal runs, including pushed and interrupted runs, remain indefinitely.
A proven merged transition committed in the canonical journal makes the private
run clone immediately disposable, including tracked edits, untracked or ignored
output, stashes and unique local Git history. Cleanup requires no content
preservation proof or content fingerprints for that case; GC reports
`merged_disposable`, even after the retention deadline. A remote push alone
supplies no disposal authority.

For closed and promoted runs, the latest committed terminal transition starts a
fixed 168-hour interval. Before its deadline, cleanup requires clean local
contents and independent canonical retention of every available Git object,
including reflog-only and unreachable objects. Clean Git status alone does not
suffice: actual tracked bytes must match retained blobs without content filters.
Converted line endings or expanded ident strings can therefore hold cleanup
until expiry. A preservation hold also prevents teardown. At the deadline,
clone-local leftovers may be discarded. Live rebind always requires
preservation. All disposal retains lifetime, generation, session, resource and
path checks, including structural inventory bounds and refusal of nested
repositories, mounts and special files. Missing or unprovable journal history
holds. GC never closes or abandons a run.

Per-run setup retains original cache bytes and ordered hook digests outside the
clone. Terminal status commits before cleanup, so a failed close commit leaves
its environment intact and a failed teardown cannot undo a completed merge.
Known nonzero teardown completion can be retried with exact-run
`moe clone gc --apply <project>/<run>`; interrupted workers need stopped-use
recovery first. Successful teardown is recorded before clearing its cache; a
cache-clear retry never reruns hooks. Recovery can repair active refresh debt
without closing the run. Named workspaces keep their existing warm lifecycle.

Deletion uses verified Linux descriptor/mount checks and an authorized private
quarantine tree. Interrupted removal stays discoverable even without a live
clone path, and pending removal blocks recreation. Recovery receipts under
`.moe/clone-reclamation/` and named-shell lifetime records under
`.moe/run-shells/` remain permanently. Unsupported platforms hold unattended
deletion; there is no force option, recursive pathname fallback, Docker cleanup
or permission repair.

The first Ctrl-C stops forward work and unwinds through commit and cleanup,
then releases ownership and exits 130 without cascading. A second Ctrl-C may
force exit. Rerunning the same stage can immediately resume a matching released
claim. A crash without release still requires proof that the previous owner is
gone: same host and PID namespace, dead process, and a heartbeat older than ten
minutes. Fresh crashes wait; live unreleased, missing, unreadable, mismatched
or foreign claims refuse takeover. Do not delete claims to bypass these checks.

Session GC distinguishes absent claims from unreadable ones. Its orphan rules
for registered sessions and worktree-less branches still permit claimless
cleanup. A present claim holds GC unless it names that exact branch and proves
its owner dead; malformed JSON, unreadable files and unexpected file types hold
too. A released claim still needs the same death proof. Held sessions are
silently skipped by both `moe session gc` and the heartbeat. `moe session list`
shows all local session branches and registered session worktrees, including
worktrees outside the managed directory. A branch without a registered worktree
shows `(no registered worktree)` in the path column. A reported path describes
Git's registration, not whether the directory is present or readable. Listing
does not establish owner death or permission to discard work. Unlanded branches
without a worktree need one restored before `moe session resolve` can land them.

An interrupted close records its validated tip before merging. Retry finishes
cleanup if that exact tip already landed, even if main has advanced. For older
sessions without a close receipt, `moe session resolve <branch>` can explicitly
clean up a branch whose entire tip is contained in main. It starts no agent or
next stage. Readable trees with additional tracked edits refuse cleanup;
unreadable residual trees are retained at a printed path under
`.moe/session-recovery/` before cleanup continues. Those backups are never
automatically collected. Inspect and preserve unfinished work, then rerun its
stage or resolve it. Use `moe session abandon <branch>` only after stopping the
owner and deciding to discard the session.

Coordination covers processes sharing one bureaucracy root and its repository
lock; independent clones do not share a distributed lease.

If a stage reports `session: incomplete setup`, its session checkout never
finished and cannot resume. After addressing the underlying setup failure, run
`moe session abandon <branch>` with the exact branch printed in the error, then
re-run the original stage command. Abandon discards that incomplete session's
worktree and branch.

Each document keeps its latest session per backend. On Claude, switching
backends and returning resumes that conversation, and changing its model keeps
it. Codex starts a new conversation on every launch: it fixes a session's
instructions when the session is created, so a resumed stage would run on the
first turn's guidance. The canvas carries the work across Codex launches and
between backends. Headless turns start fresh on both backends and replace only
their backend's latest session ID.

Stage logic can recover orphaned Claude sessions from the Claude cache or an
identity-matching mirror. Missing history starts a fresh conversation with a
notice. Malformed or conflicting mirror identity stops the turn before launch
when recovery needs that mirror.
### After An Upgrade

These notes apply only to state or processes left by an older MoE binary.

`moe sdlc code` and `moe sdlc test` are retired: they print the build command
and start nothing. A run started before build existed keeps its code and test
canvases readable (`moe sdlc cat`, `moe sdlc log`, the web run page); its first
build turn seeds `content/code.md` and `content/test.md` from them and must
restamp both before the run can reach review.

New clones enroll under ownership before setup or agent execution. After an
upgrade, a normal stage command can encounter an existing unenrolled clone and
refuse entry before setup or agent execution. Stop all old MoE binaries, shells,
descendants and unmanaged users of that clone and its resources, then run
`moe clone recover <project>/<run>` from an interactive host terminal. Recovery
inspects and validates enrollment; malformed records, changed identities or
independent debts may still hold. After successful recovery, rerun the original
stage command. Recovery never deletes a clone or waives its retention checks.
Changed teardown scripts need a separate approval of their exact new digests
against the original resource snapshot; missing hooks must be restored.
Mount-namespace changes require explicit host revalidation. Do not enable
unattended apply until old binaries and unmanaged use have been drained.

Older runs' scalar session IDs are adopted lazily using the selected backend's
native cache or mirror identity. The legacy value stays intact while new turns
write backend-keyed IDs in run.json. Running an older MoE binary after this
metadata migration is unsupported: it cannot preserve the new session map.

Older MoE binaries do not take the run-owner lock. Stop active invocations of
older MoE binaries when adopting this admission policy.
