# Ministry of Everything (MoE)

**A CLI-first agent harness for a single operator working with AI.**

MoE runs [Claude Code](https://claude.com/claude-code) against living
documents. You collaborate with agents through threaded conversations
attached to markdown files; each document compresses its conversation
into a clean artifact that becomes context for the next step. The
harness is domain-agnostic — software development is the first workflow
to open, with knowledge-base and quick-fix workflows alongside.

There is no background worker, no TUI, no dashboard that updates on its
own. Agents act only when you invoke a command. The UX problem is
**prioritization and resumption**, not real-time updates.

![MoE dashboard — open runs and backlog](docs/dash.png)

---

## At a glance

MoE is split across two repos that sit side by side — like `git` and
the repository it operates on:

- **`moe/`** (this repo) — the Go CLI. Stdlib only, shells out to
  `git` and `claude`.
- **`bureaucracy/`** — your private state: projects, runs, documents,
  and the markdown fragments that steer agents. Discovered via a
  `bureaucracy.conf` marker file found by walking up from `$PWD`, or
  via `$MOE_HOME`.

Every turn lands as one commit on the bureaucracy's `main` branch, with
trailers (`MoE-Run`, `MoE-Document`, `MoE-Session`, …) that scope the
journal. Rewinding is `git reset --soft`; reverting is `git revert`.
Git is the checkpoint.

---

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

`moe help` is the source of truth for the command surface.

---

## Workflows

A workflow is a short stage DAG with one canonical document per stage.
The current workflows are:

| Workflow  | Stages                                | For                                  |
|-----------|---------------------------------------|--------------------------------------|
| `sdlc`    | `design` → `code` → `push`            | designed features with a review loop |
| `quick`   | `code` → `push`                       | small fixes that don't need a design |
| `kb`      | `research` → `summarize`              | knowledge-base articles              |

Each stage is a subcommand that opens a Claude Code session on that
stage's document. Each workflow is its own top-level verb — `moe sdlc`,
`moe kb`, `moe quick`, `moe twin`. For example:

```sh
moe sdlc new "add batch support"   # open a new run
moe sdlc design                    # threaded chat on design/content.md
moe sdlc code                      # agent codes inside a sandbox clone
moe sdlc push --pr                 # open a PR against the target repo
```

`moe dash` shows your open runs and backlog. `moe idea` captures
loose ideas without starting a run.

---

## How it works

- **Guidance is markdown, not config.** Agent behavior comes from
  concatenating `soul.md`, `workflows/<wf>/<stage>.md`, and `docs/<slug>.md`
  fragments into a single `--append-system-prompt`. Every agent
  mistake becomes a fragment edit; the next invocation picks it up.
- **Per-run sandbox worktrees.** Code work runs inside a private `git
  worktree` of the target repo at `.moe/clones/<project>/<run>/`,
  linked off the canonical submodule and pre-positioned on a
  `moe/<run-id>` branch. Two runs on the same project get two
  independent working trees and indexes; only the per-run branch is
  shared with the canonical submodule's ref DB.
- **Tool scoping via Claude Code.** Non-code documents get `Read`,
  `Grep`, `WebSearch`, and a scoped `Edit` — the worst a bad turn
  does is write a bad paragraph. The `code` document gets the
  dangerous permissions (`Edit`, `Write`, `Bash`), confined to its
  sandbox worktree. Enforcement is `--allowedTools`, not a custom
  sandbox.
- **Backend is Claude Code headless.** `claude -p` as a subprocess —
  real CLI, real OAuth, one human driver. Scheduled or unattended
  runs must route to the Claude API under Commercial Terms instead.

---

## Status

Pre-1.0 and under active development. The command surface, file
layout, and commit-trailer conventions are subject to change. If
you're reading this because you're considering trying it — welcome,
but expect sharp edges.

---

## References

- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents)
- [OpenAI: Harness Engineering](https://openai.com/index/harness-engineering/)
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html)
- [The Emerging Harness Engineering Playbook](https://www.ignorance.ai/p/the-emerging-harness-engineering)
