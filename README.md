# ▓▒░ MINISTRY OF EVERYTHING ░▒▓

Ministry of Everything (MoE) is a CLI-first harness for one operator directing
AI agents through durable markdown work.

MoE runs [Claude Code](https://claude.com/claude-code) or
[Codex](https://chatgpt.com/codex/) against living markdown documents. Each
stage produces a canvas: a short artifact the next stage can read without
replaying the whole chat. Every turn is committed to a personal Git journal, so
the project keeps memory that can be resumed, reverted, audited, and reused.

There is no background worker and no autonomous scheduler. Agents act when you
invoke a command. The operator stays strategist, reviewer, and source of
judgment; MoE removes the coordination tax around opening work, handing context
forward, checking progress, and filing the lessons that should shape the next
run.

Everything works from the CLI:

![MoE CLI dashboard - open runs and backlog in CLI](docs/dash-cli.png)

And there's also a small web server available which is useful for quick checks
from a phone (via something like Tailscale) or for use locally to browse runs
and read canvas files:

![MoE web dashboard - open runs and backlog with local web server](docs/dash-web.png)


## What MoE Is

MoE has two repos in play:

- `moe/` is this Go CLI. It is a thin wrapper around `git` and the selected
  agent backend.
- `bureaucracy/` is your private operating journal: registered projects,
  runs, stage documents, ideas, project twins, lore, hooks, and the markdown
  fragments that steer agents. MoE finds it by walking up from `$PWD` to a
  `bureaucracy.conf` marker, or by reading `$MOE_HOME`.

A workflow is a small ladder of stages. A run is one pass through that ladder.
A stage has one canvas file at
`projects/<project>/runs/<slug>/documents/<stage>/content.md`. The agent reads
that file, talks with you, edits the file, and MoE commits the turn with
trailers like `MoE-Run`, `MoE-Document`, `MoE-Session`, and `MoE-Workflow`.

Git is the checkpoint. Rewinding a bad turn is `git reset --soft`; undoing a
landed turn is `git revert`. There is no separate database that knows the real
history better than the journal.

You might want MoE if:

- you run several agent threads and need to resume them without chat-history
  archaeology;
- you want agents to work from durable design, test, review, and knowledge
  artifacts instead of one long prompt;
- you want follow-up ideas, project intent, and cross-project lessons to feed
  future runs automatically;
- you want recurring maintenance — dependency bumps, doc sync, changelog
  updates — to surface as ready-to-open runs instead of living in your memory;
- you prefer explicit CLI commands and Git history over a hosted coordination
  product.

## The Core Loop

A normal software-development pass looks like this:

```sh
mkdir my-bureaucracy && cd my-bureaucracy
moe init
moe project add <repo-url>

moe sdlc new my-project/add-batch-support
moe sdlc design my-project/add-batch-support
moe sdlc code my-project/add-batch-support
moe sdlc test my-project/add-batch-support
moe sdlc push my-project/add-batch-support
```

The `new` command opens the run and writes the first files into the
bureaucracy. `design` shapes the request into a reviewable plan. `code` gives
the agent write access inside an isolated clone of the target project and
requires it to commit the implementation there. `test` verifies the behavior
and records what was run. `push` fast-forwards the target project's default
branch, or opens a PR with `--pr`.

At the end of a stage, MoE prints a chain prompt. The shortcuts are:

More bangs go further. Every cascade is headless — the axis is *how far*,
not *how*:

- `!` runs exactly the next stage headlessly and then parks at the next gate.
- `!<stage>` runs headlessly up to that named gate, without shipping.
- `!!` runs every remaining stage headlessly and ships **this run** (or
  auto-closes, for workflows without a push gate), then stops.
- `!!!` is the same as `!!`, but after this run ships it **rides the whole
  chain** — cascading into the next live chained run.

In practice the everyday path is lighter: jot an idea, shape the design in one
conversation, then let the rest run:

```sh
moe idea new my-project/add-batch-support              # jot it when it occurs to you
moe sdlc new --from-idea my-project/add-batch-support  # promote it to a run
```

Promoting the idea will offer to jump into the design stage right away. When
the design stage ends, MoE prints the chain prompt. Type `!!` there to
run `code`, `test`, and `push` headlessly. The five-command block above is the
same path spelled out by hand.

When several SDLC runs are already designed and ready for code/test, use
`moe chain edit` to order the active runs in `$EDITOR`. `!!!` is the chain
lever: after it ships one run, MoE rides into the next live chained run at
its first pending stage and keeps going. `!!` ships just the run in front of
you and stops — reach for it when you want to ship one thing without setting
the whole queue in motion.

`moe dash` is the terminal home screen for re-entry. `moe serve` starts a local
web UI, bound to `127.0.0.1:4242` by default, that shows the dashboard, run
detail pages and canvas links, can open and parent live SDLC runs, and can edit,
close, promote, or reopen ideas.

## Install

Requires Go 1.26+ and at least one agent backend on your `PATH`:
[Claude Code](https://claude.com/claude-code) for `claude`, or Codex for
`codex`.

```sh
go install github.com/modulecollective/moe/cmd/moe@latest
```

Then initialize a bureaucracy and register a project:

```sh
mkdir my-bureaucracy && cd my-bureaucracy
moe init
moe project add <repo-url>
```

The default backend is `claude`. To prefer Codex for new runs, set:

```sh
export MOE_AGENT=codex
```

You can also pass `--agent claude` or `--agent codex` when opening a run or an
individual stage. `moe help` and per-command usage are the source of truth for
the exact command surface.

### Codex Setup

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

## Ways To Use MoE

| Workflow | Stages | Use it for |
| --- | --- | --- |
| `sdlc` | `design` -> `code` -> `test` -> `push` | designed code changes with a ship gate |
| `audit` | `plan` -> `report` | fresh-eyes review that files feedback but does not push code |
| `chat` | one `chat` session, resumed across sittings | thinking-partner sessions to reason, decide, and groom the backlog, without writing code |
| `kb` | `research` -> `summarize` | project knowledge articles |
| `idea` | one `idea` canvas, edited through verbs | backlog capture before a full run exists |
| `twin` | `vision` -> `architecture` -> `patterns` -> `operations` -> `glossary` -> `finalize` | recorded project intent |
| `hooks` | `code` | project-specific hook scripts |
| `chores` | `code` | edit project chore definitions: what maintenance is due, and the run each one opens |
| `meta-moe` | `report` | feedback about MoE itself |

### SDLC

`moe sdlc` is the main software-development workflow:

```sh
moe sdlc new [--workspace <name>] [--agent <name>] <project>/<slug>
moe sdlc design [--agent <name> | --once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc code   [--agent <name> | --once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc test   [--agent <name> | --once | --to=<stage> | --ship | --chain] <project>/<run>
moe sdlc push [--pr] <project>/<run>
```

`moe sdlc new --from-idea <project>/<slug>` promotes an idea into a run and
seeds the design canvas from the idea body. `moe sdlc reopen <project>/<slug>`
starts a new run seeded with a terminal prior run's design canvas, useful
when a closed or merged topic still has more work behind it.

The cascade mode flags on `design`/`code`/`test` mirror the post-stage chain
prompt's bang vocabulary at the CLI: `--once` (= `!`) dispatches one stage
headless and parks at the next gate; `--to=<stage>` (= `!<stage>`) walks
headless to a named gate; `--ship` (= `!!`) cascades headless through push
and ships this run; `--chain` (= `!!!`) does the same and then rides the
whole chain. They are mutually exclusive and cannot combine with `--agent`
(cascade walks multiple stages on the run's persisted agent).

Chains are the batch version of that same forward motion for active SDLC runs.
`moe chain edit` opens every active SDLC run across projects; reorder the lines
to make a sequence, delete lines you want left unchained, and save. `moe dash`
shows a `chained -> <project>/<run>` hint for active parents with a live child.
When a `!!!` cascade reaches the end of a chained parent, MoE starts the child
at its first pending stage: a fresh child starts at `design`, while a partly
completed child resumes where it is parked. (`!!` ships the parent and stops —
it does not ride into the child.)

When you type an older idea or run slug into an SDLC command, MoE follows
promotion and reopen trailers where it can. In an interactive shell it can ask
whether you meant the current descendant; in non-interactive use it prints a
hint.

### Audit

`moe audit` is a review workflow, not a shipping workflow:

```sh
moe audit new <project>/<slug>
moe audit plan <project>/<run>
moe audit report <project>/<run>
moe audit close <project>/<run>
```

The plan stage records what the review should cover. The report stage reads the
project, prior canvases, and digital twin, then writes ranked findings and files
followups, twin observations, or lore through the normal feedback channels. It
has no push stage.

### Chat

`moe chat` is a thinking-partner workflow, not a coding or shipping one:

```sh
moe chat new [--workspace <name>] [--agent <name>] <project>/<slug>
moe chat chat [--agent <name>] <project>/<run>
moe chat close [--no-edit] <project>/<run>
```

`new` opens the run, `chat` opens or resumes the session, and `close` archives
it. The agent reads project source through a per-run sandbox clone but never
edits it and never drives coding: if the conversation lands on "this needs
building", it captures an idea and you start the SDLC ladder yourself. The run
stays open across sittings, so re-running `chat` continues the same thread. The
canvas is a moe-written session log; the conversation transcript is the record,
read back with `moe chat log`. Grooming the idea backlog (`moe idea
new|edit|close|reopen`) is the one state change a chat session makes on your
behalf.

### Ideas

`moe idea` is the cheap backlog surface:

```sh
moe idea new [--chat] <project>/<slug>
moe idea edit [--chat] <project>/<slug>
moe idea list <project>
moe idea move <project>/<slug> <to-project>
moe idea close <project>/<slug>
moe idea reopen <project>/<slug>
```

By default, idea capture and editing use `$EDITOR`; pass `--chat` when you want
an agent to help shape the note. Promoting an idea to SDLC preserves lineage in
the journal. `idea reopen` is for a promoted idea whose destination run was
abandoned and should become backlog again.

### Chores

Chores turn recurring project maintenance into runs you open on demand. A chore
definition says what maintenance is due, when it becomes due, and which workflow
run to open for it. MoE evaluates chores against the journal and surfaces the due
ones — but nothing fires on its own. A due chore is a seeded run waiting in
`moe dash` until you choose to open it.

A chore is a directory of small files under `projects/<project>/chores/<name>/`:

    projects/my-project/chores/bump-deps/
      cadence      # 720h     -> due monthly
      cooldown     # 48h       -> don't re-open within 48h of finishing
      prompt.md               -> the seed prompt the opened run starts from

    projects/my-project/chores/regen-docs/
      trigger      # go.mod    -> due when a merged change touches this path
      workflow     # sdlc      -> the run to open (sdlc is the default)
      prompt.md               -> "Regenerate the dependency table; go.mod changed."

A chore needs a `trigger`, a `cadence`, or both. `trigger` is a path glob (or
`*` for any merged project change); `cadence` makes it due on a clock. A chore
goes due when its trigger matches new merged work, its cadence elapses, or its
own definition changes — unless it is cooling down or already has an open run.

Two command families, mirroring hooks:

```sh
moe chores new|code|close <project>/<run>     # edit chore definitions (journaled)
moe chore list [--project <p>]                # show what's due
moe chore check [<project>/<chore>]           # dry-run validation and due-state
moe chore open <project>/<chore>              # open the seeded run for a due chore
```

`moe chores …` edits definitions under `projects/<project>/chores/*` through a
journaled run. `moe chore open` refuses if the chore isn't due, already has an
open run, or is cooling down.

### Knowledge, Twin, Hooks, And Meta-MoE

`moe kb new`, `moe kb research`, and `moe kb summarize` maintain open-schema
knowledge articles for a project. `moe kb lint <project>` checks wiki hygiene
without opening a run.

`moe twin reflect <project>` walks the fixed digital-twin documents and folds
new observations into recorded intent. `moe twin claim <project>` records a
decided out-of-band twin edit without opening a laddered run.

`moe hooks new`, `moe hooks code`, and `moe hooks close` are the journaled loop
for project hook scripts. `moe hook fire <project> <event>` is the fast loop:
it creates a transient sandbox, runs one event's scripts once, prints the
sandbox path, and exits.

Chores get their own section above; the editing/supervision split mirrors hooks.

`moe meta-moe new` and `moe meta-moe report` inspect a project's MoE history
and produce maintainer-facing feedback about the harness itself.

## Concepts

### Runs, Stages, And Canvases

Runs live under `projects/<project>/runs/<slug>/`. Each run has `run.json` plus
one document directory per stage. The canvas is the public artifact for that
stage; the raw transcript is stored beside it as agent-specific JSONL so
`moe <workflow> log` can render the conversation later.

`moe <workflow> cat <project>/<run> <stage>` prints a canvas. For one-stage
workflows, the stage can usually be omitted. `moe <workflow> log` renders the
transcript; `--agent claude|codex` disambiguates if both transcript files exist.

### Bureaucracy Repo And Target Repos

The bureaucracy is the journal. Target projects are registered as submodules
under it. MoE materializes a project before commands touch its source, so cold
projects pay one submodule checkout and warm projects are cheap.

Code-writing stages do not edit the canonical submodule directly. They use a
per-run sandbox clone under `.moe/clones/<project>/<run>/`, created from the
target project and isolated from other runs.

### Sandboxes And Workspaces

Per-run sandbox clones are disposable and scoped to one run. Named workspaces
are long-lived working trees for cases where setup cost matters:

```sh
moe workspace new <project>/<name>
moe workspace list [<project>]
moe workspace shell <project>/<name>
moe workspace refresh <project>/<name>
moe workspace release <project>/<name>
moe workspace remove <project>/<name>
```

A named workspace can be claimed by one run at a time, but the directory
survives run close. `refresh` rebuilds cached `dev-env.d/*` output in place;
`release` clears a stuck claim.

### Feedback Channels

MoE's memory improves through a few explicit channels:

- Followups are out-of-scope work noticed during a run. Agents write them to
  `followups.md`; close-time harvest promotes surviving entries to ideas.
- The idea backlog holds work that is worth remembering but not ready for a
  full run.
- The digital twin records project intent in `vision`, `architecture`,
  `patterns`, `operations`, and `glossary` documents. When code and
  twin disagree, the twin wins until a deliberate edit updates it.
- Lore stores portable facts that apply across projects. Agents see a compact
  catalog and open entries only when the "applies when" hint matches.
- Meta-MoE reports are feedback about the harness itself.

## Command Reference

The catalog below is a map, not a replacement for `moe help`.

### Re-Entry And Supervision

- `moe dash [--all] [--project <id>] [--workflow <name>]` prints the terminal
  dashboard, including a CHORES bucket for due project chores.
- `moe serve [--addr <host[:port]>] [--port <n>]` runs the local web UI.
- `moe chore list|check|open` lists due project chores, dry-runs a chore
  definition, or opens the run a due chore configures.
- `moe where` prints the resolved bureaucracy path.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy.
- `moe project add <repo-url>` registers a target project.
- `moe project list` lists registered projects.
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe chain edit` opens an editor over active SDLC runs; reorder lines to
  record a run chain in the bureaucracy journal.
- `moe chain clear [--yes]` drops every currently live run-chain edge.
- `moe <workflow> close [--no-edit] <project>/<run>` closes workflows that do
  not ship through `sdlc push`.

### Workflows

- `moe sdlc new|design|code|test|push|reopen|cat|log` drives designed
  code work.
- `moe audit new|plan|report|close|cat|log` drives review passes.
- `moe chat new|chat|close|cat|log` drives thinking-partner sessions.
- `moe kb new|research|summarize|close|cat|log|lint` drives project knowledge.
- `moe idea new|edit|close|list|move|reopen|cat|log` manages backlog notes.
- `moe twin reflect|vision|architecture|patterns|operations|glossary|finalize|claim|close|cat|log`
  maintains recorded intent.
- `moe hooks new|code|close|cat|log` edits project hook scripts through a
  journaled run.
- `moe chores new|code|close|cat|log` edits project chore definitions through a
  journaled run.
- `moe meta-moe new|report|close|cat|log` records MoE-maintenance feedback.

### Hooks And Environments

Project hooks live under `projects/<project>/hooks/<event>.d/*` in the
bureaucracy:

- `dev-env.d/*` emits `KEY=VALUE` lines that MoE caches and supplies to agent
  sessions and workspace shells.
- `dev-env-teardown.d/*` cleans up when a run or workspace closes.
- `pre-push.d/*` is an invocation-time ship gate; a failing script halts the
  push path and opens a recovery code session.

Use `moe hook fire <project> dev-env|dev-env-teardown|pre-push` to exercise one
event in a transient sandbox without creating a run.

### Cleanup And Recovery

- `moe session list|abandon|resolve|gc` inspects or cleans leftover stage
  session worktrees and branches.
- `moe clone list|gc` inspects or removes orphan per-run sandbox clones.
- `moe claude-cache gc` removes orphan Claude session cache files after their
  mirrored transcripts have been recovered.
- `moe workspace release` clears a stale named-workspace claim.

Stage logic can recover orphaned Claude sessions from the Claude cache or from
mirrored transcript files when the normal close path was interrupted. The GC
verb is for cleanup after that recovery path has had its chance.

## How Agents Are Steered

MoE assembles an instruction preamble fresh for every turn. The important
inputs are plain markdown:

- [`soul.md`](soul.md) defines the general operating philosophy and quality
  bar.
- `workflows/<workflow>/<stage>.md` defines the lens for the current stage.
- The stage-location header says where the run is in the ladder and what the
  chain prompt will offer next.
- Project digital-twin documents point the agent at recorded intent.
- Lore and followup pointers tell the agent where to look and where to leave
  traces.
- Project-specific guidance such as `AGENTS.md` or `CLAUDE.md` is named
  explicitly because the agent's working directory may be the bureaucracy
  rather than the target repo.

The rule is simple: if the agent keeps making the same kind of mistake, prefer
editing the markdown it reads over adding Go code.

## Skills

Claude Code and Codex both support skills: named markdown files the backend can
load when their description matches the situation. MoE ships three:

- `moe-bureaucracy` teaches agents how to leave traces for downstream runs:
  followups, twin observations, and lore, without exceeding the current stage's
  scope.
- `moe-context` teaches agents how to read the bureaucracy as context: prior
  runs' canvases, journal trailers for slicing by run/doc/workflow, past
  transcripts, the twin, and lore.
- `moe-howto` teaches agents how to capture and groom the idea backlog from a
  chat session, the verb set chat uses on your behalf.

MoE materializes the relevant skills into the session's backend-specific skill
directory with paths already filled in for the current run.

## Status

MoE is pre-1.0 and under active development. The command surface, file layout,
and trailer conventions can change. Expect sharp edges.

## Contributing

Don't :-) Not accepting issues or PRs right now. This is one firm's internal
tool, shared in case it is useful.

## License

MIT. See [`LICENSE`](LICENSE).

## References

- [Module Collective: Building a Ministry of Everything](https://www.modulecollective.com/posts/building-a-ministry-of-everything/)
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html)
- [Karpathy: LLM Wiki gist](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
