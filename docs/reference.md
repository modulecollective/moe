# Reference

Look-things-up material: the command catalog, backend setup, shell completion,
hooks and environments, and cleanup. `moe help` and per-command usage are the
source of truth for the exact command surface; this page is a map.

## Command Catalog

### Re-Entry And Supervision

- `moe dash [--all] [--project <id>] [--workflow <name>] [--watch]` prints the
  terminal dashboard, with due project chores at the head of BACKLOG and a
  daily-activity histogram of recent run activity (`--project` scopes the chart
  to one project). `--watch` redraws it in place every 3 seconds until Ctrl-C —
  a live pane rather than a snapshot; it needs a terminal on stdout and exits 2
  without one. When a `moe serve` is running against this bureaucracy, its
  status rides the banner's tail — armed or not, uptime, next sweep — and a
  project earns a line of its own only when it is sweeping, cooling off, or its
  last sweep died. The tail also carries the drain to origin: `N unpushed` when
  commits are waiting (read from git, so it shows with or without a serve), or
  `pushed 2m ago` once serve's pusher has landed one. A push origin is refusing
  earns a row of its own with git's reason.

  In a Rex terminal, `--watch` also reports live sweeps, outstanding operator
  questions and known serve failures through OSC 7501. Activity and questions
  follow the board filters; health explicitly includes serve-wide failures.
  An open session does not prove an agent is thinking, and a question does not
  stop its run. Reports update only when their content changes and clear on
  orderly exit. They require the watch pane to keep running; restarting it
  reseeds current state. Nested tmux/screen terminals and serve children do
  not emit reports. These are current observations, not an event feed or a
  complete account of stage failures.
- `moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]` runs the moe
  server: the web UI, journal pushing, and an optional heartbeat. It binds to
  `127.0.0.1:4242` by default; `--port` is ignored when `--addr` includes a
  port. Beyond runs and canvases, its read-only surface browses lore, a
  projects index with per-project hubs, project knowledge topics, twin
  documents, the token-spend report at `/usage`, and a dashboard with the same
  daily-activity chart (and a project-scoped one on each project page). **The
  web starts nothing.** Every action it offers writes a journal commit and
  stops there: capture or edit an idea or intent, tag an idea for a workflow,
  close or reopen a run, mark the current stage advanced, answer a run's open
  question. Starting agents is the heartbeat's job alone. No request executes
  code — but agents read those writes in their prompts, and an armed serve's
  heartbeat starts agents because of them: whoever can reach the listener
  steers the machine. Reach is the only auth, so whatever sits in front of the
  listener is the security boundary. Interactive stage-driving happens in a
  terminal. Separately from all that, and armed or not, serve drains the
  bureaucracy's local main to origin every couple of seconds; this does not
  ship a project's code branch. Journal verbs commit and return; the push is
  serve's. With no serve running, commits sit on local main until `moe sync` or
  the next serve start takes them — `git status` in the bureaucracy says how
  many are waiting, and serve's log carries any push failure. `--dynamic` (or a
  non-empty `MOE_SERVE_DYNAMIC`) is the standing consent rung: it starts the
  resident heartbeat, a per-project ticker that runs `moe pulse new --dynamic`
  when the board warrants it. Unarmed, the ticker never fires and what the web
  wrote simply waits. Stopping the process retracts it: no new sweep starts. At
  armed startup, serve looks for a local Rex server. With a usable server,
  serve opens a Rex session named `moe`, reached from the Rex client like any
  other session, whose `dash` block runs `moe dash --watch`; each armed start
  replaces that block with a fresh one. Each sweep runs in that session as a
  block labelled `heartbeat:<project>`. Such a sweep finishes even if serve
  stops or crashes; an armed restart with a usable Rex host picks it up rather
  than starting another, and Ctrl-C in its block is how to stop it. A failed
  sweep's block stays open for reading until that project's next sweep. With no
  Rex server, sweeps run on serve's own terminals and stop with it. If startup
  cannot read a recorded session's jobs, serve keeps its identity and holds all
  new sweeps, retrying adoption on each heartbeat tick, even when no project is
  due. Only a valid session listing proving absence permits replacement. The
  dash opens after reconciliation; a dash failure alone does not hold sweeps.
  An incompatible Rex discovery reply instead requires fixing the host and
  restarting serve. The startup log, `moe dash` and `/serve` name which host is
  in use. A `/serve` page owns that ticker's status and trace — what each recent
  tick decided, and the output tail of any sweep that failed — reachable from
  the menu and from the one-line status cluster every board header carries.
  `MOE_SERVE_NOTIFY_URL` is the other env var: set it and serve POSTs a small
  JSON body to that URL each time a heartbeat sweep exits on its own, naming
  which project's sweep it was and whether it succeeded. A sweep serve
  interrupted on its way down sends nothing: an `ok:false` there would report a
  failure the operator caused. A failed POST is logged and blocks nothing.
- `moe project mode <id> [paused|safe|auto]` caps what the heartbeat may start
  in one project, without stopping the process. See below.
- `moe chore list|check|open|skip` lists due project chores, dry-runs a chore
  definition, opens the run a due chore configures, or clears a due chore until
  it is next triggered.
- `moe input add <project>/<run> [text ...]` pushes a chunk of prose at a run;
  with no text arguments it reads stdin, so a paragraph can be piped. The run's
  next *successful* agent turn receives it verbatim in its prompt, once, and
  then it lives on the run page as history — the canvas that turn writes is
  where durable direction persists. `moe input answer <project>/<run>
  [text ...]` fills the question a dynamic pulse asked on that run (one open
  per run, so the run is the address). `moe input list [project]` shows both
  halves, oldest first: what has asked you, then what you have given and no
  turn has picked up.

  Nothing here holds anything. An unanswered question does not stop the run —
  its turns are told it was asked and no reply came, and they proceed on their
  best judgment. Stillness stays a pulse's `park`. Writing an entry starts
  nothing either: it makes the prose available to the next agent turn and can
  make eligible work actionable for an armed serve's sweep. Project mode,
  readiness, and occupancy still apply; a hand-typed
  `moe pulse new --dynamic <project>` is the expedite path. A pending note or
  answer counts as an operator mark under `safe` mode, consumed on delivery.
- `moe usage [--project <id>] [--since <dur>]` sums the token usage recorded in
  every mirrored stage transcript, grouped by workflow, stage and model, with a
  selected rolling-window total plus per-run and by-day breakdowns. It reads
  state that already exists — no collection is turned on and nothing is
  written. The `NOTIONAL` column prices those tokens at API sticker rates so
  workflows can be compared in a common currency; under a subscription it is
  not a bill. Totals containing unpriced models are starred, while a wholly
  unpriced total shows `—`. `--since` takes `7d`/`24h`-style windows and
  defaults to `30d`; a whole stage is attributed to its last work turn's commit
  time, falling back to run activity. A transcript with no journal time remains
  in aggregate, rolling-window, and per-run totals, is omitted from `BY DAY`,
  and is called out in the report. `moe serve` renders the same report at
  `/usage`: the totals and the workflow/stage/model table on top, a table of
  runs below linking to each run page, both sortable by any column and
  scoped by the same `project` and `since` filters.
- `moe where` prints the resolved bureaucracy path.
- `moe version` prints the moe version.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context. Both `cat` and `log` accept `@latest` in the
  `<run>` slot to mean the workflow's most-recent run.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy in the explicit
  directory, otherwise `$MOE_HOME` if set, otherwise the current directory.
- `moe project add <repo-url>` registers a target project.
- `moe project list` lists registered projects with their mode and ship route.
- `moe project mode <id> [paused|safe|auto]` reads or sets a project's mode —
  the standing cap on what the heartbeat may start there:
  - `paused` — the heartbeat never sweeps the project. No agent turn is spent
    on it at all.
  - `safe` — it sweeps and grooms as ever (survey, park, chore nomination), but
    the kick starts only threads carrying an explicit operator mark: a valid
    advance marker, a chore's standing intent, a workflow tag on the idea a run
    was promoted from, or an undelivered note on any run in the thread — a
    `moe input add` note or an answered question, consumed once a turn delivers
    it. Everything else is held with a named reason.
  - `auto` — the default, and today's behaviour: every kickable parked thread
    the survey doesn't park gets started.

  The mode binds the clock, not you. `!`, `!!`, `!!!`, `moe chain kick`, stage
  verbs and a hand-typed `moe pulse new --dynamic` all run in every mode — the
  typed word is consent whatever the config says. Serve's advance mark is the
  same shape from the other direction: it writes an operator mark, which is
  exactly what `safe` looks for before it starts a thread.
  Serve's arming stays above all three: an unarmed serve starts no agent
  anywhere. The mode is stored in `projects/<id>/project.json` (absent means
  `auto`) and is settable from the project hub's switch as well.
- `moe project ship <id> [pr|merge]` reads or sets how a finished run lands in
  the project:
  - `pr` — the default: push `moe/<run>`, open (or re-use) a PR, mark the run
    `pushed`, and keep the sandbox until the PR merges. `moe sync`'s reconcile
    flips it to merged or closed on its own.
  - `merge` — fast-forward the target repo's default branch, delete the remote
    branch, drop the sandbox, mark the run `merged`.

  The setting binds every ship that carries no route flag: bare
  `moe sdlc push`, `!!` / `!!!` at any gate, `moe chain kick`, and the
  heartbeat's rides. `--pr` and `--merge` on push, and `m` / `p` at the chain
  prompt, override it for one invocation; giving both flags is a usage error.
  Stored in `projects/<id>/project.json` (absent means `pr`) and settable from
  the project hub's switch as well.

  Unlike mode, adopting this changes behaviour: everything unflagged used to
  merge. A project that should keep merging needs `moe project ship <id> merge`
  once. The `pr` default is deliberate — a PR can be closed unmerged, a merge
  has landed.
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe chain new [--seed] <project>/<slug>` mints a chain run: a stageless
  placeholder head that collects a batch while parked, then closes when kicked
  before the ride continues through its members. `--seed` pops `$EDITOR` on its
  purpose note first.
- `moe chain edit` opens an editor over active operator-cascade runs (SDLC)
  plus chain heads; reorder lines to record a run chain in the bureaucracy
  journal.
- `moe chain note <project>/<run>` edits a head's purpose note: why the batch
  exists. Membership isn't written there — it renders live from the edges.
- `moe chain kick <project>/<run>` rides a chain headlessly from the named head,
  resuming each run from its first pending stage through shipping. What is
  chained now is what runs: a kick sweeps nothing, so nothing can grow the batch
  under it.
- `moe chain close [--no-edit] <project>/<run>` drops a head without riding it.
- `moe chain clear [--yes]` drops every currently live run-chain edge.
- `moe <workflow> close [--no-edit] <project>/<run>` closes a run; for `sdlc` it
  abandons the run instead of shipping it through `sdlc push`. `--no-edit` skips
  the followups and feedback editors, harvesting the files as-is. Capture
  workflows (`idea` and `intent`) skip harvesting and show no `--no-edit`
  option.

### Workflows

- `moe sdlc new|design|code|test|review|push|close|harvest|shell|reopen|cat|log`
  drives designed code work.
- `moe chat new|chat|close|harvest|cat|log` drives thinking-partner sessions.
- `moe idea new|edit|close|list|move|tag|untag|reopen|cat|log|harvest` manages
  backlog notes. `tag` stamps the workflow tag that licenses a pulse to start
  the idea (`untag` clears it), and `tag --design-only` narrows that licence to
  one design turn before the run holds; untagged ideas are operator-only.
- `moe intent new|edit|close|list|cat|log|harvest` manages operator-authored
  standing direction.
- `moe pulse new|close|cat|log` runs and inspects project sweeps and optional
  work dispatch. The survey is read-only; opening due chore runs does not
  execute their stages. `moe pulse new --dynamic <project>` starts eligible
  parked threads and nominated CI repairs on existing pushed PRs after the
  survey. Without `--dynamic`, curated work stays parked. The heartbeat inside
  `moe serve --dynamic` calls this with its own marker, and reads the exit
  status — which describes the whole invocation, including the rides the sweep
  started. `moe pulse new` exits non-zero when the sweep died, concluded
  nothing, or a kicked ride stalled, which drives the failure backoff. A skip
  exits 130, not a failure. It also passes `--emit-run <path>`: the sweep writes
  the run it opens there, so /serve can link a sweep to the pulse run it minted
  rather than leaving the operator to hunt the dash for a matching `pulse-*`
  slug. It also passes its exact owned-PR observation through the internal
  `--pr-checks` argument, so a successful sweep acknowledges what its survey
  saw. New actionable failures on recorded pushed PRs can wake a sweep without a
  journal change. A survey may nominate same-run CI repair with
  `{"run":"<slug>","repair":"ci"}` in a singleton thread group. Dynamic consent
  and safe-mode operator marks still apply; a fresh run note licenses retry
  after the one-attempt-per-head journal claim is spent.

`moe <workflow> harvest [--no-edit] <project>/<run>` re-runs a run's
`followups.md`, `feedback/lore.md`, and `feedback/twin.md` harvests — into
ideas, `lore/`, and twin-edit ideas respectively — without closing it. It picks
up captures regenerated after the run was already closed, and it is the
recovery verb for a conversational session whose own session-end harvest
failed partway.

Conversational sessions (`moe idea edit --chat`, `moe intent edit --chat`,
`moe chat`) harvest at session end rather than at close: the capture
workflows' close deliberately skips harvest and a chat run is perpetual, so
session end is the only harvest point they reliably reach. That pass is
silent — the operator was in the conversation, so there is no editor review.
Run the verb above for the reviewing form.

## Codex Setup

The `codex` backend needs no setup in `~/.codex/config.toml`. MoE defines its
own sandbox permissions profile (`moe-workspace-git`) inline on every Codex
turn and selects it — read-only root, writable tmp, and writable `.` plus
`.git` on the working directory and each `--add-dir` root. The profile also
enables direct command networking on every interactive, resumed, and headless
turn, so development tools such as `gh` can use the operator's existing
credentials to reach their services. Codex's stock `workspace-write` mode
leaves `.git` read-only, which fails a `git commit` inside the per-run clone;
shipping the profile with MoE keeps that from depending on operator config.

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
  agent; only the resolved backend's choice is used.
- `model` — the legacy fallback, scoped by a paired `agent:` as described below.

All model values are handed verbatim to the backend's `--model`. MoE keeps no
model catalog, so a bad id fails as the backend CLI's own error. Family aliases
(`fable`/`opus`/`sonnet` on claude) and un-dated ids (`gpt-5-codex` on codex)
float with releases; full ids (`claude-fable-5`) pin. Empty values are invalid;
there is no reset keyword.

**Selectors** have two axes — workflow and stage. Because a bare stage name
could be ambiguous across workflows (`code` is a stage of `sdlc`, `chat` of
`chat`), stage-only selectors take a leading dot:

| Selector      | Matches                     | Specificity |
| ------------- | --------------------------- | ----------- |
| `*`           | every stage turn            | 0           |
| `sdlc`        | every stage of one workflow | 1           |
| `.review`     | that stage in any workflow  | 1           |
| `sdlc.review` | exactly one workflow stage  | 2           |

Highest specificity wins per property; equal specificity breaks to the last
rule in the file. Every property cascades independently: a `model-codex` rule
does not clear an inherited `model-claude`, `model`, or `agent`.

**Precedence.** The stylesheet sits below your explicit per-run bindings and
above the background defaults, mirroring "explicit beats the stylesheet":

- Agent: `$MOE_FORCE_AGENT` → `--agent` flag → `run.json` agent → **stylesheet**
  → `claude`.
- Model, after resolving the agent: internal explicit model →
  **`model-<resolved-agent>`** → legacy **`model`** with its pairing rule →
  backend CLI default. There is no public `--model` flag or `$MOE_MODEL`;
  internal curation callers may supply an explicit model.

Steer a run with `--agent`, the whole process with `$MOE_FORCE_AGENT`, or the
ambient default with a broad stylesheet `agent:` rule.

The backend-specific property wins over legacy `model` regardless of their
relative selector specificity. For example, `* { model-codex: base; }` beats
`sdlc.design { model: special; }` on a Codex turn. Specificity compares
declarations of the same property; precedence chooses between properties. With
the example file above, `MOE_FORCE_AGENT=claude moe sdlc design moe/<run>` uses
`fable`, and the same command with `MOE_FORCE_AGENT=codex` uses `gpt-6-astra`.
Missing choices never borrow another backend's model.

When no explicit or backend-specific model applies, a legacy `model:` paired
with an `agent:` rides only when the turn's backend matches the stylesheet's
resolved `agent` for that slot. Pairing uses the winning property after the
cascade, even across separate rules. If an override selects a different
backend, the legacy model is dropped with a one-line stderr notice and the
backend default applies. An unpaired legacy model is handed verbatim to
whichever backend runs. A backend-specific choice emits no drop notice because
the legacy fallback is unused. Existing files keep their behavior.

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
`design code test review …`) and the `<project>/<run>` slug for run-taking verbs
(`moe sdlc code ⇥`), plus idea slugs (including `--from-idea`) and named
workspaces. The script itself never changes as commands are added — all the
logic lives in `moe` and is best-effort, so completion stays silent outside a
bureaucracy rather than erroring.

## Hooks And Environments

Project hooks live under `projects/<project>/hooks/<event>.d/*` in the
bureaucracy:

- `dev-env.d/*` emits `KEY=VALUE` lines that MoE caches and supplies to agent
  sessions and workspace shells. A run that commits a change in this directory
  refreshes its cache before its next stage or real push; unrelated hook edits
  leave the run's cache warm.
- `dev-env-teardown.d/*` cleans up when a run or workspace closes.
- `pre-push.d/*` is an invocation-time ship gate; a failing script halts the
  push path and opens a recovery code session.

Use `moe hook fire <project> dev-env|dev-env-teardown|pre-push` to exercise one
event in a transient sandbox without creating a run.

Hooks, chore definitions, knowledge topics, and twin documents are
bureaucracy-side artifacts: edit them by hand, or let an sdlc run land them. An
sdlc stage's per-turn commit picks up `projects/<project>/hooks`, `chores`,
`knowledge`, and `digital-twin`, so `moe sdlc design → code → close` is the
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

### Knowledge topics

`projects/<project>/knowledge/` is a plain doc tree: `index.md` at the top as
the catalog, one file per topic flat under `topics/`. `moe serve` browses it
read-only. Any sdlc stage may write it, and the stage refuses to close if the
turn's edits left the tree structurally broken — a topic missing from
`index.md`, a broken relative link, an empty doc. Hand edits bypass the gate;
it polices agent writers.

### Per-project dev secrets

Dev and test runs often need secrets (API keys, DB URLs, tokens) that must never
be committed and must not leak across projects. The `dev-env.d` hook is the seam,
no new subsystem required. A script decrypts a per-project file and emits its
`KEY=VALUE` lines; MoE caches them at the tree's gitignored `.moe/dev-env.env`
and sources them into the agent session and `moe workspace shell`. Decryption
runs operator-side at stage open, before the agent subprocess exists, so the
agent receives only the decrypted vars for its own project and never reads the
key. Per-project scoping is structural: only that project's `dev-env.d` runs for
its trees. The cache header is a comment, so sourcing the file still exports
only the hook's `KEY=VALUE` body.

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

age decrypts with no passphrase, so the same hook survives the headless `!!!`
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

## Cleanup And Recovery

- `moe session list|abandon|resolve|gc` inspects or cleans leftover stage
  session worktrees and branches.
- `moe clone list|gc` inspects or removes orphan per-run sandbox clones.
- `moe workspace release` clears a stale named-workspace claim.
- `MOE_GIT_TRACE=1` (strict `=1`) prints one line per git invocation —
  dir, argv, duration, error — to stderr. First tool to reach for when a
  stage open or sync stalls inside git.

Each document keeps its latest conversation per backend. Switching backends and
returning resumes that backend's conversation; changing its model keeps the
same conversation. The canvas carries shared work between backends. Headless
turns still start fresh and replace only their backend's latest session ID.

Stage logic can recover orphaned Claude sessions from the Claude cache or an
identity-matching mirror. Codex requires its native rollout to resume. Missing
history starts a fresh conversation with a notice. Malformed or conflicting
mirror identity stops the turn before launch when recovery needs that mirror.

Older runs' scalar session IDs are adopted lazily using the selected backend's
native cache or mirror identity. The legacy value stays intact while new turns
write backend-keyed IDs in run.json. Running an older MoE binary after this
metadata migration is unsupported: it cannot preserve the new session map.
