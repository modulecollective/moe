# Reference

Look-things-up material: the command catalog, backend setup, shell completion,
hooks and environments, and cleanup. `moe help` and per-command usage are the
source of truth for the exact command surface; this page is a map.

## Command Catalog

### Re-Entry And Supervision

- `moe dash [--all] [--project <id>] [--workflow <name>]` prints the terminal
  dashboard, including a CHORES bucket for due project chores and a
  daily-activity histogram of recent run activity (`--project` scopes the chart
  to one project).
- `moe serve [--addr <host[:port]>] [--port <n>] [--insecure]` runs the local
  web UI, bound to `127.0.0.1:4242` by default. Beyond runs and canvases, its
  read-only surface browses lore, a projects index with per-project hubs,
  project knowledge topics, twin documents, and a dashboard with the same
  daily-activity chart (and a project-scoped one on each project page). **Safe
  by default:** all views,
  idea capture/edit/close/reopen, and run close/edit/reopen work, but the
  run-spawning actions — opening new runs, advancing a stage, kicking a chain
  head, and opening a due chore's run — refuse with 403. Pass `--insecure` (or set a
  non-empty `MOE_SERVE_INSECURE`) to enable them; anything that can reach the
  listener can then execute code.
- `moe chore list|check|open|skip` lists due project chores, dry-runs a chore
  definition, opens the run a due chore configures, or clears a due chore until
  it is next triggered.
- `moe where` prints the resolved bureaucracy path.
- `moe version` prints the moe version.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context. Both `cat` and `log` accept `@latest` in the
  `<run>` slot to mean the workflow's most-recent run.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy.
- `moe project add <repo-url>` registers a target project.
- `moe project list` lists registered projects.
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe chain new [--seed] <project>/<slug>` mints a chain run: a stageless
  placeholder head to collect a batch under. `--seed` pops `$EDITOR` on its
  purpose note first.
- `moe chain edit` opens an editor over active operator-cascade runs (SDLC,
  twin, KB, hooks, chores) plus chain heads; reorder
  lines to record a run chain in the bureaucracy journal.
- `moe chain note <project>/<run>` edits a head's purpose note: why the batch
  exists. Membership isn't written there — it renders live from the edges.
- `moe chain kick <project>/<run>` rides a chain headlessly from the named head.
- `moe chain close [--no-edit] <project>/<run>` drops a head without riding it.
- `moe chain clear [--yes]` drops every currently live run-chain edge.
- `moe <workflow> close [--no-edit] <project>/<run>` closes a run in any
  workflow; for `sdlc` it abandons the run instead of shipping it through
  `sdlc push`.

### Workflows

- `moe sdlc new|design|code|review|test|push|close|harvest|shell|reopen|cat|log`
  drives designed code work.
- `moe chat new|chat|close|harvest|cat|log` drives thinking-partner sessions.
- `moe kb new|research|summarize|close|harvest|cat|log|lint` drives project
  knowledge.
- `moe idea new|edit|close|list|move|reopen|cat|log` manages backlog notes.
- `moe twin reflect|vision|architecture|patterns|operations|glossary|finalize|close|harvest|cat|log`
  maintains recorded intent.
- `moe hooks new|code|close|harvest|cat|log` edits project hook scripts through a
  journaled run.
- `moe chores new|code|close|harvest|cat|log` edits project chore definitions
  through a journaled run.
- `moe pulse new|pulse|close|cat|log` runs and inspects a project's read-only
  backlog sweep.

`moe <workflow> harvest [--no-edit] <project>/<run>` re-runs a run's
`followups.md` harvest into ideas without closing it — the way to pick up
follow-ups a re-run regenerated after the run was already closed.

## Codex Setup

If you use the `codex` backend interactively, add this profile to
`~/.codex/config.toml`:

```toml
[permissions.workspace-git.filesystem]
":root" = "read"
":tmpdir" = "write"

[permissions.workspace-git.filesystem.":project_roots"]
"." = "write"
".git" = "write"
```

MoE selects it with `-c default_permissions=workspace-git`. Without the profile,
interactive Codex sessions can fail when Git needs to write
`<clone>/.git/index.lock`.

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
ambiguous across workflows (`code` is a stage of both `sdlc` and `chores`),
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
  → `$MOE_AGENT` → `claude`.
- Model: **stylesheet** → backend CLI default. (There is no `--model` flag or
  `$MOE_MODEL` in v1 — editing the checked-in file is the one knob.)

Note the consequence: a stylesheet `agent:` shadows `$MOE_AGENT`. If your rules
pair `agent:` (as recommended), `$MOE_AGENT` does nothing for those stages —
steer a single turn with `--agent`, or the whole process with
`$MOE_FORCE_AGENT`.

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

Stage logic can recover orphaned Claude sessions from the Claude cache or from
mirrored transcript files when the normal close path was interrupted.
