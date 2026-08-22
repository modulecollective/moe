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
  last sweep died.
- `moe serve [--addr <host[:port]>] [--port <n>] [--dynamic]` runs the local
  web UI, bound to `127.0.0.1:4242` by default. Beyond runs and canvases, its
  read-only surface browses lore, a projects index with per-project hubs,
  project knowledge topics, twin documents, and a dashboard with the same
  daily-activity chart (and a project-scoped one on each project page).
  **The web starts nothing.** Every action it offers writes a journal commit and
  stops there: capture or edit an idea, tag it for a workflow, close or reopen a
  run, mark the current stage advanced. Starting agents is the heartbeat's job
  alone, so there is no route from the listener to code execution at all —
  armed or not. Interactive stage-driving happens in a terminal.
  `--dynamic` (or a non-empty `MOE_SERVE_DYNAMIC`) is the standing consent rung:
  it starts the resident heartbeat, a per-project ticker that runs `moe pulse
  new --dynamic` when the board warrants it. Unarmed, the ticker never fires and
  what the web wrote simply waits. Stopping the process retracts it. A `/serve`
  page owns that ticker's
  status and trace — what each recent tick decided, and the output tail of any
  sweep that failed — reachable from the menu and from the one-line status
  cluster every board header carries.
- `moe project mode <id> [paused|safe|auto]` caps what the heartbeat may start
  in one project, without stopping the process. See below.
- `moe chore list|check|open|skip` lists due project chores, dry-runs a chore
  definition, opens the run a due chore configures, or clears a due chore until
  it is next triggered.
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
  and is called out in the report.
- `moe where` prints the resolved bureaucracy path.
- `moe version` prints the moe version.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context. Both `cat` and `log` accept `@latest` in the
  `<run>` slot to mean the workflow's most-recent run.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy.
- `moe project add <repo-url>` registers a target project.
- `moe project list` lists registered projects with their mode.
- `moe project mode <id> [paused|safe|auto]` reads or sets a project's mode —
  the standing cap on what the heartbeat may start there:
  - `paused` — the heartbeat never sweeps the project. No agent turn is spent
    on it at all.
  - `safe` — it sweeps and grooms as ever (survey, park, chore nomination), but
    the kick starts only threads carrying an explicit operator mark: a valid
    advance marker, a chore's standing intent, or a workflow tag on the idea a
    run was promoted from. Everything else is held with a named reason.
  - `auto` — the default, and today's behaviour: every kickable parked thread
    the survey doesn't park gets started.

  The mode binds the clock, not you. `!`, `!!`, `!!!`, `moe chain kick`, stage
  verbs and a hand-typed `moe pulse new --dynamic` all run in every mode — the
  typed word is consent whatever the config says. Serve's advance mark is the
  same shape from the other direction: it writes an operator mark, which is
  exactly what `safe` looks for before it starts a thread.
  Serve's arming stays above all three: an unarmed serve automates nothing
  anywhere. The mode is stored in `projects/<id>/project.json` (absent means
  `auto`) and is settable from the project hub's switch as well.
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe chain new [--seed] <project>/<slug>` mints a chain run: a stageless
  placeholder head to collect a batch under. `--seed` pops `$EDITOR` on its
  purpose note first.
- `moe chain edit` opens an editor over active operator-cascade runs (SDLC,
  twin) plus chain heads; reorder
  lines to record a run chain in the bureaucracy journal.
- `moe chain note <project>/<run>` edits a head's purpose note: why the batch
  exists. Membership isn't written there — it renders live from the edges.
- `moe chain kick <project>/<run>` rides a chain headlessly from
  the named head. What is chained now is what runs: a kick sweeps nothing, so
  nothing can grow the batch under it.
- `moe chain close [--no-edit] <project>/<run>` drops a head without riding it.
- `moe chain clear [--yes]` drops every currently live run-chain edge.
- `moe <workflow> close [--no-edit] <project>/<run>` closes a run in any
  workflow; for `sdlc` it abandons the run instead of shipping it through
  `sdlc push`.

### Workflows

- `moe sdlc new|design|code|review|test|push|close|harvest|shell|reopen|cat|log`
  drives designed code work.
- `moe chat new|chat|close|harvest|cat|log` drives thinking-partner sessions.
- `moe idea new|edit|close|list|move|tag|untag|reopen|cat|log|harvest` manages
  backlog notes. `tag` stamps the workflow tag that licenses a pulse to start
  the idea (`untag` clears it); untagged ideas are operator-only.
- `moe intent new|edit|close|list|cat|log|harvest` manages operator-authored
  standing direction.
- `moe twin reflect|vision|architecture|patterns|operations|glossary|finalize|close|harvest|cat|log`
  maintains recorded intent.
- `moe pulse new|pulse|close|cat|log` runs and inspects a project's read-only
  backlog sweep. `moe pulse new --dynamic <project>` runs it at the dynamic
  consent rung — the sweep starts every kickable parked thread on the board
  instead of parking them, which is what makes the verb callable by a clock.
  The heartbeat inside `moe serve --dynamic` calls exactly this, and reads the
  exit status — which describes the whole invocation, including the rides the
  sweep started. `moe pulse new` exits non-zero when the sweep died, concluded
  nothing, or a kicked ride stalled, which drives the failure backoff. A skip
  exits 130, not a failure. It also passes `--emit-run <path>`: the sweep writes
  the run it opens there, so /serve can link a sweep to the pulse run it minted
  rather than leaving the operator to hunt the dash for a matching `pulse-*`
  slug.

`moe <workflow> harvest [--no-edit] <project>/<run>` re-runs a run's
`followups.md` and `feedback/lore.md` harvests — into ideas and `lore/`
respectively — without closing it. It picks up captures a re-run regenerated
after the run was already closed, and it is the recovery verb for a
conversational session whose own session-end harvest failed partway.

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
`.git` on the working directory and each `--add-dir` root. Codex's stock
`workspace-write` mode leaves `.git` read-only, which fails a `git commit`
inside the per-run clone; shipping the profile with MoE keeps that from
depending on operator config.

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
model, everything else stays cheap" is a two-line rule instead of a per-command
flag.

```css
/* Stages not matched here keep the vendor CLI's own default model.
   `fable` is claude's floating latest-in-family alias. */

sdlc.design { model: fable; }
sdlc.review { model: fable; }
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

**Grammar.** CSS-ish `selector { property: value; ... }` rules plus `/* ... */`
comments. Two properties in v1:

- `model` — handed verbatim to the backend's `--model` (unless a paired
  `agent:` scopes it to a backend the turn isn't running — see Precedence
  below). MoE keeps no model catalog, so unlike selectors, property names, and
  `agent:` values, a `model:` value is **not** validated at load: a bad id fails
  at turn start as the backend CLI's own error. Family aliases
  (`fable`/`opus`/`sonnet` on claude) and un-dated ids (`gpt-5-codex` on codex)
  float with releases; full ids (`claude-fable-5`) pin.
- `agent` — the backend name (`claude` | `codex`), resolved through the same
  registry `--agent` uses.

**Selectors** have two axes — workflow and stage. Because a bare stage name is
ambiguous across workflows (`vision` is a stage of `twin` alone, `code` of `sdlc`),
stage-only selectors take a leading dot:

| Selector      | Matches                     | Specificity |
| ------------- | --------------------------- | ----------- |
| `*`           | every stage turn            | 0           |
| `sdlc`        | every stage of one workflow | 1           |
| `.review`     | that stage in any workflow  | 1           |
| `sdlc.review` | exactly one workflow stage  | 2           |

Highest specificity wins per property; equal specificity breaks to the
last rule in the file. The two properties cascade independently — a
`sdlc.design { model: … }` does not clear an `agent:` inherited from a
`sdlc { … }` rule.

**Precedence.** The stylesheet sits below your explicit per-run bindings and
above the background defaults, mirroring "explicit beats the stylesheet":

- Agent: `$MOE_FORCE_AGENT` → `--agent` flag → `run.json` agent → **stylesheet**
  → `claude`.
- Model: **stylesheet** → backend CLI default. (There is no `--model` flag or
  `$MOE_MODEL` in v1 — editing the checked-in file is the one knob.)

Steer a run with `--agent`, the whole process with `$MOE_FORCE_AGENT`, or the
ambient default with a broad stylesheet `agent:` rule.

A `model:` paired with an `agent:` is scoped to that backend: it rides only when
the turn's resolved backend matches the stylesheet's own resolved `agent` for
that (workflow, stage) — the winning `agent` property after the cascade, not
literally the same rule. If the ladder resolves a different backend — via
`$MOE_FORCE_AGENT`, `--agent`, or the `run.json` agent — the stylesheet model is
dropped (the backend's own default applies) and a one-line stderr notice says so
at turn start. An unpaired `model:` (no `agent` resolves for the stage) is
handed verbatim to whatever backend runs; a name that backend can't serve fails
at turn start as **the backend CLI's own error** — MoE keeps no model catalog
and never validates the value itself. Pair an `agent:` when it matters: pairing
is what scopes the model to its backend.

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
`design code review test …`) and the `<project>/<run>` slug for run-taking verbs
(`moe sdlc code ⇥`), plus idea slugs (including `--from-idea`) and named
workspaces. The script itself never changes as commands are added — all the
logic lives in `moe` and is best-effort, so completion stays silent outside a
bureaucracy rather than erroring.

## Hooks And Environments

Project hooks live under `projects/<project>/hooks/<event>.d/*` in the
bureaucracy:

- `dev-env.d/*` emits `KEY=VALUE` lines that MoE caches and supplies to agent
  sessions and workspace shells.
- `dev-env-teardown.d/*` cleans up when a run or workspace closes.
- `pre-push.d/*` is an invocation-time ship gate; a failing script halts the
  push path and opens a recovery code session.

Use `moe hook fire <project> dev-env|dev-env-teardown|pre-push` to exercise one
event in a transient sandbox without creating a run.

Hooks, chore definitions, and knowledge topics are bureaucracy-side
artifacts: edit them by hand, or let an sdlc run land them. An sdlc stage's
per-turn commit picks up `projects/<project>/hooks`, `chores`, and
`knowledge`, so `moe sdlc design → code → close` is the journaled route
with no push involved.

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
its trees.

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
password manager); a leaked bureaucracy clone is then ciphertext only. Rotating a
secret re-decrypts on the next run; a named workspace needs `moe workspace
refresh` to pick up new values. If a framework insists on reading a `.env` off
disk, redirect the same `age -d` output to `"$MOE_SANDBOX/.env"` instead — but
only when the target repo already gitignores that file, since `pre-push` refuses
to ship with any untracked file present.

## Cleanup And Recovery

- `moe session list|abandon|resolve|gc` inspects or cleans leftover stage
  session worktrees and branches.
- `moe clone list|gc` inspects or removes orphan per-run sandbox clones.
- `moe workspace release` clears a stale named-workspace claim.
- `MOE_GIT_TRACE=1` (strict `=1`) prints one line per git invocation —
  dir, argv, duration, error — to stderr. First tool to reach for when a
  stage open or sync stalls inside git.

Stage logic can recover orphaned Claude sessions from the Claude cache or from
mirrored transcript files when the normal close path was interrupted.
