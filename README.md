# ▓▒░ MINISTRY OF EVERYTHING ░▒▓

*An anti-social agent harness — one operator, a gaggle of bots.*

Ministry of Everything (MoE) helps one operator turn intent into parallel
agent work without losing context or control. Agents work in bounded threads
attached to markdown documents; every conversation, decision, and artifact
is saved back into a Git journal so the project keeps an improving memory of
itself. The human stays strategist, scheduler, reviewer, and source of
judgment — MoE removes the coordination tax between thought and execution so
the operator and the bots share a compounding model of the project.

MoE runs [Claude Code](https://claude.com/claude-code) or
[Codex](https://chatgpt.com/codex/) against living markdown documents. The
document is the compact handoff between stages; the conversation that
produced it is saved underneath but rarely needs to be re-read. Software
development is the first workflow, with knowledge-base, hook-authoring,
meta-review, and digital-twin workflows alongside. There is no background
worker, no TUI, no dashboard that updates on its own — agents act only when
you invoke a command, and the UX problem is **prioritization, supervision,
and resumption**, not real-time updates.

![MoE dashboard — open runs and backlog](docs/dash.png)

## At a glance

MoE is a small CLI wrapped around a durable operating journal:

- **`moe/`** (this repo) — the Go CLI. Stdlib only, shells out to
  `git` and `claude`.
- **`bureaucracy/`** — the operator's personal journal: projects,
  runs, documents, backlog, digital twins, and the markdown fragments
  that steer agents. Discovered via a `bureaucracy.conf` marker file
  found by walking up from `$PWD`, or via `$MOE_HOME`.

Every turn lands as one commit on the bureaucracy's `main` branch, with
trailers (`MoE-Run`, `MoE-Document`, `MoE-Session`, …) that scope the
journal. Rewinding is `git reset --soft`; reverting is `git revert`.
Git is the checkpoint.

The feedback loop is the product: followups noticed mid-run flow into
the idea backlog, runs turn that backlog into shipped work, and twin /
kb / lore fold what each pass learned into the next run's starting
context. See [Feedback paths](#feedback-paths) for the channels that
close those loops.

## Install

Requires Go 1.26+ and [Claude Code](https://claude.com/claude-code) on
your `PATH`.

```sh
go install github.com/modulecollective/moe/cmd/moe@latest
```

Scaffold a bureaucracy:

```sh
mkdir my-bureaucracy && cd my-bureaucracy
moe init
```

Register a target project (a git repo — the "thing being worked on"):

```sh
moe project add <repo-url>
```

Pick a default agent backend if you want one (optional — defaults to
`claude`; set `MOE_AGENT` in your shell rc, or pass `--agent codex` on
`moe sdlc new` for a single run):

```sh
export MOE_AGENT=codex
```

Then open a run and walk it through the stages. Runs are addressed as
`<project>/<slug>`:

```sh
moe sdlc new tele/add-batch-support
moe sdlc design tele/add-batch-support
```

At the end of each stage MoE prints a chain prompt. The keystrokes `!`
(run the next stage headless), `!<stage>` (cascade up to a stage), and
`!!` (cascade all the way to ship) hand the rest of the chain to the
agent without re-typing the verb.

`moe help` is the source of truth for the command surface.

### Codex setup

If you'll use the `codex` backend, add this profile block to
`~/.codex/config.toml`:

```toml
[permissions.workspace-git.filesystem]
":root" = "read"
":tmpdir" = "write"

[permissions.workspace-git.filesystem.":project_roots"]
"." = "write"
".git" = "write"
```

MoE selects it on every codex invocation with
`-c default_permissions=workspace-git`. Without the block, interactive
codex sessions in code stages fail EROFS on `<clone>/.git/index.lock`
when committing — its sandbox protects the project's `.git/` subtree
more strictly than the `codex exec` path does, and the per-run clone
needs that subtree writable so the agent can commit. Headless
(`codex exec`) and `claude` are unaffected; the profile is harmless
for them.

## Workflows

A workflow is a short stage DAG with one canonical document per stage.
The current workflows are:

| Workflow   | Stages                                                     | For                                      |
|------------|------------------------------------------------------------|------------------------------------------|
| `sdlc`     | `design` → `code` → `test` → `push`                        | designed features with review and ship gates |
| `kb`       | `research` → `summarize`                                   | knowledge-base articles                  |
| `idea`     | single canvas — verbs `new` / `edit` / `close`             | backlog without starting a run           |
| `twin`     | `vision` → `architecture` → `patterns` → `operations` → `roadmap` → `glossary` → `finalize` | project digital twin |
| `hooks`    | `code`                                                     | project-specific automation hooks        |
| `meta-moe` | `report`                                                   | inspect the bureaucracy itself           |

Each stage is a subcommand that opens a Claude Code session on that
stage's document. Each workflow is its own top-level verb — `moe sdlc`,
`moe kb`, `moe twin`. For example:

```sh
moe sdlc new tele/add-batch-support         # open a new run
moe sdlc design tele/add-batch-support      # threaded chat on design/content.md
moe sdlc code tele/add-batch-support        # agent codes inside a sandbox clone
moe sdlc test tele/add-batch-support        # agent verifies and records what passed
moe sdlc push tele/add-batch-support        # fast-forward to the target repo; --pr opens one instead
```

`moe dash` shows your open runs and backlog. `moe idea` captures
loose ideas without starting a run. Followups discovered during work
can flow back into the backlog, and twin/kb passes keep project memory
fresh without turning documentation into a separate manual job.

## How it works

- **One operator, many bounded threads.** MoE does not try to replace
  judgment with autonomy. It gives the operator fast verbs for opening,
  resuming, chaining, closing, and shipping agent work while keeping
  every thread attached to an auditable canvas.
- **Project memory compounds across runs.** Four channels do the
  compounding: per-run **followups** caught mid-flight, the **idea**
  backlog they promote into, the project's **digital twin** of
  recorded intent, and bureaucracy-wide **lore** of facts that apply
  across projects. The output of one pass becomes context for the
  next, for both the human and the agents — see
  [Feedback paths](#feedback-paths) below.
- **Git is the checkpoint.** Every turn lands as one commit on `main`
  with trailers scoping the run, document, and session. Rewinding is
  `git reset --soft`; reverting a turn is `git revert <sha>`.
- **Markdown fragments steer the agent.** The instruction preamble
  the agent sees on each turn is assembled fresh from a fixed set of
  plain markdown files ([`soul.md`](soul.md),
  `workflows/<wf>/<stage>.md`, the project's digital twin, and so on).
  Fixing how the agent behaves usually means editing a fragment, not
  the Go code — the full assembly is described in
  [Prompt assembly](#prompt-assembly) below.

## Features

The verb catalog, grouped by what each one solves. `moe help` and the
per-verb usage lines have the full detail; this section is the
signpost.

### Runs and stages

The primitive is a thread of stage canvases attached to a project,
and the dashboard is the re-entry point when you walk away.

- `moe dash` — pick what to resume; lists open runs, the idea
  backlog, and any stale sessions that need cleanup.
- `moe <wf> new <project>/<slug>` — open a run in any workflow that
  takes operator-typed slugs (`sdlc` / `kb` / `idea` / `hooks` /
  `meta-moe`; the `twin` workflow opens via `moe twin reflect`
  instead, see below). `--from-idea <project>/<slug>` seeds the new
  run from an existing idea canvas, `--workspace <name>` (sdlc and
  hooks) attaches the run to a named workspace, `--agent claude|codex`
  pins the backend for this run only.
- `moe sdlc design|code|test|push` — drive the sdlc stages one at a
  time. The chain prompt at exit offers `!` (run the next stage
  headless), `!<stage>` (cascade up to a gate), and `!!` (cascade all
  the way to ship). `moe sdlc push` fast-forwards the target repo's
  default branch to the run's branch; `--pr` opens a pull request
  instead.
- `moe sdlc resume <project>/<run>` — pick up a parked run at
  whichever stage is pending without having to remember which verb is
  next.
- `moe sdlc reopen <project>/<slug>` — start a fresh run seeded with
  a terminal prior run's design canvas, for when you decide a closed
  problem actually wasn't.
- `moe <wf> cat` / `moe <wf> log` — dump a stage canvas, or render
  the recorded agent transcript, so you can audit a past turn.

### Project memory and follow-ups

The project should know more after each run than before it. See
[Feedback paths](#feedback-paths) for the channels themselves; these
are the verbs that drive them.

- Followups are captured inside a run via the `moe-bureaucracy`
  skill — the agent appends loose ends to the run's `followups.md`
  and the operator triages them at close. Surviving entries open as
  fresh `idea` runs with their context carried in.
- `moe idea new|edit|close|list|move` — the backlog surface;
  intentionally light (no agent unless `--chat`). `move` rehomes an
  idea to a different project; `list` shows the same backlog as the
  dash.
- `moe twin reflect <project>` — walk the seven-stage twin ladder to
  fold this round's twin-feedback observations forward into the
  recorded-intent documents.
- `moe twin claim <project>` — record a decided edit to the twin
  out-of-band (no run, no ladder), for when you already know what you
  want it to say.
- `moe kb research <project>/<slug>` / `moe kb summarize <project>/<slug>` —
  open-schema wiki for project knowledge that doesn't belong in code.
- `moe kb lint <project>` — run the open-schema hygiene check
  without driving a run.
- `moe meta-moe new` / `moe meta-moe report` — feedback to MoE
  itself, sourced from one project's run history.

### Workspaces and sandboxes

Code work happens in isolated working trees of the target repo, but a
dev server's warm state shouldn't die at every branch switch.

- **Per-run sandbox clones** at `.moe/clones/<project>/<run>/` —
  created automatically when a run's `code` stage first opens, one
  per run, isolated from every other run on the same project. Each
  is a `git clone --local --shared --no-checkout` of the canonical
  submodule (not a `git worktree` — that exposed a submodule /
  superproject gitdir boundary that codex's `apply_patch` tripped
  on).
- `moe workspace new <project> <name>` — eagerly create a long-lived
  working tree before any run attaches, e.g. to warm a dev server
  whose startup is slow.
- `moe workspace list [<project>]` / `moe workspace shell` — inspect
  named workspaces (optionally filtered to one project) or drop into
  one.
- `moe workspace refresh <project> <name>` — re-run the project's
  `dev-env.d/*` scripts in place when the cached env breaks.
- `moe workspace remove` / `release` — tear down a workspace, or
  clear a stuck claim left behind by a crash.
- `moe sdlc shell <project>/<run>` — drop into a shell rooted at the
  run's working tree (the sandbox clone, or the named workspace it
  attached to).

### Backends and tool scoping

Not every operator wants the same agent, and not every stage should
have the same tool surface.

- Two backends: `claude` (default) and `codex`. Selection is a
  four-rung ladder: `--agent` flag → `run.json` → `$MOE_AGENT` →
  `"claude"`.
- Document shapes the toolset. Non-code stages get `Read`, `Grep`,
  `WebSearch`, and a scoped `Edit` — the worst a bad turn does is
  write a bad paragraph. Code stages get the dangerous permissions
  (`Edit`, `Write`, `Bash`), confined to the per-run sandbox clone.

### Project hooks and project setup

Every project's dev environment is different; MoE doesn't try to know
how. Project-owned hook scripts live under
`projects/<p>/hooks/<event>.d/*`.

- `dev-env.d/*` — each script emits `KEY=VALUE` lines on stdout; the
  merge is sourced into the agent subprocess (and into
  `moe workspace shell`). The contract: hooks set up the working
  tree's environment.
- `dev-env-teardown.d/*` — cleanup at run close.
- `pre-push.d/*` — invocation-time ship gates; the first non-zero
  exit halts the chain and opens a fresh `code` session with the
  failure as kickoff.
- `moe hooks new|code|close` — the slow loop: a journaled run whose
  `code` stage edits the scripts under review.
- `moe hook fire <project> <event>` — the fast loop: a transient
  sandbox that runs one event's scripts end-to-end, no run, no
  journal, no dash row.
- `moe project add|list|remove` — register, inspect, or unregister a
  target project.

### Cross-session machinery

A single operator crossing machines, walking away mid-stage, or
recovering from a crash shouldn't lose work.

- **Auto-sync** runs on every session open and close — pull/rebase
  the bureaucracy, bump project pointers, push. `moe sync` runs the
  same machinery explicitly when you need it.
- `moe session list` — see leftover stage-session worktrees and
  branches.
- `moe session abandon <session>` — drop a session's worktree and
  branch without landing its commits.
- `moe session resolve <session>` — retry the rebase+ff-merge that a
  close failed on.
- `moe where` — print the resolved bureaucracy path; handy in
  scripts and from inside a sandbox clone where `$PWD` isn't
  obviously inside the bureaucracy.
- Every commit carries `MoE-*` trailers so the journal is grep-able
  with `git log`, and `git revert <sha>` undoes a turn cleanly.

## Prompt assembly

Alongside the operator's message, the agent receives an assembled
instruction preamble. MoE composes that preamble fresh for every turn
by concatenating a fixed set of markdown fragments, in order. Each
fragment answers a different question for the agent.

- **Philosophy and quality bar.** A single workflow-agnostic file
  ([`soul.md`](soul.md)) — how to make tradeoffs, when to push back,
  what "done" means. Same content for every turn in every workflow.
- **Where you are in the workflow.** A generated header naming the
  current stage, the stages before and after, and the exact
  `moe …` invocation the operator will be offered next if this turn
  closes cleanly. Lets the stage's lens (next bullet) stay on-topic
  and not repeat the location.
- **The stage's lens.** A workflow-and-stage-specific file from
  [`workflows/`](workflows/) (e.g. `workflows/sdlc/design.md`) — what
  the agent should be *doing* at this phase, what counts as ready
  to hand back, what to avoid here specifically.
- **The project's intent.** When the project has a digital twin (a
  small set of intent documents — see [Feedback paths](#feedback-paths)
  below), a reference block points the agent at the directory so it
  reads recorded intent before touching code.
- **Portable cross-project facts.** A one-line catalog of the
  bureaucracy's `lore/` entries — facts discovered on past runs of
  other projects that might apply here. The full bodies stay on disk;
  the agent opens an entry only when its "applies when" hint matches
  the task. Lore is also described in
  [Feedback paths](#feedback-paths).
- **Where to leave traces.** A short cue naming this run's
  `followups.md` file and pointing at the skill that knows how to
  write to it (see [Skills](#skills) below).
- **What this specific turn is doing.** Run id, the canvas file the
  agent is editing, the project clone the agent can change, what is
  read-only.
- **Project-specific rules.** The project's own `AGENTS.md` (or
  `CLAUDE.md`) is mentioned by path. Because the agent's working
  directory is the bureaucracy and not the project, the agent
  backend's own discovery rules wouldn't otherwise find these files;
  the prompt closes that gap.
- **What moved since last time.** When an earlier-stage document
  (the design, say, for a code-stage turn) has been re-committed
  since this stage last ran, a banner names the document and the
  previous commit so the agent can diff and re-read what changed.

Every piece above is a plain markdown file. Fixing how the agent
behaves usually means editing a fragment, not the Go code.

## Skills

Claude Code and Codex both support *skills* — small named markdown
files the agent can choose to load when a situation matches their
description. The backend handles the loading; the operator doesn't
have to bake the content into the prompt. That makes skills a second
channel, parallel to the assembled preamble above, for shipping
reusable know-how to the agent.

MoE ships one skill today, `moe-bureaucracy`, and uses it as the
agent's interface for *leaving traces* for future runs — twin
observations, portable lore, and followups (all described in
[Feedback paths](#feedback-paths) below). The skill's markdown is
embedded in the `moe` binary; when MoE opens a session, it writes
the file into the session's `.claude/skills/` and `.codex/skills/`
trees with per-run paths already substituted in. The agent reads the
skill's description, sees when it applies, and invokes it without
further prompting.

## Feedback paths

MoE's compounding property — the "project memory that improves
itself" pitch from the intro — comes from a small set of durable
feedback channels. Three improve the project being worked on; one
improves MoE itself.

- **Followups — work noticed but not done.** During any run an agent
  may spot something worth doing that's out of scope for the current
  stage. Instead of acting on it, the agent appends a one-line entry
  (slug, title, optional context) to that run's `followups.md` via
  the `moe-bureaucracy` skill. When the operator closes the run,
  they triage the list; surviving entries open as new `idea` runs
  with the original context carried into the seed canvas. The next
  time an agent works in this area, the captured intent is part of
  its starting context rather than lost in chat history.

- **Lore — portable facts across projects.** Some operational facts
  apply to more than one project — for example, "this kind of
  sandbox requires that kind of proxy." MoE keeps a bureaucracy-wide
  `lore/` directory (a sibling of `projects/`) where each such fact
  lives in its own short markdown file with a `title:` and an
  `applies-when:` heuristic in its frontmatter. On every stage that
  touches the wiki layer, MoE injects a one-line catalog of these
  entries into the prompt; the full bodies stay on disk, and the
  agent opens an entry only when the heuristic matches the task at
  hand. Agents propose new entries via the `moe-bureaucracy` skill
  and the operator merges them at close. The next project to hit the
  same problem starts with the answer already on hand.

- **Digital twin — recorded project intent.** Each project has a
  `digital-twin/` directory of short intent documents (vision,
  architecture, patterns, operations, roadmap, glossary) — the *why*
  and *shape* of the project, separate from the code that
  implements it. Every project stage's prompt includes a reference
  to this directory so the agent reads intent before code. When an
  agent notices that the recorded intent and the actual code
  disagree (`patterns.md` says X, but the implementation does Y),
  it writes the observation into a per-run feedback file rather
  than silently picking a side. A later `moe twin reflect` pass
  walks those observations into proposed edits to the twin
  documents themselves. The rule is: the twin is the intent, the
  code is the implementation, and when they disagree, the twin
  wins until someone updates it.

- **Meta-moe — feedback to MoE itself.** The same compounding
  shape, pointed at MoE's own development. The `meta-moe` workflow
  walks one project's run history and produces a maintainer-facing
  report — repeated work the operator had to redo, corrections
  given more than once across runs, agent-authored suggestions for
  the harness itself. It is how the harness eats its own dogfood:
  a project that uses MoE generates the evidence that improves MoE
  the next time it is built.

Each channel turns one run's discovery into the next run's default
context, for both the operator and the agent.

## Status

Pre-1.0 and under active development. The command surface, file
layout, and commit-trailer conventions are subject to change. If
you're reading this because you're considering trying it — welcome,
but expect sharp edges.

## Contributing

Don't :-) Not accepting issues or PRs right now. This is one firm's
internal tool, shared in case it's useful.

## License

MIT. See [`LICENSE`](LICENSE).

## References

- [Module Collective: Building a Ministry of Everything](https://www.modulecollective.com/posts/building-a-ministry-of-everything/)
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html)
- [Karpathy: LLM Wiki gist](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
