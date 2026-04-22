# Ministry of Everything (MoE)

**A bureaucracy-themed agent harness for the full lifecycle of anything.**

Module Collective LLC · April 2026

---

## Vision

The Ministry of Everything is a CLI-first agent harness where a human operator collaborates with AI agents through threaded conversations attached to living documents. It manages the full lifecycle of products and projects — from ideation through design, implementation, deployment, and operations — with a small fixed set of lifecycle stages that ripple signed-upstream changes to the agents working downstream.

The core insight: **the document and the conversation about the document are the same thing.** The spec *is* the conversation about the spec. The architecture *is* the threaded discussion that produced it. Documents compress conversations into clean artifacts that become context for downstream work.

The Ministry is designed for a single operator managing multiple products with agent assistance. It is domain-agnostic — software development is the first ministry to open, but the same machinery handles any workflow that produces interconnected documents. The bureaucracy is the feature.

**"Please take a number."**

### Design Principles

1. **The human is the workflow engine.** No orchestrator, no DAG executor, no scheduler. The operator looks at the run, decides what to work on next, and tells `moe` to do it. A small CLI assembles prompts, invokes Claude Code, and tracks state in git. The harness gets out of the way.
2. **The repo is the source of truth.** All state — documents, decisions, conversations, progress — lives in git. Nothing lives in Slack, Google Docs, or people's heads. The bureaucracy repo is the "back office" (workflows, guidance fragments, run history). Target project repos stay clean. The `moe` CLI lives in its own repo — tool and state are separate so the CLI can be open-sourced without leaking private bureaucracy contents.
3. **Agents are participants, not tools.** Agents join conversations and contribute to documents. Guidance for how they behave lives as plain markdown in the bureaucracy repo (global `soul.md` plus per-stage and per-document fragments assembled at invocation time) — no role taxonomy, no handbook hierarchy.
4. **Runs terminate, products persist.** Units of work (runs) have a lifecycle and end. Products are the long-lived entities that accumulate completed work.
5. **Loose recipes, deterministic mechanics.** Stages are a small fixed DAG (`design` → `code` today), each with a canonical document of the same name. Additional documents (knowledge bases, meeting notes, etc.) can be layered on without sign-offs. Within a run, upstream-change detection is deterministic — given per-document work turns on main, what moved since a downstream agent last looked is always derivable from the log.
6. **Model-agnostic.** The harness works with Claude Code, Ollama/Qwen, Codex, or any LLM backend. A small config routes invocations to the right model, keyed by document type or stage. The harness is the moat, not the model.
7. **Minimize entropy.** Every agent mistake becomes a guidance-fragment update. The system improves every time you use it.
8. **One repo, one history, one branch.** All run state and run metadata live on the bureaucracy repo's `main`. There are no per-run branches — the bureaucracy is a journal, not a code repo. Per-run scoping comes from commit trailers (`MoE-Run`, `MoE-Document`, `MoE-Session`, `MoE-PR`); a run's history is `git log --grep="MoE-Run: <id>"`. Stage progress is derived from which documents have `work: update <doc>` commits and when they landed — no separate sign-off state. Code branches live where they belong: inside each target submodule.
9. **Target repos are independent.** No MoE coupling in target repos. Works with any repo — open source forks, client projects, personal code. Target projects are registered as git submodules under `projects/`, but only one is checked out at a time during a session.
10. **Many projects registered, a handful actively worked.** Submodules are cheap to register, so the ceiling on *registered* projects is high. The ceiling on *concurrently active* projects is the operator's review bandwidth — practically ~10-30 — because every run ultimately cashes out in a human reading a diff. MoE helps manage the fan-out, it does not remove it.
11. **Stdlib only, where practical.** The `moe` CLI uses Go stdlib plus `git` and `claude` on PATH. No YAML parser, no CLI framework, no DAG engine, no graph library, no web server dependencies. Two stdlib-native config formats match two audiences: JSON for machine state, INI for flat human config. Markdown for guidance. Humans never see JSON.

### Direction

MoE is a thin CLI wrapper around `claude -p` plus conventional git plumbing. Every feature maps directly to a Claude Code flag, a git operation, or something the human decides in the moment. Automation can grow back later — parallel runs, derived-artifact hooks on merge, a thin web layer reading git state — but it grows out of a minimal, working CLI, not the other way around.

---

## Data Model

### Hierarchy

```
bureaucracy/                       # private state repo, cloned alongside the moe CLI repo
├── bureaucracy.conf               # Sentinel marker — presence identifies this dir as a bureaucracy root
├── soul.md                        # Global agent guidance
├── stages/                        # Per-stage markdown guidance (design.md, code.md, …)
├── docs/                          # Per-document markdown guidance (spec.md, architecture.md, …)
├── projects/                      # Per-project state: registration, run tree, and submodule checkout
│   ├── telomere/
│   │   ├── project.json           # Project metadata
│   │   ├── src/                   # Submodule → github.com/modulecollective/telomere
│   │   ├── overrides/             # Project-level guidance overrides (optional)
│   │   └── runs/
│   │       ├── add-batch-support/ # in_progress
│   │       ├── fix-timeout-bug/   # in_progress
│   │       ├── mvp-build/         # pushed
│   │       └── websocket-eval/    # scrapped
│   └── next-idea/
│       └── …
├── .moe/                          # Per-run sandbox clones + other transient state (gitignored)
└── (single branch: main — per-run scoping via commit trailers)
```

The submodule lives at `projects/<id>/src/`, not directly at `projects/<id>/` — that leaves room for `project.json`, `runs/`, and `overrides/` to be tracked by the bureaucracy git alongside the submodule. A submodule at `projects/<id>/` would be a single gitlink tree entry, preventing any sibling files under the same path.

`stages/` and `docs/` are flat directories of optional markdown fragments. Stage sessions concatenate the applicable ones with `soul.md`, any run-level overrides, and the upstream documents to build the prompt. No role taxonomy, no handbook-per-agent — just guidance keyed to the two axes that actually vary: the lifecycle stage and the kind of document.

### Two Repos: CLI and Bureaucracy

MoE is split across two independent git repos, cloned side-by-side, in the same relationship as `git` ↔ a repository or `hugo` ↔ a site:

- **`moe/`** — the CLI repo. Go source for the `moe` binary. No private data, no pointer to the bureaucracy. Open-source-eligible. Installed to `$PATH` like any other tool.
- **`bureaucracy/`** — the private state repo, cloned at the same level as `moe/`. Holds `soul.md`, per-stage and per-doc guidance, the document graph definition, and `projects/*` — one directory per registered project, each with its `project.json`, run tree, and submodule checkout under `src/`. The `moe` binary operates on whichever bureaucracy directory it's invoked from (discovered via `$PWD` walk or `$MOE_HOME`). The root is identified by a sentinel marker file, `bureaucracy.conf` — its presence is the whole signal; keys inside are reserved for future config. `moe where` prints the resolved root so scripts and the operator can confirm which bureaucracy is in scope.

Upgrading `moe` is a `go install`; the bureaucracy is untouched. Matches principle 11 — the CLI is just a tool on `$PATH`.

### The Bureaucracy Repo

The bureaucracy repo holds guidance fragments, run state, and run history across all projects. There is one unified history — one set of run IDs, one `git log`. Dashboards, portfolio views, and cross-project queries are all views over the same repo.

### Project (Target Repo)

A long-lived entity representing a software product or project. Registered as a git submodule under `projects/<id>/src/`. Born from `moe project add <repo-url>`, persists as long as the project is managed.

**Target project repos are registered as git submodules under `projects/<id>/src/`.** The bureaucracy repo stores all orchestration metadata — guidance, run state, run history — alongside the submodule under `projects/<id>/`. The target repo stores only its own code. This separation means:

- Projects can pre-exist MoE. Fork an interesting open source project, `moe project add` it, and start managing runs against it.
- Target repos stay clean — no MoE files, no framework artifacts. Someone looking at the target repo sees normal, well-crafted commits.
- Hundreds of projects can be registered. Only one submodule is checked out at a time during a session.
- Projects can use whatever structure, language, or conventions make sense for them.

```json
// projects/telomere/project.json
{
  "id": "telomere",
  "status": "incubating",
  "submodule": "projects/telomere/src",
  "remote": "git@github.com:modulecollective/telomere.git",
  "default_branch": "main",
  "deploy_url": "https://telomere.modulecollective.dev",
  "created": "2025-03-15"
}
```

The `id` is derived from the repo URL's last path component and doubles as the project's display name — no separate name/description fields. Fresh registrations land in `"incubating"`; the operator bumps status as the project matures (`"live"`, `"archived"`, etc.). `default_branch` is detected from `git ls-remote --symref HEAD` at registration time. `deploy_url` is optional and omitted when unset.

Code-editing stages run inside a per-run sandbox clone of `projects/<id>/src/`, not the submodule checkout itself. Code changes land as commits on the clone's `moe/<run>` branch; `moe sdlc push` pushes that branch to the target remote and opens a PR via `gh pr create` — the submodule pointer is not updated as part of the push.

**Project-level concerns (stored in the bureaucracy repo under `projects/<id>/`):**

For v1 the only project-level file is `project.json`. A human-maintained `backlog` doc (run ideas, title + paragraph each) is a natural next addition. Longer-lived artifacts — changelog, decision log, architecture overview, API reference, ops runbook — are deferred; they'd accumulate from completed runs once `moe sdlc push` grows side-effects beyond pushing the branch and opening the PR.

### Run

A unit of work against a project. Has a defined lifecycle and terminates. Every run belongs to exactly one project and exactly one workflow (`sdlc`, `kb`, and future `ops`, …); the workflow determines which stages the run moves through.

```json
// projects/telomere/runs/fix-timeout-bug/run.json
{
  "id": "fix-timeout-bug",
  "project": "telomere",
  "title": "Fix timeout bug from overnight alert",
  "status": "in_progress",
  "workflow": "sdlc",
  "created": "2026-04-12",
  "documents": {
    "design": { "session": "9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0" },
    "code":   { "session": "7d2a5e1c-90b3-4c11-a4d2-2e5b1c0a9f33" }
  }
}
```

Run statuses: `in_progress | pushed`. Documents have no status field — they are just files on disk (`content.md`), and a document's history is its commit history. The only per-document data in `run.json` is the Claude Code session id so `moe sdlc design`/`moe sdlc code` can resume the same conversation.

`run.json` also carries an `abstract` field: a 2–3 sentence prose summary of the run's current substance, refreshed by a separate Sonnet call at the end of every stage turn (applies to every workflow, not just `kb`). Discovery across runs is a filesystem walk: `find projects -name run.json | xargs jq -r '.abstract'`. Abstract refresh is best-effort — a failed call logs a warning and leaves the prior abstract in place.

**Stages are a canonical document per phase, not a separate sign-off state.** The run's progress is readable from the git log: a `work: update design` commit means design has been worked on; a later `work: update code` means code has moved past design. Re-running `moe sdlc design` after a `work: update code` is how the operator revises the spec — and the next `moe sdlc code` turn sees an upstream-change banner pointing at the diff. Shipping is a separate verb (`moe sdlc push`) that runs the mechanical outbound work; there is no explicit "sign this stage" ceremony between turns.

Known stages in the `sdlc` workflow:

- `design` — design is settled; `moe sdlc design` edits its canvas
- `code` — code is settled; `moe sdlc code` edits its canvas inside the run's sandbox clone
- `push` — the terminal stage; `moe sdlc push` publishes the code branch and opens a PR

Known stages in the `kb` workflow:

- `research` — `moe kb research` searches the web and extends a source list with 1–3 sentence abstracts; resumed sessions add rather than replace
- `summarize` — `moe kb summarize` synthesizes a Wikipedia-style article from the research doc; signing this stage is publication (there is no push — the artifact is markdown inside the bureaucracy)

Additional stages (review, test, deploy, retro, …) or additional workflows will be added when a real use case forces the question.

**Run progress is driven by the operator, not a rigid phase sequence.** Small bug fixes may only touch `code`. Larger work starts in `design` and moves forward. The staleness gate in `moe sdlc push` refuses to ship if the design has moved after the last code turn — the operator's recourse is another `moe sdlc code` turn to reconcile.

Within a run, documents beyond `design` and `code` are allowed: a knowledge base, meeting notes, a rollback plan. These don't participate in the stage DAG — they're just files with a session and a commit history, edited with the generic document-work flow when and if it's reintroduced.

**Run rollup on completion:**

When `moe sdlc push <project> <run>` runs:
- Preconditions: `code/content.md` is non-empty and has a `work: update code` commit; `design/content.md` has not been committed after the last code turn (staleness gate); the run's sandbox clone has branch `moe/<run>` ahead of the target's default branch.
- `moe sdlc push` repoints the clone's `origin` at the target project's remote, pushes `moe/<run>`, and opens a PR via `gh pr create` using `code/content.md` as the body (first push only; reruns detect the existing PR and skip this step).
- On the first successful push, `run.json` flips to `pushed` and the change is committed on main with `MoE-PR: <url>` in the trailers. Re-runs just push new commits; the sandbox stays in place so `moe sdlc code` can iterate on review feedback.

When `moe scrap <run> "reason"` runs (future):
- The reason is recorded in a `MoE-Scrapped` commit trailer on main
- Run remains in the product's history as institutional memory
- No code or config changes are applied

### Derived Artifacts

Deferred. `moe sdlc push` today flips the run status, pushes the branch, and opens the PR. Generating release notes, decision summaries, or updating long-lived project docs from completed runs is a future phase — when it arrives, it'll run as additional commits on main from inside `moe sdlc push` or an adjacent verb, not as a background worker.

### Document

The primary unit of work within a run. A document is both a **living artifact** (the spec, the architecture design, the test plan) and a **conversation space** where you collaborate with agents to produce and refine it.

Documents serve two critical functions:

1. **Workspace**: You converse with the assigned agent, explore options, make decisions. The conversation is verbose and exploratory — this is where the thinking happens.
2. **Compressed artifact**: The agent distills the conversation into a clean, approved document. This document becomes **context for downstream work** — future agents read the approved document, not the conversation. This is how Ministry manages context windows efficiently.

```
Conversation (verbose, exploratory)
    ↓ agent drafts / human reviews
Document content (compressed, authoritative)
    ↓ fed as context to downstream agents
Next document's agent starts with clean, dense input
```

**Document lifecycle — trailers, not branches:**

All commits land directly on `main`. There are no per-run branches. Each commit carries structured trailers so any one run's history can be reconstructed programmatically:

```
work: update spec

MoE-Run: add-batch-support
MoE-Project: telomere
MoE-Document: spec
MoE-Session: 9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0
```

A run's full audit trail is `git log --grep="MoE-Run: add-batch-support"`. A document's history is the same plus `MoE-Document: spec`. The atomic-review surface lost by skipping a feature branch is reconstructed by `moe review`, which prints the run's commits and the current state of each document.

```
main (the only branch)
  ├── commit: "Open run telomere/add-batch-support: …"
  ├── commit: "work: update design"
  ├── commit: "work: update design"
  ├── commit: "work: update code"
  ├── commit: "work: update code"
  └── commit: "push: telomere/add-batch-support"     (MoE-PR: <url>, status→approved)
```

Each document stage has a canonical slug (`design`, `code`) and each turn lands as a `work: update <slug>` commit with the run's trailers. `moe sdlc push` is the terminal action — it pushes the sandbox branch, opens a PR, and records the outcome as a single commit on main. No separate sign-off state; the journal itself is the record.

**The ripple is operator-driven, not background.** Agents only act when `moe sdlc design` or `moe sdlc code` is invoked. "Rippling through documents" means the operator walking the graph — typically via `moe next`, which dispatches the obvious action for each attention-triggering doc. There is no background worker; there is a human pressing Enter through an attention queue.

- **In progress**: The operator is running `moe sdlc design` and `moe sdlc code`. Each turn appends a trailer-tagged commit to main.
- **Ready to push**: code has moved past design, nothing in the design doc has moved since. `moe dash` surfaces this as NEEDS ATTENTION; `moe sdlc push` refuses to ship otherwise.
- **Approved**: `moe sdlc push` flipped the run status, opened the PR, and recorded a `MoE-PR:` trailer. The sandbox stays in place so `moe sdlc code` can iterate on review feedback and `moe sdlc push` again updates the branch.

Human review is required by default at every `moe sdlc design` / `moe sdlc code` turn and before `moe sdlc push`. A future **yolo mode** can collapse those into one operator command — `moe yolo <proj> <req> --through <stage>` — which walks the document graph autonomously, drafting every document the run needs, then carrying the work up to `<stage>` without pausing for input. Yolo is the right-hand end of a spectrum: operator watches the shell, Ctrl-C is always the off switch, every step still lands as trailer-tagged commits on main so `git revert` unwinds cleanly.

Note on vocabulary: this MoE-level "yolo" (multi-stage autonomy above the document line) is orthogonal to Claude Code's own `--dangerously-skip-permissions` (tool-call autonomy below the document line, *inside* a single `claude -p` session). Yolo mode generally wants both: skip permissions inside each session, and skip the operator-in-the-loop between sessions.

**Ripple flow in practice:**

1. `moe sdlc new telomere "Add batch support"` — opens the run, commits the scaffolding to main.
2. `moe sdlc design telomere add-batch-support` — you collaborate with Claude on the design canvas; each turn commits on main with trailers.
3. `moe sdlc code telomere add-batch-support` — Claude edits code inside the run's sandbox clone; turns commit on main (for the document) and on `moe/add-batch-support` (for the code).
4. The code session flags a concern about the design — you carry it back into `moe sdlc design …` with "the code session says this interface needs pagination, reconsider". A couple more design turns.
5. Next `moe sdlc code` turn fires the upstream-change banner; Claude reconciles and commits again.
6. Everything settles. `moe review telomere add-batch-support` — you read the run's commits and current document state.
7. `moe sdlc push telomere add-batch-support` — sandbox branch pushed, PR opened, run flipped to `pushed`. Done. Re-run `moe sdlc code` and `moe sdlc push` if review feedback requires changes.

For well-scoped runs with good guidance files, this converges quickly. You start a conversation, the agents ripple through the documents you drive them through, and you come back to a complete package. One conversation, one review, one push.

Recovery and rewriting are also branchless. Rollback is `git revert <sha>`. Per-run history is `git log --grep`. Rewinding a stuck conversation is `git reset --soft` — the same regardless of which run you're working on.

**Document slugs are free-form.** `spec`, `architecture`, `implementation`, `security-review`, `migration-plan` — whatever fits. MoE doesn't maintain a typed library; the operator picks a slug, `moe work` creates the directory on first use, and per-doc guidance in `docs/<slug>.md` (if present) gets concatenated into the prompt. Conventionally each **stage** has a canonical document sharing its name — `design`'s work lives in a `design` document, `code`'s in a `code` document — because that's what `upstreamChangeBanner` uses to point downstream agents at what moved.

Each document is just a directory on disk — `documents/<name>/content.md` — and an entry in the parent `run.json` carrying the Claude Code session id. The document's "state" is its content and its commit history; there is no sidecar status file.

**Ripple mechanism:**

When a prerequisite document has been committed after a downstream document's last work turn, the next run of the downstream doc's verb (e.g. `moe sdlc code` after `moe sdlc design`) gets an upstream-change banner in its system prompt: the prereq doc name, the path to its content.md, the bureaucracy SHA the agent last ran on, and the exact `git diff` command to see what moved. The banner is advisory — the contract with the agent is still social, but the social cue is legible instead of implicit.

Mechanics:

- Detection is deterministic: compare each prereq doc's most recent `work: update <doc>` commit time against the downstream doc's most recent work-turn commit (both filtered by `MoE-Run: <id>`). Nothing is mutated in the background.
- First-turn sessions get no banner — there's no "since" to compute against.
- `design` has no prerequisites, so the design canvas never sees a banner. `code` sees it when `design` has been re-committed since its last turn.
- `moe sdlc push` turns the same signal into a hard gate: if design moved after the last code turn, push refuses until `moe sdlc code` has been re-run to reconcile.

The operator controls the pace. A touched `design` doesn't force the `code` canvas to pick it up immediately; the banner fires on the next `moe sdlc code` run, whenever that happens.

**Cross-document agent collaboration:**

A downstream agent reviewing an upstream change can flag concerns, which the operator carries back into the upstream document's conversation. For example: while working on the architecture, Claude reads a spec change and says "A batch size of 10,000 will require a completely different storage architecture — can we cap at 1,000?" — you copy that pushback into the spec thread (`moe work telomere add-batch spec`) with "the architecture session flagged …", Claude responds on the spec, and you review the result. The operator is the messenger; the sessions don't talk to each other directly.

This turns the stages into a genuine collaborative workspace rather than a one-way waterfall. Upstream docs get better because downstream agents pressure-test them — the same way a good implementation engineer pushes back on a design that won't hold up. Your role as operator is to arbitrate and mediate, which you'd do anyway.

**Ripple summary view:**

`moe review` (planned) synthesizes the per-run view by filtering the main-branch log on the run's trailer:

- The run's commits in order, grouped by document.
- Current `content.md` of each document at HEAD.
- Any stages where the most recent `MoE-Stage-Signed` commit post-dates a downstream document's last turn — i.e., work the banner will fire on next time that document is opened.

This is the intended primary review surface. One synthesized view, organized so you can see the coherent picture across all documents. When it looks good, `moe sign code`.

**What isn't here:**

MoE does not maintain a typed document graph — no `document-graph.conf`, no node-type taxonomy (`conversational`/`derived`/`persistent`), no `depends_on` edges between documents. The only typed graph in the system is the stage DAG in `internal/stage/stage.go` (`design` → `code`). Everything else — which documents a run should have, how they relate, which ones imply others — is operator judgment at run-open time, guided by conventions in `soul.md` and `docs/<slug>.md` fragments rather than by a parsed schema.

This is a deliberate shrink. Earlier drafts speculated a parts bin of 15+ node types with dependency edges, derived artifacts generated at approval, and product-level persistent documents updated on every sign. None of it was built, and every instance I could imagine was better served by the operator writing whatever slug they wanted and the agent reading whatever upstream document they named. When a real need for typed dependencies between documents shows up, it can come back; until then it's speculative generality the `soul.md` warns against.

### Thread

A conversation within a document. The primary mechanism is **Claude Code's native session**: each document maps to one `--session-id`, and multi-turn continuity is server-side. `moe work telomere add-batch spec` invoked a second time resumes that session — the agent remembers the prior turns.

For auditability, each `moe work` invocation appends a JSONL record to the document's local thread log:

```jsonl
{"id": "blip-001", "author": "james", "type": "human", "ts": "…", "content": "We need to support batch operations for the timeout API. Users are calling it in a loop and hitting rate limits."}
{"id": "blip-002", "author": "agent", "type": "agent", "ts": "…", "content": "That makes sense. I'd suggest a POST /batch endpoint that accepts an array of timeout configs. A few questions: should batch operations be atomic (all-or-nothing) or partial (best-effort)?"}
{"id": "blip-003", "author": "james", "type": "human", "ts": "…", "parent": "blip-002", "content": "Partial. Return individual results for each item."}
{"id": "blip-004", "author": "agent", "type": "agent", "ts": "…", "content": "Got it. I've updated the spec document with the batch endpoint definition, partial semantics, and new acceptance criteria.", "document_version": 3}
```

Stored at `projects/<project>/runs/<run>/documents/<doc>/thread.jsonl`. Append-only. JSONL because it's the most stdlib-friendly streaming format: `encoding/json` + `bufio.Scanner`.

The thread is the exploratory space. The document content is the compressed output. Both are preserved, but downstream agents consume the document, not the thread. The thread is there for auditability — understanding *why* the spec says what it says.

### Blip

An individual contribution to a thread. Can be:
- **Message**: Text from human or agent
- **Artifact reference**: Link to code diff, test result, deploy log, etc.
- **Decision**: Explicit record of a choice made (with rationale)
- **Status update**: Document version published, approval, or escalation
- **Ripple notification**: Upstream document changed, reconciliation needed

Blips are append-only. Replies create sub-threads via the `parent` field, but blips can't be edited or deleted. This preserves the full decision history.

### Context Flow

The document model and guidance layer together create a natural compression pipeline for agent context:

```
Run: "Add batch support"

Working on the spec:
  - Guidance: soul.md + stages/design.md + docs/spec.md + project overrides
  - Run description (lightweight)
  - Product-level context (project.json, existing architecture doc)

Working on the architecture:
  - Guidance: soul.md + stages/design.md + docs/architecture.md + project overrides
  - Upstream Spec document (current content at HEAD; tagged "design-signed" if the design stage is signed)
  - Product-level context
  - Existing codebase structure from product repo

Working on the implementation:
  - Guidance: soul.md + stages/design.md (until code stage) + docs/implementation.md + project overrides
  - Upstream Architecture + Spec documents (current content, stage-tagged)
  - Relevant source files from product repo

Working on the test plan:
  - Guidance: soul.md + stages/design.md + docs/test-plan.md + project overrides
  - Upstream Spec + Architecture (stage-tagged)
  - Code diffs from implementation
  - Test results
```

Each agent gets guidance files plus dense, current documents rather than raw conversation history. Upstream content is always the document at HEAD, tagged with whether the relevant lifecycle stage has been signed (e.g., downstream implementation agents see the spec tagged "design-signed" once the operator has signed the `design` stage). This keeps context windows focused and signal-rich. Thread logs are available for human review but are not fed to downstream agents unless explicitly requested.

**Session-expiry fallback.** Multi-turn continuity relies on Claude Code's server-side session (keyed by `--session-id`). If a resume returns an empty or missing session (rotation, expiry, or a different machine), `moe work` falls back to injecting the last N turns from `thread.jsonl` as a compact recap in the user message, then continues. The audit log is the durable record; the server session is an optimization.

---

## Agent Architecture

### Agent Guidance Layer

Agent behavior is controlled by **markdown files in the repo** — the same files agents work with, reviewed and versioned the same way. This is the harness. Three layers, most specific wins:

```
bureaucracy/
├── soul.md                        # Global: philosophy, quality bar, tone
├── stages/                        # Per-stage guidance (what the work is like at this checkpoint)
│   ├── design.md                  # "At the design stage, explore tradeoffs; resist over-specifying."
│   └── code.md                    # "Before PR, verify tests, rationale captured, scope disciplined."
├── docs/                          # Per-document guidance (what good looks like for this kind of doc)
│   ├── spec.md                    # "Specs name the problem, the acceptance criteria, and the non-goals."
│   ├── architecture.md            # "Architecture docs explain why, not just what. Include tradeoffs."
│   └── implementation.md          # "Keep the task list short. One change per commit."
├── agents.conf                    # Model + allowedTools routing, keyed by document (and/or stage)
├── projects/
│   ├── telomere/
│   │   ├── project.json           # Project metadata
│   │   ├── src/                   # Submodule checkout
│   │   ├── overrides/             # Project overrides (optional)
│   │   │   └── implementation.md  # "Use Deno, not Node. Deploy to Fly.io."
│   │   └── runs/
│   │       └── add-batch-support/
│   │           └── overrides/     # Run overrides (rare, optional)
│   │               └── implementation.md   # "This run touches billing, extra caution"
```

**Layer resolution:** run-level → project-level → global. Same pattern as gitconfig, CSS specificity, or Consul's config hierarchy. Stage sessions concatenate whichever override files exist in most-specific-first order.

**`soul.md`** is the equivalent of Codex's AGENTS.md or Claude Code's CLAUDE.md. It's the general guidance every invocation gets: your engineering philosophy, how you like things communicated, quality standards, what to escalate vs. decide autonomously. This is the document that captures your engineering judgment in a form agents can consume.

**Stage fragments** (`stages/<stage>.md`) shape behavior by *where* the work sits in the lifecycle. `stages/design.md` emphasizes exploration and tradeoff-surfacing; `stages/code.md` emphasizes rigor and scope discipline on the way to a landable diff. A fragment applies when the run has entered that stage but not yet signed the next one.

**Document fragments** (`docs/<doc>.md`) shape behavior by *what* is being written. `docs/spec.md` says what a good spec looks like; `docs/architecture.md` says what a good architecture document looks like. These are the closest thing we keep to role definitions — and they're indexed by the artifact, not by a role taxonomy.

**Project and run overrides** let you tailor behavior locally. A prototype might have relaxed standards. A fork of a safety-critical system might have stricter review requirements.

Because these are markdown files in git, they evolve through the same mechanism as everything else. An agent makes a recurring mistake → you update the relevant fragment → that mistake doesn't happen again. **The harness improves every time you use it.** Agents can even propose updates to their own guidance fragments (reviewed as a diff, just like any other document change).

### Agent Context Assembly

When the operator runs `moe work <project> <run> <document>`, the CLI assembles the invocation's context from the guidance layer plus the document model:

```
Context = soul.md
        + stages/<current-stage>.md   (the earliest unsigned active stage)
        + docs/<document>.md          (if present)
        + project overrides           (if any)
        + run overrides               (if any)
        + upstream documents          (current content, tagged with stage-signed state)
        + current document content    (what the invocation is working on)
```

The assembly is a string-concatenation of markdown files injected via `--append-system-prompt`. Multi-turn conversation history comes from Claude Code's session store (keyed by `--session-id`), not from replaying a JSONL thread — the session store is the primary record.

This is the full context engineering pipeline. Dense, relevant, layered. No stale Slack threads, no giant monolithic instruction file, no raw conversation dumps from upstream phases.

### Model and Tool Routing

Model selection and allowed-tools are keyed by document (and optionally stage) in a single flat config — `agents.conf` — so the operator can say "implementation docs get Bash and Edit; spec and architecture docs get read-only browsing." Exact schema TBD; the guiding rule is one small file rather than a taxonomy of roles.

```ini
# agents.conf (sketch — subject to change)

[doc:spec]
model = claude-opus-4-6
tools = Read,Grep,Glob,WebSearch

[doc:architecture]
model = claude-opus-4-6
tools = Read,Grep,Glob,WebSearch

[doc:implementation]
model = claude-opus-4-6
tools = Read,Grep,Glob,WebSearch,Edit,Write,Bash
```

Documents without an explicit section fall back to a `[default]` block. Cost tracking is per-invocation — each `moe work` run records the session's cost (from Claude Code's output) in a sidecar file or commit trailer; `moe history` aggregates.

Edits to either file show up immediately in the next `moe work` invocation — no rebuild, no restart.

### Session Continuity & Coordination State

Following Anthropic's harness research, each run maintains:

- **`run.json`** (run root): Tracks which documents exist and their Claude Code session ids. That's all — no document statuses; a document's state is its content plus its commit history.
- **`features.json`** (inside implementation document directory): Implementation-specific session continuity. Detailed feature list with completion status, test coverage, and quality grades. Lives with the implementation doc because it's only relevant to code-editing work.
- **Claude Code sessions**: The `--session-id` keeps multi-turn conversation state server-side. Resumption is transparent.
- **Git history**: The ultimate source of truth for what changed and when.

Most documents get continuity from their Claude Code session and their own `content.md` — they pick up where they left off by reading their output. Implementation work additionally needs `features.json` because it involves complex multi-step work spanning sessions (implementing features, running tests, fixing failures).

The first implementation session in a run uses an **initializer prompt** that sets up the environment, writes the initial feature files, and makes the first commit. Subsequent sessions use the **continuation prompt** that reads progress, picks up where the last session left off, and makes incremental progress.

### Model Agnosticism

Swap a model per document type by editing `agents.conf`. The `model` field is passed verbatim to `claude --model` (or to whatever CLI the backend exposes). A future backend abstraction could route `ollama:qwen3` to a local Ollama invocation instead of Claude Code, but for v1, Claude Code is the backend.

### Claude Code Headless as the Primary Backend

**Claude Code in headless mode (`claude -p`)** is MoE's primary agent backend. It is invoked by `moe work` as a subprocess for **operator-initiated, ordinary-individual-usage runs** — a human at a keyboard kicking off `moe work` under their own Claude Code install.

**Compliance boundary (read this before changing the backend logic).** Anthropic's [Claude Code Legal and Compliance page](https://code.claude.com/docs/en/legal-and-compliance) (clarified Feb 19, 2026, server-side enforcement fully live April 4, 2026 at 12:00 PT) draws a bright line around Claude Code + Pro/Max subscriptions. The good news sits right at the top of the page: *"Claude Code CLI on your own computer works as it always has — it's Anthropic's official product built for scripted and automated use, and the Consumer ToS exempts it from the prohibition on automated access."* The binding limit is the phrase *"advertised usage limits for Pro and Max plans assume ordinary, individual usage of Claude Code and the Agent SDK."*

- ✅ **Allowed:** Spawning the real `claude` CLI as a subprocess from your own script, with OAuth flowing through Claude Code normally, on your own machine, at a volume consistent with one human working. Autonomous behavior *within* a session (including `--dangerously-skip-permissions` and long-running loops that the operator kicked off and is watching) is inside the exemption — the TOS line is about who's running it, not whether Claude presses its own buttons.
- ❌ **Banned:** Extracting OAuth tokens from Claude Code and replaying them against `api.anthropic.com`. Using Pro/Max OAuth tokens with the Agent SDK (the Agent SDK requires API key authentication as of Feb 2026). Driving Claude Code under a subscription from scheduled jobs, always-on services, multi-tenant deployments, or anything that looks like production automation rather than "ordinary, individual usage." Third-party "harnesses" that wrap or impersonate Claude Code under a subscription (the OpenClaw / OpenCode / Roo Code / Goose pattern Anthropic actively blocked).

MoE's position: `moe work` spawning `claude -p` from an operator-initiated command is in the allowed bucket — real CLI, real OAuth, no token extraction, single human driver, individual-scale traffic. **Any future workflow that runs without a human at the other end — scheduled triggers, shared services, multi-tenant hosting — must route to the Claude API backend under Commercial Terms instead, regardless of cost.** See Review Notes for the routing rules.

> ⚠️ **Never** read `~/.claude` auth material from `moe`, re-use Claude Code's OAuth tokens against the API, or pipe Pro/Max credentials through the Anthropic SDK. These are the patterns Anthropic detects and blocks — do not cross this line to "optimize" anything.

`moe work` invokes Claude Code with assembled context:

```bash
claude -p \
  --session-id "moe/telomere/add-batch-support/spec" \
  --append-system-prompt "$(cat assembled-guidance.md)" \
  --allowedTools "Read,Grep,WebSearch,Edit(projects/telomere/runs/add-batch-support/documents/spec/content.md)" \
  --output-format stream-json \
  --verbose \
  --model claude-opus-4-6 \
  < prompt.md
```

**Key mechanics:**

- **Multi-turn sessions via `--session-id`**: Each document maps to a Claude Code session keyed by a per-document UUID stored in `run.json` (Claude Code requires UUIDs as session ids; MoE generates one on first use). Context is preserved server-side across turns. The operator posts a new turn via `moe work`, the agent responds, the CLI appends the response to the thread log and commits any document changes on `main`.
- **Context injection via `--append-system-prompt`**: `moe work` assembles guidance (soul.md + applicable stage fragment + applicable doc fragment + project/run overrides) and upstream documents (tagged with stage-signed state), then injects them as the system prompt.
- **Streaming JSON output**: Responses stream back as newline-delimited JSON, which `moe` parses with `json.NewDecoder` over stdout and displays to the operator in real time.

**Permission model — principle of least privilege per document:**

Each document type gets exactly the permissions it needs, enforced natively by Claude Code's `--allowedTools` flag. Most document types get a tight, safe sandbox. Only the implementation document gets the scary permissions.

| Document | Read bureaucracy | Web search | Edit own content.md | Read target project | Write target project | Shell exec |
|-----------|---------------|------------|---------------------|--------------------|--------------------|------------|
| spec | ✓ | ✓ | ✓ | | | |
| architecture | ✓ | ✓ | ✓ | | | |
| security-review | ✓ | ✓ | ✓ | ✓ (code audit) | | |
| test-plan | ✓ | ✓ | ✓ | ✓ | | |
| implementation | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| deploy-plan | ✓ | ✓ | ✓ | | | scoped to deploy scripts |

The vast majority of the Ministry's work happens in the top rows — invocations that can read everything but only write to one markdown file. The worst a misbehaving document-editing session can do is write a bad paragraph in one document; `git revert <sha>` undoes it cleanly. The dangerous permissions are concentrated in one document type, which can have stricter review requirements (implementation output always lands under a human gate at `moe review`).

The code-editing document is **optional**. MoE works as a document-only planning tool — spec, architecture, test plan — with manual implementation. Coding automation is an upgrade you add when ready.

For additional isolation of code-editing sessions (Docker, Daytona, SSH hosts), wrap the `claude -p` invocation with `docker run` or `ssh`. Not needed for v1 — stdlib `os/exec` handles all of these uniformly.

---

## CLI

### Commands

```
moe                                           # print usage + a hint: "try 'moe dash'"
moe dash                                      # dashboard — the home screen
moe next                                      # triage mode — do the top attention item, then the next
moe init [--remote <url>] [dir]               # scaffold a new MoE workspace (stages but does not commit; prompts interactively)
moe where                                     # print the resolved bureaucracy root ($MOE_HOME or $PWD walk)
moe project add <repo-url>                    # register a target repo as submodule under projects/<id>/src/
moe project remove <id>                       # unregister a project (refuses if projects/<id>/runs/ holds runs)
moe project list                              # registered submodules
moe sdlc new <project> "title" [--id slug]    # open a new sdlc run, scaffold its dir
moe sdlc design <project> <run>               # open a Claude Code session on the design document
moe sdlc code <project> <run>                 # open a Claude Code session on the code document (sandbox clone)
moe sdlc push <project> <run>                 # push the run's code branch and open (or update) a PR
moe status <project> <run> <document>         # reconcile a dispatched Managed Agents session
moe show <project> <run> <document>           # render current content.md + tail of thread.jsonl
moe review <project> <run>                    # synthesize per-run view (filtered log + doc snapshots)
moe scrap <project> <run> "reason"            # close without merging, record rationale
moe flag <project> <run> ["note"]             # mark as needing attention on the dashboard
moe unflag <project> <run>                    # clear the flag
moe history [project]                         # past runs (git log + cost aggregates)
moe sync                                      # git push the bureaucracy repo (sets upstream on first push)
moe version                                   # print moe version / OS / arch / Go runtime
moe help                                      # print usage
```

Each workflow nests its subcommands under its own verb (`moe sdlc <subcommand>`) so future workflows (`moe kb …`) can pick short stage names without colliding. The ones you live in are `moe dash`, `moe next`, `moe sdlc design`, and `moe sdlc code`, with `moe sdlc push` at the end of each run. `moe where`, `moe sync`, `moe version`, and `moe help` are housekeeping — they don't move run state, just report or publish it.

### `moe sdlc design` / `moe sdlc code` — The Core Loop

```bash
moe sdlc design telomere add-batch-support
```

(Or `moe sdlc code` once the design has settled. Either command follows the same flow below, with `moe sdlc code` adding the sandbox clone setup and `moe sdlc design` skipping it. The original design intent described an editor-based scratch-turn flow; today both commands hand off to an interactive `claude` session directly, and the steps below describe the original intent.)

This does:

1. **Init the submodule** if the session will touch code (`git submodule update --init projects/telomere`). Document-only sessions skip this.
2. **Print a one-screen context header** — current `content.md` snippet, upstream doc list, last turn timestamp, staleness reason if any.
3. **Open `$EDITOR` on a scratch turn file** (`.moe/scratch/<session>.md`) seeded with a template:
   ```
   # Your turn. Save and close to send. Empty file cancels.
   #
   # Document: spec
   # Stage: design (unsigned)
   # Upstream: pitch
   # Last agent turn (2026-04-12 09:42):
   #   > I've drafted the batch endpoint spec. A few open questions…
   ```
   Empty file = abort cleanly, no invocation, no commit. Shortcut: `moe work … -m "quick note"` skips the editor for one-liners.
4. **Assemble the prompt** from the guidance layer:
   - `soul.md` (global)
   - `stages/<current-stage>.md` (earliest unsigned active stage for the run)
   - `docs/<document>.md` (per-document guidance, if present)
   - `projects/telomere/overrides/<document>.md` (project-level, if exists)
   - `projects/telomere/runs/add-batch-support/overrides/<document>.md` (run-level, if exists)
   - Upstream documents (e.g., if working on architecture, include the spec — tagged with stage-signed state)
   - Current document content (if resuming)
   - Run context (`run.json`)
   - The operator's scratch turn (as the user message)
5. **Invoke Claude Code** with the per-document UUID session id stored in `run.json`:
   ```bash
   claude -p \
     --session-id "9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0" \
     --append-system-prompt "$(cat assembled-guidance.md)" \
     --allowedTools "Read,Grep,Glob,WebSearch,Edit(projects/telomere/runs/add-batch-support/documents/spec/content.md)" \
     --output-format stream-json \
     --verbose \
     --model claude-opus-4-6 \
     < prompt.md
   ```
   (Claude Code requires `--session-id` to be a UUID; MoE generates one per document on first use and stores it in `run.json` so subsequent turns resume the same conversation.)
6. **Stream output to the operator** via `json.NewDecoder` on the subprocess's stdout. Ctrl-C aborts cleanly — the session stays intact on Anthropic's side; nothing is committed locally.
7. **Commit the result** on `main` with structured trailers:
   ```
   work: update spec

   MoE-Run: add-batch-support
   MoE-Project: telomere
   MoE-Document: spec
   MoE-Session: 9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0
   MoE-Cost: $0.12
   ```
8. **Persist any session-id updates** to `run.json` if EnsureDocument minted a new UUID. Saved with the same commit.
9. **Append the turn to `thread.jsonl`** for audit.
10. **Fire a desktop notification** if the run exceeded a threshold (default 30s) — `osascript` on macOS, `notify-send` on Linux, via `os/exec`.

The operator reads the output, decides what to do:

- Run `moe work telomere add-batch-support spec` again to continue the conversation (same `--session-id`, picks up where it left off).
- Run `moe work telomere add-batch-support architecture` to start the next document (spec's current `content.md` becomes upstream context).
- `moe show …` to re-read the current document and recent thread without invoking an agent.
- Edit `content.md` directly and commit (human steering is first-class).
- `moe review telomere add-batch-support` to see the full diff.

### Multi-Turn Continuity

A per-document UUID stored in `run.json` (under `documents.<doc>.session`) is passed as `--session-id` to Claude Code. Each `moe work` invocation on the same document reuses the same UUID, so Claude Code resumes the server-side conversation. The operator posts a message, the agent responds, the document evolves. This is the "conversation is the document" model — using Claude Code's native session management.

### Per-Document Permissions

The `--allowedTools` flag scopes what each kind of work can do:

| Document | `--allowedTools` |
|------------|------------------|
| spec | `Read,Grep,Glob,WebSearch,Edit(…/spec/content.md)` |
| architecture | `Read,Grep,Glob,WebSearch,Edit(…/architecture/content.md)` |
| test-plan | `Read,Grep,Glob,WebSearch,Edit(…/test-plan/content.md)` |
| implementation | `Read,Grep,Glob,WebSearch,Edit,Write,Bash` (scoped to `projects/$target`) |
| deploy-plan | `Read,Grep,Glob,WebSearch,Bash` (scoped to deploy scripts) |

Non-code document invocations can only write to their own `content.md`. The implementation document gets the scary permissions. Enforcement is by Claude Code, not a custom sandbox.

### Git Model

```
main (the only branch — bureaucracy is a journal, not a code repo)
  ├── commit: "Open run telomere/add-batch-support: …"   trailers: MoE-Run, MoE-Project
  ├── commit: "work: update design"                           trailers: + MoE-Document, MoE-Session
  ├── commit: "work: update design"
  ├── commit: "work: update code"                             ← agent has committed inside the sandbox clone's moe/<id> branch
  ├── commit: "work: update code"
  └── commit: "push: telomere/add-batch-support"              trailers: + MoE-PR (status→approved)
```

- One branch (`main`). Per-run scoping comes from commit trailers, not branches.
- `moe sdlc push` pushes the sandbox's `moe/<run>` branch to the target remote, opens a PR via `gh pr create` (first time only), and flips the run's status — recorded as a commit on main with a `MoE-PR` trailer. `moe scrap` records rationale on main.
- `moe review` is `git log --grep="MoE-Run: <id>"` plus a render of each document's current state.
- Rewinding is `git reset --soft`; reverting is `git revert <sha>`.
- No custom checkpoint format. Git is the checkpoint.

The branch model belongs *inside* each target submodule — that's where actual code review happens via PRs. The bureaucracy itself is append-only narrative, like a wiki or a lab notebook.

**Concurrent work on the same project:**

Without per-run branches, the bureaucracy working tree is no longer the contention point — different runs' files don't overlap. The remaining contention would be the submodule checkout under `projects/<project>/`, which can only be on one target-repo branch at a time. MoE removes that contention by giving every run its own private, copy-on-write clone of the submodule — no opt-in, no flag:

- On first `moe sdlc code` against a run, the submodule at `projects/<project>/` is cloned to `.moe/clones/<project>/<run>/`. Later turns on the same run reuse it, so branch state and uncommitted edits persist across invocations.
- On macOS the clone is an APFS `clonefile(2)` call — O(metadata), no data copied, blocks shared with the source until either side writes. On other platforms a recursive copy is the fallback. Either way, `moe` code is pure-stdlib Go; no container runtime, daemon, or third-party dep.
- Submodules store their `.git` as a gitfile pointer into `.git/modules/projects/<project>/`. MoE clones that gitdir alongside the worktree (to `.moe/clones/<project>/.git-modules/<run>/`) and rewrites the clone's gitfile and `core.worktree` so the clone is a fully independent git repo. Two activities on the same project never touch each other's index, refs, or objects.
- `moe sdlc push` deliberately leaves the sandbox in place so `moe sdlc code` can resume iteration (PR review feedback, post-ship bugs). Cleanup is a separate concern (future `moe close` / `moe scrap`). The canonical `projects/<project>/` checkout is passive — MoE only reads from it to seed clones.

Document-only sessions (e.g. `moe sdlc design` on a design canvas, or future knowledge-base docs) don't touch the submodule and never needed isolation in the first place; they continue to write one markdown file under the bureaucracy and run freely in parallel.

**Submodule handling:**

- `moe sdlc code` requires a submodule on disk and runs the agent inside its sandbox clone. The agent creates branch `moe/<run>` and commits edits there; nothing ever lands on the canonical `projects/<project>/` checkout.
- `moe sdlc push` re-points the clone's `origin` at the project's remote, runs `git push -u origin moe/<run>`, and opens a PR via `gh pr create` (first push only). Re-runs push additional commits to the existing branch and print the open PR URL.

### Run State

Flat JSON, committed to main with the run's trailers (see the **Run** section for the schema). Stage commands update `run.json`; readers glob `projects/*/runs/*/run.json` and aggregate. No databases.

---

## Session UX

Across many registered projects, only a handful of runs are actually in motion at any time, and on any given morning you're the blocker for a small subset of those. The UX job is to surface that subset fast and get the operator back into flow.

Nothing runs in the background in v1 — agents act only when `moe work` is invoked. So the problem is **prioritization and resumption**, not live updates. That framing keeps the interface a shell tool, not a long-lived process.

### The daily loop

```
$ moe dash                            # what needs me today
$ moe status telomere add-batch       # drill in: per-run stages, docs, last turns
$ moe work telomere add-batch spec    # compose a turn in $EDITOR, stream response
$ moe show telomere add-batch spec    # re-read current state without invoking an agent
$ moe review telomere add-batch       # full run diff before approve
$ moe sign code telomere add-batch      # flip status (future: push submodule, open PR)
```

Most sessions are: `moe dash` → pick one → `moe work …` → read → repeat or move on. For a focused triage pass through everything demanding attention, skip the browsing step and use `moe next`.

### The dashboard (`moe dash`)

The home screen. One-shot, non-interactive, sorted by attention.

```
Ministry of Everything                                   2026-04-12  09:47

NEEDS ATTENTION (3)
  telomere    add-batch-operations       architecture stale after spec update
  telomere    fix-timeout-bug            design signed, ready for code
  photo-arc   exif-import                implementation: tests failing, 2 turns ago

ACTIVE (4)
  telomere    add-batch-operations       design unsigned · working on architecture
  telomere    fix-timeout-bug            design signed · code unsigned
  photo-arc   exif-import                design signed · working on implementation
  punchcard   oauth-flow                 design unsigned · last touched 6d ago

RECENT (last 7 days)
  telomere    rate-limit-fix             approved 3d ago    $4.21
  spam-fight  gmail-filter-v2            scrapped 5d ago    "wrong abstraction"

47 projects registered · 4 active · [moe project list] to browse
```

Implementation: `tabwriter` + ANSI color. Globs `projects/*/runs/*/run.json`, applies the attention filter, sorts. ~150 lines.

### The attention filter

A run lands in **NEEDS ATTENTION** when any of the following are true:

1. **Pending turn** — an agent posed a direct question (detected by the last blip's type + content) and hasn't been answered.
2. **Settled-upstream stale** — a document is stale *and* all of its upstream docs are part of a signed stage. The clean reconciliation case: no ambiguity about what to do next.
3. **Ready to sign** — the active stage's documents are coherent and not stale; `moe sign design` or `moe sign code` is the obvious next move.
4. **Explicit flag** — the operator ran `moe flag <project> <run> "note"`. The note shows in the dashboard. Self-left breadcrumbs.
5. **Failed run** — the last `moe work` crashed, hit a test failure, or had a submodule conflict. Exit status and last error are recorded in `run.json`.

A run *not* in NEEDS ATTENTION is still discoverable under ACTIVE — it just isn't demanding anything from the operator right now. Dormant runs (no activity in 30+ days) collapse out of the default view; `moe dash --all` shows everything.

### Triage mode (`moe next`)

The dashboard is passive — it tells you what's there. `moe next` is active — it drives you through the NEEDS ATTENTION list one item at a time, picking the right action for each. Think email's "next message" versus its inbox view.

`moe next` is a **dispatcher**, not a loop. Each attention trigger maps to the right action:

| Trigger | Action |
|---------|--------|
| Pending turn | `moe work <project> <run> <doc>` — opens the editor |
| Settled-upstream stale | `moe work` on the stale doc (upstream is settled, so the work is clear) |
| Ready to sign | `moe review`, then prompt: `[g] sign stage / [s]crap / [w]ork / skip` |
| Explicit flag | show the note, prompt: `[w]ork / [u]nflag / skip` |
| Failed run | show the error, prompt: `[r]etry / [e]dit / skip` |

After each item completes (or is skipped), `moe next` re-reads the dashboard — state has likely changed — and advances to the new top. Session-wide controls:

- **Enter** — take the default action for this item
- **`s`** — skip (don't revisit this item in this session)
- **`f`** — flag for later (`moe flag`) and move on
- **`q`** — quit

Flags for focused passes:

```
moe next --project telomere      # only this project
moe next --only ready            # only ready-to-sign runs (sign-off day)
moe next --only stale            # only stale reconciliations
moe next --dry-run               # preview the queue without taking action
```

The skip list is in-memory only — it resets when you quit. Quitting mid-session is safe: nothing is held open, nothing rolls back.

**Why this matters at scale.** Browsing the dashboard to pick what to touch is fine when three things need attention. When twelve do, the browsing itself becomes the friction. `moe next` is the "inbox zero" path — sit down, Enter-Enter-Enter through the obvious ones, skip what needs thought, come out the other side with a smaller list.

### Per-run view (`moe status <project> <run>`)

Stage-aware, not a flat list:

```
telomere / add-batch-operations                     opened 2026-04-08
in_progress · created 2026-04-10 · 14 turns · $2.83

STAGES
  design          signed 3h ago
  code            unsigned

DOCUMENTS
  design          6 turns, last 3h ago
  code            2 turns, last 2h ago  (design re-signed since last turn — banner will fire)

LAST ACTIVITY
  2h ago  work: update code
  3h ago  sign: design
  4h ago  work: update design

NEXT  moe work telomere add-batch-operations code
```

The "NEXT" hint is advisory — the operator can ignore it. The "banner will fire" annotation is derived from the run's filtered commit log: a `MoE-Stage-Signed` commit for an upstream stage that post-dates the downstream document's last `work:` commit. Same data the `upstreamChangeBanner` uses at `moe work` time.

### Reading without invoking (`moe show`)

```
$ moe show telomere add-batch-operations spec
```

Renders current `content.md` plus the last N turns from `thread.jsonl`. No `claude -p` invocation, no cost, no commit. Useful for: "what did we agree to yesterday?" before composing a turn.

### Composing a turn — editor-based

`moe work` opens `$EDITOR` on a scratch file, seeded with a header showing context and the agent's last message. Save and close → send. Empty file (or only comments) → abort cleanly without invoking Claude.

Why editor-first:
- Matches how engineers already think through technical input.
- Works over SSH, tmux, VS Code's terminal, Zellij — no special bindings.
- Cheap to abort (Ctrl-C or save empty).
- Diffable, attachable to commits as an optional audit trail.

One-liners skip the editor: `moe work … -m "try a storage split instead"`.

### Streaming, interrupt, notifications

While Claude Code runs, output streams token-by-token to the terminal (parsed from `stream-json`). The operator can:

- **Ctrl-C** to abort. The local invocation stops immediately; nothing is committed. Claude Code's server-side session remains intact, so the next `moe work` on the same document can pick up where it left off.
- **Let it run**. For implementation turns, a run can take 10+ minutes. `moe work` fires a desktop notification on completion if the wall-clock exceeds a threshold (`osascript` on macOS, `notify-send` on Linux, configurable in `agents.conf`). Keeps you from staring at the terminal.

### Cross-document pushback — operator-mediated with sugar

When a downstream agent flags a concern about an upstream doc, the operator carries it back. A small convenience:

```
$ moe work telomere add-batch-operations spec --from architecture
```

Pre-seeds the editor scratch with the architect's last relevant turn as a blockquote and a prompt for the operator's framing. Otherwise identical to a normal turn — the operator still writes the actual question. Skippable; plain `moe work … spec` also works.

### Flagging and unflagging

```
$ moe flag telomere add-batch-operations "need to decide on max batch size"
$ moe unflag telomere add-batch-operations
```

Adds or removes a note attached to the run in `run.json`. Flagged runs always show in NEEDS ATTENTION with the note rendered inline. This is the operator's own Post-it — no magic, just a visible reminder.

---

## Implementation

The `moe` CLI is Go stdlib plus `git` and `claude` on PATH. Estimated ~2000-2500 lines once all phases ship, including stream-JSON parsing, config parsers, the attention-filter dashboard, and the `moe next` dispatcher. Phase 1 (single-document end-to-end) is ~600-800 of that. No external dependencies.

### Subcommand Routing

Table-driven dispatch + `flag.FlagSet` per subcommand. Each command file registers itself into a map in `init()`; `main` is a thin entrypoint that hands `os.Args[1:]` and the standard streams to `cli.Run`. ~30 lines of routing replaces a framework, `help` derives from the live table, and the dispatcher is testable with in-memory `io.Writer`s.

```go
// cmd/moe/main.go
func main() {
    os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}

// internal/cli/cli.go
type Command struct {
    Name    string
    Summary string
    Run     func(args []string, stdout, stderr io.Writer) int
}

var commands = map[string]*Command{}

func Register(c *Command) { /* panics on duplicate */ commands[c.Name] = c }

func Run(args []string, stdout, stderr io.Writer) int {
    if len(args) == 0 {
        PrintUsage(stderr); fmt.Fprintln(stderr, "try 'moe dash'")
        return 1
    }
    cmd, ok := commands[args[0]]
    if !ok {
        fmt.Fprintf(stderr, "moe: unknown command %q\n", args[0])
        PrintUsage(stderr); return 1
    }
    return cmd.Run(args[1:], stdout, stderr)
}

// internal/cli/version.go
func init() {
    Register(&Command{Name: "version", Summary: "print moe version", Run: runVersion})
    Register(&Command{Name: "help",    Summary: "print usage",       Run: runHelp})
}
```

New subcommands drop a file in `internal/cli/` with an `init()` that calls `Register` — no central switch to edit.

### Configuration Files — Three Audiences, Three Formats

No YAML dependency. The files a human edits regularly are never JSON. The files a machine manages are never a custom format.

| File | Writer | Reader | Format | Parser |
|------|--------|--------|--------|--------|
| `run.json`, `features.json`, `project.json` | `moe` CLI | `moe status`, agents | JSON | `encoding/json` |
| `agents.conf` | Human (rare) | `moe work` | INI | `bufio.Scanner` + `strings.Cut`, ~20 lines |
| `soul.md`, `stages/*.md`, `docs/*.md`, overrides | Human (frequent) | Agents | Markdown | No parsing — concatenated |
| `thread.jsonl` | `moe work` | Human audit | JSONL | `bufio.Scanner` + `json.Unmarshal` |

INI parser sketch (flat config):

```go
func ParseINI(r io.Reader) (map[string]map[string]string, error) {
    sections := map[string]map[string]string{}
    var current string
    scanner := bufio.NewScanner(r)
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" || line[0] == '#' { continue }
        if line[0] == '[' {
            current = strings.Trim(line, "[]")
            sections[current] = map[string]string{}
        } else if k, v, ok := strings.Cut(line, "="); ok {
            sections[current][strings.TrimSpace(k)] = strings.TrimSpace(v)
        }
    }
    return sections, scanner.Err()
}
```

### Git Operations

`os/exec` shelling out to `git`. This is already what `moe work` does for submodule init and commits.

```go
func gitCommit(dir, msg string, paths ...string) error {
    if err := exec.Command("git", append([]string{"add", "--"}, paths...)...).Run(); err != nil {
        return err
    }
    return exec.Command("git", "commit", "-m", msg).Run()
}

func gitLogForRequest(reqID string) (string, error) {
    out, err := exec.Command("git", "log", "--grep=MoE-Run: "+reqID).Output()
    return string(out), err
}
```

No in-process git library. `git` on PATH is always available for this use case.

### Claude Code Invocation

`os/exec` to invoke, `encoding/json` with `json.NewDecoder` to stream-parse `--output-format stream-json`:

```go
cmd := exec.Command("claude", "-p",
    "--session-id", sessionID,
    "--append-system-prompt", guidance,
    "--allowedTools", tools,
    "--output-format", "stream-json",
    "--verbose",
    "--model", model,
)
cmd.Stdin = strings.NewReader(prompt)

stdout, _ := cmd.StdoutPipe()
decoder := json.NewDecoder(stdout)
cmd.Start()

for decoder.More() {
    var event map[string]any
    decoder.Decode(&event)
    // display event, append to thread.jsonl, detect tool use, etc.
}
cmd.Wait()
```

### Prompt Assembly

`os.ReadFile` + `strings.Builder`. Concatenate soul.md + applicable stage fragment + applicable doc fragment + overrides (most-specific-first) + upstream documents (tagged with stage-signed state) + current content + run context. ~50 lines.

### File Discovery

`filepath.Glob("projects/*/runs/*/run.json")` for aggregation. One line.

### Stage Operations

The only typed graph is the stage DAG, and it's hand-rolled in `internal/stage/stage.go`:

```go
type Stage struct {
    Name     string
    Requires []string // stages that must be signed before this one
    Help     string
}

var all = map[string]Stage{
    "design": {Name: "design", Help: "…"},
    "code":   {Name: "code", Requires: []string{"design"}, Help: "…"},
}
```

`stage.Active` finds the earliest unsigned stage whose prerequisites are all signed; `stage.Dependents` inverts for unsign cascade; `stage.LatestSign` + the run's filtered commit log drive the upstream-change banner. Two stages, ~170 lines, no adjacency-map abstractions.

### Terminal Display

`text/tabwriter` for aligned status tables. `fmt` for everything else. Raw ANSI escape codes for color — four const declarations, no library. Diffs shell out to `git diff` which is already colored.

### Module Layout

```
moe/
├── cmd/moe/main.go              # subcommand routing (~50 lines)
├── internal/cli/                # one file per subcommand (~400 lines total)
├── internal/guidance/           # prompt assembly (~100 lines)
├── internal/git/                # branch/submodule/commit helpers (~200 lines)
├── internal/claude/             # assemble and exec claude -p, stream parse (~150 lines)
├── internal/state/              # run.json, thread.jsonl I/O (~150 lines)
├── internal/stage/              # stage DAG, sign/unsign, upstream-change detection (~170 lines)
├── internal/config/             # INI + block parsers (~100 lines)
└── internal/display/            # tabwriter status, ANSI, diffs (~100 lines)
```

~2000-2500 lines once all phases ship. No web framework, no graph engine, no database, no IPC, no YAML. Shell out to `git` and `claude`.

---

## UI

### CLI (Primary Interface)

The `moe` commands above are the primary interface. Status tables, streaming agent output, git diffs. That's the product.

### Web UI (Deferred)

A read-only web layer reading git state is a future addition once the CLI patterns stabilize. When added, it's `net/http` + `html/template` + SSE for streaming — all stdlib. No WebSocket required (server-sent events handle agent streaming naturally; operator actions remain CLI-side).

Planned views (deferred):

- **Ministry Home**: Bureau list with status chips, active run counts, alert badges.
- **Bureau Dashboard**: Project-level view — changelog, ops health, active runs, backlog, decision log.
- **Run View**: Google Wave-inspired document workspace. Document selector, split view of document content + thread log, upstream-change banner indicators.
- **Context Panel**: Side panel showing upstream docs, code, test results relevant to the active document.
- **Run Review**: Per-run synthesized view — filtered commit log + current document content + staleness indicators.

Three reusable components compose all views: **document viewer** (rendered markdown), **commit-log viewer** (filtered by run trailer), and **thread viewer** (JSONL log).

### Interaction Model

**Batch-with-gates, not real-time chat.** Each `moe work` invocation runs an agent to completion — it reads context, produces a draft, commits. Between invocations the operator can:

- **Continue**: Run `moe work` again on the same document to extend the conversation (same `--session-id`).
- **Revise**: Seed the next turn with feedback (piped in via stdin or an editor).
- **Steer**: Edit the document directly and commit (human authorship is first-class).
- **Advance**: Run `moe work` on the next document to pull the current one in as upstream context.

The thread log accumulates across invocations. The "chat with agents" experience is the sequence of these invocations across a run's lifecycle. True real-time co-editing is a future evolution.

---

## Architecture

### How a Session Works

1. Operator invokes `moe work <project> <run> <document>`
2. `moe` inits the target submodule if the session needs code access; otherwise no submodule work
3. `moe` assembles the prompt from `soul.md`, applicable stage/doc fragments, overrides, upstream documents, current content
4. `moe` invokes `claude -p` with the assembled prompt, the document's per-document UUID `--session-id`, `--allowedTools`, and the document's configured model
5. `moe` streams Claude Code's output to the operator and appends to `thread.jsonl`
6. When Claude Code finishes, `moe` commits the resulting changes (document content.md, run.json status update, and any target-submodule pointer updates) on `main` with structured trailers
7. Operator inspects and decides the next move: continue, advance to another document, edit manually, review, approve, scrap

### Process Model

**The human is the scheduler.** One agent at a time under the operator's attention. Multiple terminal sessions work in parallel across different runs — they don't touch each other on the bureaucracy side because all commits land on `main` with distinct trailers.

Concurrent implementation sessions on the same project don't contend, because every `moe work` automatically gets a private copy-on-write clone of the submodule under `.moe/clones/<project>/<run>/` — APFS `clonefile(2)` on macOS, a recursive copy fallback elsewhere. Two runs against the same project get two independent working trees and two independent gitdirs; neither touches the canonical `projects/<project>/` checkout. Document-only sessions don't touch the submodule at all and run freely in parallel. See the **Git Model** section for the concrete mechanism. Docker or SSH wrappers remain a Phase-5 option layered *on top of* the clone, for kernel-enforced isolation rather than concurrency.

**Cross-document negotiation is operator-mediated.** When the architecture session pushes back on the spec ("batch size 10,000 needs different storage"), the operator carries that pushback into the spec conversation. No automated inter-agent messaging. A future evolution could add a `for` loop in `moe work --negotiate` to iterate across documents until settled or max iterations — it's a `for` loop, not a DAG engine.

### Technology

CLI: Go stdlib. Persistence: git + committed JSON/JSONL + INI + block-format config. Agent backend: Claude Code headless (`claude -p`). Per-run workspace isolation: pure-Go APFS `clonefile(2)` on macOS (recursive-copy fallback elsewhere) — no container runtime, no daemon, no dep. Stronger kernel-enforced sandbox for implementation work remains a Phase-5 option via `os/exec` into `docker`/`ssh`/`daytona`, layered on top of the clone.

### Git Model

**Two repos, two audiences.** The bureaucracy repo (back office) contains guidance fragments, run state, run history — all on a single `main` branch, scoped per-run via commit trailers. Target project repos (front office) are git submodules — clean code, no MoE artifacts; their own branch model lives there, where code review actually happens via PRs. The `moe` CLI itself lives in its own repo — tool and state are separate. See the **Hierarchy** and **Git Model** subsections for the full picture.

---

## Bootstrap Plan

The Ministry's first project is itself. Build the minimum that can manage one run end-to-end, then dogfood.

### Phase 0: Workspace Scaffolding

- [x] `moe init` scaffolds the marker file (`bureaucracy.conf`) plus `projects/` (with `.gitkeep`), initializes git on `main`, optionally sets an origin remote, stages the scaffolding, and prompts the operator to commit
- [x] `moe where` resolves the bureaucracy root via `$MOE_HOME` or a `$PWD` walk to the marker file
- [x] `moe project add <repo-url>` adds a submodule under `projects/<id>/src/`, detects the default branch via `git ls-remote --symref`, writes `projects/<id>/project.json`, and commits
- [x] `moe project remove <id>` is the symmetrical inverse (refuses if `projects/<id>/runs/` holds any runs)
- [ ] Extend `moe init` to also lay down `stages/`, `docs/`, `soul.md`, `agents.conf`
- [ ] Write initial `soul.md` and one or two fragments (`stages/design.md`, `docs/spec.md`)
- [ ] Seed `agents.conf` with the minimal per-doc model + tools entries

### Phase 1: Single-Document End-to-End

- [x] `moe sdlc new <project> "title" [--id slug]` scaffolds `run.json` and commits it on main with `MoE-Run` / `MoE-Project` trailers (slugs auto-suffix on collision; explicit `--id` is strict). Equivalent wrappers will be registered on future workflows (`moe kb new`, …).
- [x] `moe work <project> <run> <document>` mints a UUID session id per document (stored in `run.json`), runs `claude` with `--session-id` (first turn) or `--resume` (subsequent turns), injects a minimal system prompt, and commits any document changes with the full trailer block. Per-run sandbox clones under `.moe/clones/` are provisioned for projects with a submodule.
- [ ] Flesh out `moe work` to match the full README description: `-p`/`--output-format stream-json` headless flow, `--allowedTools` per document, `--model` from `agents.conf`, guidance-fragment assembly, upstream-document context injection, editor-based turn composition, streaming display, desktop notification on long runs, `thread.jsonl` audit log
- [ ] `moe status` reads `run.json` glob, renders with `tabwriter`
- [ ] `moe review` filters `git log --grep="MoE-Run: <id>"` and renders each document's current content
- [ ] Run the loop manually against a real run. Iterate on the guidance fragments.

### Phase 2: Full Run Lifecycle

- [x] `moe sign <project> <run> <stage>` / `moe unsign` record `MoE-Stage-Signed` / `MoE-Stage-Unsigned` commit trailers, enforce `design` as a prerequisite of `code`, cascade unsigns to dependent stages, flip run status on `code`, and tear down the sandbox clone on `sign code`
- [x] `moe sdlc push` publishes the bureaucracy (auto `-u origin HEAD` on first push)
- [ ] Finish `moe sign code`: commit+push the target submodule, open a PR on the target remote (deferred side-effects like release notes live in Phase 4)
- [ ] `moe scrap` — record rationale, flip run status (all on main)
- [ ] `moe history` — `git log` aggregation with cost totals from commit trailers
- [ ] Additional per-doc fragments (implementation, test-plan, deploy-plan)
- [ ] Project-level overrides

**Self-hosting checkpoint**: MoE manages its own development via `moe work`.

### Phase 3: Ripple & Review Surfaces

- [x] `upstreamChangeBanner` in `moe work` surfaces re-signed prerequisite stages since the doc's last turn, with file path and diff command
- [ ] `moe status` shows which downstream documents have pending upstream-change banners, plus per-doc last-turn timestamps
- [ ] `moe review` renders the per-run synthesized view (filtered log + current doc contents + pending banners)
- [ ] `moe work` optionally inlines current upstream-document content into the prompt, not just the banner pointer

### Phase 4: Derived Artifacts (Revisit)

- [ ] Revisit what, if anything, should be derived from a completed run — release notes, decision summaries, backlog updates. Deferred from v1; design first, build only what earns its place against real runs.

### Phase 5: Code Editing

- [ ] `docs/implementation.md` with initializer/continuation pattern guidance
- [ ] `features.json` session continuity
- [ ] `moe work <project> <run> implementation` with expanded `--allowedTools` scoped to the per-run sandbox clone
- [ ] Submodule commit/push/PR flow at `moe sign code` (push from the clone, bump the canonical submodule pointer)
- [ ] Optional: Docker/SSH wrapper around `claude -p` for stronger (kernel-enforced) isolation on top of the clone

**Self-hosting checkpoint**: MoE can plan *and* execute its own development.

### Phase 6: Ops Hooks (Future)

- [ ] `docs/ops.md` / `stages/*` guidance for monitoring-driven runs
- [ ] External triggers (cron, alerts) that spawn runs — **any trigger without a human at the keyboard must route to the Claude API under Commercial Terms, not Claude Code headless.** See Review Notes.
- [ ] Cross-project queries (likely via shell scripts over `git log` + `jq`; real SQL is deferred until needed)

### Phase 7: Polish & Optional Web UI

- [ ] Read-only web layer via `net/http` + SSE, reading git state
- [ ] Bureaucracy theme fully applied (naming, UI personality)
- [ ] Open source release as a Module Collective portfolio piece
- [ ] Configuration guide for alternative LLM backends

---

## What's Deferred

Features not in v1. Not rejected — just sequenced after the minimum works.

| Feature | Status | Notes |
|---------|--------|-------|
| Typed document graph with derived/persistent nodes | Rejected for v1 | Earlier design proposed `document-graph.conf`; pruned as speculative generality. Revisit only if a real need for cross-document dependency mechanics shows up. |
| Automated iteration across documents | Deferred | Human sequences work. A `for` loop in `moe work --negotiate` handles cross-doc iteration if needed. |
| Web UI with real-time streaming | Deferred | Read-only layer over git state, `net/http` + SSE. Post-CLI-stabilization. |
| Scheduled runs / ops alert triggers | Deferred to Phase 6+ | Must route to Claude API backend, not Claude Code headless (compliance). |
| Parallel agent execution | Deferred | Multiple terminal sessions cover current needs. Goroutines + `sync.WaitGroup` when automated. |
| DuckDB / SQL-queryable run history | Deferred | `git log` + commit trailers cover v1. Shell out to `sqlite3` or DuckDB later if needed. |
| Anthropic Managed Agents backend | Rejected for core, narrow-yes for implementation work | See Review Notes. |
| Managed sandbox providers (Docker, Daytona) | Deferred | `os/exec` wrapper around `claude -p` is sufficient when needed. |
| Cross-project knowledge queries | Deferred | Revisit once enough runs exist to ask questions against `git log` + trailers. |
| Yolo mode (`moe yolo <req> --through <stage>` — autonomously drafts every document and crosses every gate up to `<stage>`, including `deploy` if the operator asks for it; distinct from Claude Code's in-session `--dangerously-skip-permissions`, though yolo generally wants both) | Deferred | Every run passes human review at every turn and gate in v1. |
| Interactive TUI | Deferred | One-shot `moe dash` covers the prioritization/resumption problem. Revisit if navigation friction justifies a Bubble Tea or raw-termios TUI — but that would breach the stdlib-only stance, so only if the one-shot dashboard is demonstrably insufficient. |

---

## Review Notes

_Open items from spec review — April 2026._

**[Q] Anthropic Managed Agents — evaluated, rejected for core, narrow-yes for implementation.** [Anthropic Managed Agents](https://www.anthropic.com/engineering/managed-agents) (launched April 2026) provisions a per-session container as the agent's workspace, with Anthropic running the agent loop on its orchestration layer. Evaluated against MoE and rejected for the document-centric core because it conflicts with four design principles: (1) it's per-token API billing — the opposite of the Claude Code headless cost model; (2) it's Claude-only and first-party-only, violating model-agnosticism; (3) it keeps session state server-side via SSE, violating "repo is the source of truth"; (4) it duplicates sandbox functionality already covered by `os/exec` wrappers. Beyond principles, the vast majority of MoE's workload is document-editing sessions that only read the repo and write one markdown file — none of which benefit from a server-hosted container with bash/code execution. **Narrow-yes case:** Phase 5 implementation work could treat Managed Agents as one sandbox option alongside a Docker/SSH wrapper, selected per invocation. Not a priority; revisit only if hosted-for-clients scenarios emerge where Max/Pro subscriptions don't transfer.

**[Q] Subscription vs. API backend routing — compliance constraint.** Anthropic's Consumer Terms (clarified Feb 2026, enforced April 4, 2026) permit Claude Code headless under a Pro/Max subscription **only for interactive, operator-driven runs** — a human kicking off `moe work` from their own terminal under their own Claude Code install. Scheduled runs, ops-triggered runs, multi-tenant deployments, always-on services, and anything resembling production automation must route through the Claude API under Commercial Terms, regardless of cost. This is a bright line — Anthropic actively detects and blocks the forbidden pattern (OpenClaw was cut off on April 4, 2026). Resolve before Phase 6 (the first place scheduled triggers appear). **Additional constraints regardless of trigger:** never read `~/.claude` auth material from `moe`, never re-use Claude Code's OAuth tokens against the API, never pipe Pro/Max credentials through the Anthropic SDK. **Phase 7 open-source framing:** the README must not encourage new users to point subscription-backed automation at employer codebases — the public framing is "interactive use under your own Claude Code install, or API keys for everything unattended."

---

## Open Questions

1. **Multi-user future**: The initial design is single-operator. If consulting clients want visibility, do they get read-only access via the eventual web UI? Worth modeling in the data layer even if not built yet.

2. **Run dependencies**: Runs can spawn other runs. How are cross-run dependencies tracked? Likely lightweight — a `spawned_from` field in `run.json` with a reference to the source run. Blocking dependencies (run B can't start until run A merges) can be enforced by stage sessions refusing to advance B's documents until A is approved.

---

## References

- [OpenAI: Harness Engineering](https://openai.com/index/harness-engineering/) — Codex team's methodology for agent-first development
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents) — Initializer/coder pattern, progress files, multi-session continuity
- [Gas Town](https://github.com/steveyegge/gastown) — Multi-agent orchestration with Git-backed state (Steve Yegge)
- [OpenClaw](https://github.com/openclaw/openclaw) — Autonomous agent framework (Peter Steinberger); the pattern Anthropic's Feb 2026 Consumer Terms clarification targeted
- [Google Wave](https://en.wikipedia.org/wiki/Google_Wave) — The original "equal parts conversation and document" platform
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html) — Analysis of harness patterns and categories
- [The Emerging Harness Engineering Playbook](https://www.ignorance.ai/p/the-emerging-harness-engineering) — Cross-cutting patterns from OpenAI, Stripe, and OpenClaw
