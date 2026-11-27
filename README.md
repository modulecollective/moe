# ▓▒░ MINISTRY OF EVERYTHING ░▒▓

Ministry of Everything (MoE) is a command-line tool for getting finished work
out of coding agents. You describe a change. [Claude
Code](https://claude.com/claude-code) or [Codex](https://chatgpt.com/codex/)
designs it, builds it, reviews it and ships it. Everything they wrote along the
way is saved in a Git repository you own.

Coding agents are good at one task in one sitting. The trouble is everything
around the sitting. The design lives in one chat's scrollback and the test
results in another. Close the terminal and the context goes with it. Run five
agents at once and you become the person who has to remember what each one was
doing.

We think the fix is to keep the work in files, not in chats. MoE is built on
three ideas:

1. **Every stage leaves a document.** A change moves through four stages:
   design, build, review, push. Each one writes a short markdown file, called a
   canvas, that the next stage reads. Nobody replays a chat to find out what
   was decided.
2. **Everything is committed.** Each turn lands in a private Git repository
   called the bureaucracy. You can stop, come back next week, see what
   happened, and undo it.
3. **You choose how far it goes alone.** Run one stage and look at the result.
   Or tell a run to ship itself. Or leave the server on overnight and let it
   work through the ideas you marked as ready, one at a time.

MoE is for one person. There are no accounts and nothing is shared; [One
operator, on purpose](#one-operator-on-purpose) says why. It is also built with
itself: every change to MoE ships through these same stages, and most of them
now start on their own.

## A Quick Taste

Write the idea down when it occurs to you, then turn it into a run:

```sh
moe idea new my-project/add-batch-support              # jot it down
moe sdlc new --from-idea my-project/add-batch-support  # promote it to a run
```

Promoting the idea offers to start the design stage: one conversation with the
agent that turns your note into a plan you can review. When it ends, MoE asks
what to do next. Type `!!` and the run builds, reviews and ships itself without
you, each stage reading the canvas the last one wrote and each turn committed.
Type `!` instead to run just the next stage and stop there for a look. The full
set of answers is in [docs/workflows.md](docs/workflows.md#sdlc).

## Leaving It Running

Agents have made careful work cheap. When a designed, reviewed, tested change
costs one conversation and two keystrokes, the discipline that used to be
overhead becomes the default path. What still costs is coordination: opening
work, handing context forward, checking progress, writing down the lessons
that should shape the next run. So coordination is what MoE automates.

Run `moe serve --dynamic` and leave it. Every few minutes it looks across your
projects for something ready to do: a recurring chore that came due, an idea
you tagged, a run waiting for its next stage. It picks one, carries it through
build, review and ship, and then looks again. Starting the server is your
consent; you can pause it for a day, or switch a project to start only what you
marked.

That is where the economics turn. Shape a few runs during the day, tag the
ideas you want done, and step away. The server works through them one at a
time on capacity your flat-rate subscription already pays for, and each run is
still gated, committed and revertible in the morning. When a run needs your
judgment, send it a sentence with `moe input add`, or from your phone through
the web UI, and its next turn acts on it.

## Is It For You?

You might want MoE if you:

- run several agent threads and want to resume them without digging through
  chat history;
- want agents to work from written designs, test evidence and review notes
  rather than one long prompt;
- want follow-up ideas, project direction and lessons learned to feed later
  runs on their own;
- would rather steer by stating direction and reviewing what merges than by
  starting each run yourself;
- want recurring maintenance to show up as ready-to-open work instead of living
  in your memory;
- pay for a flat-rate subscription that sits idle sixteen hours a day;
- prefer explicit CLI commands and Git history to a hosted product.

## One Operator, On Purpose

MoE is deliberately single-operator. There are no accounts, no sharing, and no
multi-operator coordination surface — that is a recorded non-goal, not a
feature that hasn't been built yet.

The reason is what total capture buys. MoE saves *everything*: every
conversation, every turn, every canvas, committed to a private journal. That
total record is what makes work resumable, auditable, and reusable — and it is
only comfortable because the journal is yours alone. Do the same across a team
and you inherit consent, privacy, and signal-to-noise problems that reshape the
whole system: who reads whose transcripts, what gets redacted before capture,
whose judgment a shared twin encodes.

So MoE is social in exactly one direction: between one operator and their
agents. That relationship is what accumulates — canvases, twin, lore, and
backlog are its shared memory — and the harness's whole job is to make that one
relationship compound rather than to coordinate many.

## Install

Requires Go 1.26+ and at least one agent backend on your `PATH`:
[Claude Code](https://claude.com/claude-code) for `claude`, or Codex for
`codex`.

```sh
go install github.com/modulecollective/moe/cmd/moe@latest
```

Then initialize a bureaucracy — the private Git repo where all runs, canvases,
and project registrations live — and register a project:

```sh
mkdir my-bureaucracy && cd my-bureaucracy
moe init
moe project add <repo-url>
```

`moe ui` is the terminal home screen: every run, idea and chore on one board,
with the controls to steer them. MoE uses `claude` unless you choose otherwise;
[docs/reference.md](docs/reference.md#model-stylesheet) covers choosing a
backend and model, and `moe help` lists every command.

## The Workflows

Each workflow is a small ladder of stages; a run is one pass through the
ladder. One line each here — [docs/workflows.md](docs/workflows.md) has the
full treatment.

| Workflow | Stages | Use it for |
| --- | --- | --- |
| [`sdlc`](docs/workflows.md#sdlc) | `design` -> `build` -> `review` -> `push` | designed code changes with a ship gate |
| [`chat`](docs/workflows.md#chat) | one `chat` run, continued across sittings | a read-only thinking partner that reviews the project and grooms the backlog |
| [`idea`](docs/workflows.md#ideas) | one `idea` canvas, edited through verbs | backlog capture before a full run exists |
| [`intent`](docs/workflows.md#intents) | one `intent` canvas, edited through verbs | standing direction and goals for a project |
| [`pulse`](docs/workflows.md#pulse) | `pulse` | a read-only survey of every project that feeds the backlog and picks the next run |

Four bureaucracy-side artifacts have no workflow of their own — project
[hooks](docs/workflows.md#hooks), [chores](docs/workflows.md#chores),
[knowledge topics](docs/workflows.md#knowledge), and the
[digital twin](docs/workflows.md#twin). Edit them by hand, or let an sdlc run
land them: a stage's per-turn commit picks all four up.

## Going Deeper

- [docs/workflows.md](docs/workflows.md) — how to drive each workflow:
  commands, stages, and cascades.
- [docs/concepts.md](docs/concepts.md) — the moving parts: runs and canvases,
  the bureaucracy repo, sandboxes and workspaces, feedback channels, and how
  agents are steered.
- [docs/reference.md](docs/reference.md) — the command catalog, the console and
  server, Codex setup, shell completion, hooks/dev-env/secrets, and cleanup and
  recovery.
- [docs/evolution.md](docs/evolution.md) — how MoE moved from supervised
  sessions to work that finds, orders, and starts itself.
- [docs/un-changelist.md](docs/un-changelist.md) — what removed experiments
  taught MoE about services, workflows, queues, memory, and trust.

## Status

MoE is pre-1.0 and under active development. The command surface, file layout,
and trailer conventions can change. Expect sharp edges.

## Contributing

Please don't contribute :-) Not accepting issues or PRs right now. This is one
firm's internal tool, shared in case it is useful.

## License

MIT. See [`LICENSE`](LICENSE).

## References

- [Module Collective: Building a Ministry of Everything](https://www.modulecollective.com/posts/building-a-ministry-of-everything/)
- [Module Collective: Reflecting on the first 750 runs of the Ministry of Everything](https://www.modulecollective.com/posts/reflecting-on-750-runs-of-moe/)
- [Boris Cherny: Steps of AI Adoption](https://claude.ai/code/artifact/bfdfaef9-bc62-4dfe-ba9e-c58a26c9accf)
- [Salvatore Sanfilippo: Control the ideas, not the code](https://antirez.com/news/169)
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html)
- [Chad Fowler: The Phoenix Architecture](https://aicoding.leaflet.pub/)
- [Andrej Karpathy: LLM Wiki gist](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f)
- [Alasdair Monk: Quality Software](https://x.com/almonk/status/2079461952577802549)
