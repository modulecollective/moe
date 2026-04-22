# Ministry of Everything (MoE)

**A bureaucracy-themed agent harness for the full lifecycle of anything.**

Module Collective LLC · April 2026

---

## Vision

MoE is a CLI-first agent harness for a single operator managing many
products with agent assistance. The operator collaborates with AI
agents through threaded conversations attached to living documents —
the spec *is* the conversation about the spec, and each document
compresses its conversation into a clean artifact that becomes context
for downstream work. The harness is domain-agnostic; software
development is the first ministry to open. The bureaucracy — journal
on main, guidance as markdown, no orchestrator — is the feature.

**"Please take a number."**

---

## Data Model

### Hierarchy

MoE is split across two independent repos, side-by-side like `git` ↔ a
repository: `moe/` (the CLI, open-source-eligible, Go stdlib) and
`bureaucracy/` (private state, discovered via `$PWD` walk to a
`bureaucracy.conf` marker file, or `$MOE_HOME`).

```
bureaucracy/
├── bureaucracy.conf               # sentinel marker
├── soul.md                        # global agent guidance
├── stages/<stage>.md              # per-stage guidance fragments
├── docs/<slug>.md                 # per-document guidance fragments
├── agents.conf                    # per-doc model + allowedTools routing (INI)
├── projects/<id>/
│   ├── project.json
│   ├── src/                       # target repo, registered as a submodule
│   ├── overrides/                 # project-level guidance (optional)
│   └── runs/<id>/
│       ├── run.json
│       └── documents/<doc>/
│           ├── content.md
│           └── thread.jsonl
└── .moe/                          # per-run sandbox clones, transient state (gitignored)
```

The submodule lives at `projects/<id>/src/`, not directly at
`projects/<id>/`, so siblings (`project.json`, `runs/`, `overrides/`)
can be tracked by the bureaucracy alongside the gitlink. `stages/` and
`docs/` are flat directories of optional markdown fragments; the
naming convention (`<stage>.md`, `<slug>.md`) is the whole contract.

### Journal on main

The bureaucracy is a single-branch journal — no per-run branches. Every
turn lands as one commit on `main` with structured trailers:

```
work: update design

MoE-Run: add-batch-support
MoE-Project: telomere
MoE-Document: design
MoE-Session: 9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0
MoE-Cost: $0.12
```

A run's history is `git log --grep="MoE-Run: <id>"`. A document's
history adds `MoE-Document: <slug>`. Stage progress is derived from
which documents have `work: update <doc>` commits and when they
landed — no separate sign-off state, no sidecar status files. Rewinding
is `git reset --soft`; reverting is `git revert <sha>`. Git is the
checkpoint.

### Documents

A document is a directory — `documents/<doc>/content.md` — plus one
entry in `run.json` storing its Claude Code session id. Each turn on
the document reuses that `--session-id`, so multi-turn continuity is
server-side. An append-only `thread.jsonl` sits alongside for audit;
downstream agents read the compressed `content.md`, not the thread.

`run.json` also carries an auto-refreshed `abstract` field — 2–3
sentences of prose, rewritten by a Sonnet call after each stage turn
and bundled into the same commit as the doc edits. Discovery across
runs is a filesystem walk: `find projects -name run.json | xargs jq -r
.abstract`. The refresh is best-effort; a failed call logs and leaves
the prior abstract in place.

Document slugs are free-form (`spec`, `architecture`, `migration-plan`,
…). Conventionally each stage has a canonical document sharing its
name (`design`, `code`) because that is what upstream-change detection
targets: when `design` has been re-committed since `code`'s last turn,
the next `moe sdlc code` run gets a banner pointing at the diff. The
ripple is operator-driven — agents act only when the operator invokes
them; the banner is a social cue made legible.

### No typed document graph

MoE deliberately does not maintain a typed document graph — no
`document-graph.conf`, no node-type taxonomy, no `depends_on` edges.
The only typed graph is the stage DAG in `internal/stage/stage.go`
(`sdlc`: `design` → `code` → `push`; `kb`: `research` → `summarize`).
Which documents a run has and how they relate is operator judgment,
guided by `docs/<slug>.md` and `stages/<stage>.md` fragments rather
than a parsed schema.

---

## Agent Architecture

### Guidance layer

Agent behavior is controlled by markdown files in the bureaucracy repo,
concatenated at invocation time into a single `--append-system-prompt`
in most-specific-first order:

```
soul.md
  + stages/<stage>.md
  + docs/<slug>.md
  + projects/<id>/overrides/<slug>.md
  + projects/<id>/runs/<run>/overrides/<slug>.md
  + upstream documents (current content.md)
  + current document content.md
```

Fragments are plain markdown — no custom schema, no preprocessing.
Every agent mistake becomes a guidance-fragment edit; the next
invocation picks it up.

### Model and tool routing

`agents.conf` (flat INI) keys per-document `model` and `--allowedTools`
entries. Non-code documents get `Read`, `Grep`, `WebSearch`, and an
`Edit` scoped to their own `content.md` — the worst a bad turn can do
is write a bad paragraph, reverted with `git revert`. The `code`
document gets the dangerous permissions (`Edit`, `Write`, `Bash`),
scoped to the per-run sandbox clone. Enforcement is Claude Code's
`--allowedTools`, not a custom sandbox.

### Backend

Claude Code headless (`claude -p`) is the primary backend, invoked
as a subprocess by operator-initiated commands — real CLI, real OAuth,
one human driver, individual-scale traffic. That matches Anthropic's
Consumer Terms exemption for ordinary, individual usage of Claude Code.
Swapping backends is an `agents.conf` edit — `model` is passed verbatim
to `claude --model` (or a future adapter). The harness is the moat,
not the model.

**Scheduled or unattended runs must route to the Claude API under
Commercial Terms, not Claude Code headless, regardless of cost.** Never
read `~/.claude` auth material from `moe`, reuse Claude Code's OAuth
tokens against the Anthropic API, or pipe Pro/Max credentials through
the Agent SDK — these are patterns Anthropic actively detects and
blocks.

### UX shape

No background worker, no TUI, no live-updating dashboard. Agents act
only when the operator invokes a command; the UX problem is
**prioritization and resumption**, not real-time updates, which keeps
the interface a shell tool. `moe help` is the source of truth for the
command surface.

---

## Git Model

```
main (the only branch — bureaucracy is a journal, not a code repo)
  ├── commit: "Open run telomere/add-batch-support"   trailers: MoE-Run, MoE-Project
  ├── commit: "work: update design"                    trailers: + MoE-Document, MoE-Session
  ├── commit: "work: update code"                      ← code also commits inside the sandbox clone's moe/<id> branch
  └── commit: "push: telomere/add-batch-support"       trailers: + MoE-PR
```

One branch; per-run scoping via commit trailers. `moe sdlc push`
pushes the sandbox's `moe/<run>` branch to the target remote, opens a
PR via `gh pr create` (first push only), and records the outcome as
one commit on main with a `MoE-PR` trailer. The branch model lives
*inside* each target submodule — that is where code review happens via
PRs. The bureaucracy itself is append-only narrative.

### Per-run sandbox clones

Concurrent code work on the same project does not contend, because every
run gets a private copy-on-write clone of the submodule under
`.moe/clones/<project>/<run>/`:

- **macOS:** APFS `clonefile(2)`, pure-Go — O(metadata), no data
  copied, blocks shared until either side writes.
- **Other platforms:** recursive copy fallback.
- **Independent git:** the submodule's gitdir is cloned alongside the
  worktree; `core.worktree` and the clone's gitfile are rewritten so
  the clone is a fully independent git repo. Two runs on the same
  project never touch each other's index, refs, or objects.

`moe sdlc code` runs inside its run's clone on branch `moe/<run>`.
`moe sdlc push` leaves the sandbox in place so the next `moe sdlc code`
can iterate on PR review feedback. The canonical
`projects/<project>/src/` checkout stays passive — MoE only reads from
it to seed clones. Document-only sessions never needed isolation and
continue to write one markdown file under the bureaucracy in parallel.

Docker/SSH wrappers remain a future option for kernel-enforced
isolation layered *on top of* the clone — not a concurrency mechanism.

---

## Implementation

Go stdlib; shell out to `git` and `claude`. No framework, no YAML
dependency, no DAG engine, no web server. Machine state is JSON
(`encoding/json`); flat human config is INI (`bufio.Scanner` +
`strings.Cut`); agent guidance is markdown (concatenated, not parsed).
See source.

---

## References

- [OpenAI: Harness Engineering](https://openai.com/index/harness-engineering/) — Codex team's methodology for agent-first development
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents) — Initializer/coder pattern, progress files, multi-session continuity
- [Gas Town](https://github.com/steveyegge/gastown) — Multi-agent orchestration with Git-backed state (Steve Yegge)
- [OpenClaw](https://github.com/openclaw/openclaw) — Autonomous agent framework (Peter Steinberger); the pattern Anthropic's Feb 2026 Consumer Terms clarification targeted
- [Google Wave](https://en.wikipedia.org/wiki/Google_Wave) — The original "equal parts conversation and document" platform
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html) — Analysis of harness patterns and categories
- [The Emerging Harness Engineering Playbook](https://www.ignorance.ai/p/the-emerging-harness-engineering) — Cross-cutting patterns from OpenAI, Stripe, and OpenClaw
