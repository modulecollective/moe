# Ministry of Everything (MoE)

**A bureaucracy-themed agent harness for the full lifecycle of anything.**

Module Collective LLC · April 2026

---

## Vision

The Ministry of Everything is a CLI-first agent harness where a human operator collaborates with AI agents through threaded conversations attached to living documents. It manages the full lifecycle of products and projects — from ideation through design, implementation, deployment, and operations — through a document graph that ripples changes between interconnected artifacts.

The core insight: **the document and the conversation about the document are the same thing.** The spec *is* the conversation about the spec. The architecture *is* the threaded discussion that produced it. Documents compress conversations into clean artifacts that become context for downstream work.

The Ministry is designed for a single operator managing multiple products with agent assistance. It is domain-agnostic — software development is the first ministry to open, but the same machinery handles any workflow that produces interconnected documents. The bureaucracy is the feature.

**"Please take a number."**

### Design Principles

1. **The human is the workflow engine.** No orchestrator, no DAG executor, no scheduler. The operator looks at the request, decides what to work on next, and tells `moe` to do it. A small CLI assembles prompts, invokes Claude Code, and tracks state in git. The harness gets out of the way.
2. **The repo is the source of truth.** All state — documents, decisions, conversations, progress — lives in git. Nothing lives in Slack, Google Docs, or people's heads. The bureaucracy repo is the "back office" (workflows, guidance fragments, run history). Target project repos stay clean. The `moe` CLI lives in its own repo — tool and state are separate so the CLI can be open-sourced without leaking private bureaucracy contents.
3. **Agents are participants, not tools.** Agents join conversations and contribute to documents. Guidance for how they behave lives as plain markdown in the bureaucracy repo (global `soul.md` plus per-stage and per-document fragments assembled at invocation time) — no role taxonomy, no handbook hierarchy.
4. **Requests terminate, products persist.** Units of work (requests) have a lifecycle and end. Products are the long-lived entities that accumulate completed work.
5. **Loose recipes, deterministic mechanics.** The document graph defines known document types and typical relationships as guidance. The operator assembles whatever subset fits the situation. Within a request, staleness propagation and upstream-context assembly are deterministic — given the graph and the events, the state is always derivable.
6. **Model-agnostic.** The harness works with Claude Code, Ollama/Qwen, Codex, or any LLM backend. A small config routes invocations to the right model, keyed by document type or stage. The harness is the moat, not the model.
7. **Minimize entropy.** Every agent mistake becomes a guidance-fragment update. The system improves every time you use it.
8. **One repo, one history, one branch.** All request state and run metadata live on the bureaucracy repo's `main`. There are no per-request branches — the bureaucracy is a journal, not a code repo. Per-request scoping and lifecycle state come from commit trailers (`MoE-Request`, `MoE-Document`, `MoE-Session`, `MoE-Stage-Signed`, `MoE-Stage-Unsigned`); a request's history is `git log --grep="MoE-Request: <id>"` and its stage state is derived from that log. Code branches live where they belong: inside each target submodule.
9. **Target repos are independent.** No MoE coupling in target repos. Works with any repo — open source forks, client projects, personal code. Target projects are registered as git submodules under `projects/`, but only one is checked out at a time during a session.
10. **Many projects registered, a handful actively worked.** Submodules are cheap to register, so the ceiling on *registered* projects is high. The ceiling on *concurrently active* projects is the operator's review bandwidth — practically ~10-30 — because every request ultimately cashes out in a human reading a diff. MoE helps manage the fan-out, it does not remove it.
11. **Stdlib only, where practical.** The `moe` CLI uses Go stdlib plus `git` and `claude` on PATH. No YAML parser, no CLI framework, no DAG engine, no graph library, no web server dependencies. Three stdlib-native config formats match three audiences: JSON for machine state, INI for flat human config, `text/scanner` blocks for nested human config. Markdown for guidance. Humans never see JSON.

### Direction

MoE is a thin CLI wrapper around `claude -p` plus conventional git plumbing. Every feature maps directly to a Claude Code flag, a git operation, or something the human decides in the moment. Automation can grow back later — parallel runs, derived-artifact hooks on merge, a thin web layer reading git state — but it grows out of a minimal, working CLI, not the other way around.

---

## Data Model

### Hierarchy

```
bureaucracy/                       # private state repo, cloned alongside the moe CLI repo
├── soul.md                        # Global agent guidance
├── stages/                        # Per-stage markdown guidance (design.md, pr.md, …)
├── docs/                          # Per-document markdown guidance (spec.md, architecture.md, …)
├── document-graph.conf            # Document type library (block format)
├── projects/                      # Git submodules pointing at target repos
│   ├── telomere/                  # → github.com/modulecollective/telomere
│   └── next-idea/                 # → github.com/modulecollective/next-idea
├── requests/                      # Request state, document graph artifacts
│   ├── telomere/
│   │   ├── project.json           # Project metadata
│   │   ├── overrides/             # Project-level guidance overrides (optional)
│   │   └── runs/
│   │       ├── add-batch-support/ # in_progress
│   │       ├── fix-timeout-bug/   # in_progress
│   │       ├── mvp-build/         # approved
│   │       └── websocket-eval/    # scrapped
│   └── next-idea/
│       └── …
└── (single branch: main — per-request scoping via commit trailers)
```

`stages/` and `docs/` are flat directories of optional markdown fragments. `moe work` concatenates the applicable ones with `soul.md`, any request-level overrides, and the upstream documents to build the prompt. No role taxonomy, no handbook-per-agent — just guidance keyed to the two axes that actually vary: the lifecycle stage and the kind of document.

### Two Repos: CLI and Bureaucracy

MoE is split across two independent git repos, cloned side-by-side, in the same relationship as `git` ↔ a repository or `hugo` ↔ a site:

- **`moe/`** — the CLI repo. Go source for the `moe` binary. No private data, no pointer to the bureaucracy. Open-source-eligible. Installed to `$PATH` like any other tool.
- **`bureaucracy/`** — the private state repo, cloned at the same level as `moe/`. Holds `soul.md`, per-stage and per-doc guidance, the document graph definition, `requests/`, run history, and `projects/*` submodules pointing at target repos. The `moe` binary operates on whichever bureaucracy directory it's invoked from (discovered via `$PWD` walk or `$MOE_HOME`).

Upgrading `moe` is a `go install`; the bureaucracy is untouched. Matches principle 11 — the CLI is just a tool on `$PATH`.

### The Bureaucracy Repo

The bureaucracy repo holds guidance fragments, the document graph definition, request state, and run history across all projects. There is one unified history — one set of request IDs, one `git log`. Dashboards, portfolio views, and cross-project queries are all views over the same repo.

### Project (Target Repo)

A long-lived entity representing a software product or project. Registered as a git submodule under `projects/`. Born from `moe project add <repo-url>`, persists as long as the project is managed.

**Target project repos are registered as git submodules under `projects/`.** The bureaucracy repo stores all orchestration metadata — guidance, request state, run history. The target repo stores only its own code. This separation means:

- Projects can pre-exist MoE. Fork an interesting open source project, `moe project add` it, and start managing requests against it.
- Target repos stay clean — no MoE files, no framework artifacts. Someone looking at the target repo sees normal, well-crafted commits.
- Hundreds of projects can be registered. Only one submodule is checked out at a time during a session.
- Projects can use whatever structure, language, or conventions make sense for them.

```json
// requests/telomere/project.json
{
  "id": "telomere",
  "name": "Telomere",
  "status": "live",
  "description": "Timeouts as a service",
  "submodule": "projects/telomere",
  "remote": "github.com/modulecollective/telomere",
  "default_branch": "main",
  "deploy_url": "https://telomere.modulecollective.dev",
  "created": "2025-03-15"
}
```

During a session, `moe work` runs `git submodule update --init projects/$target` so agents can read code and (for code-editing work) edit it. Code changes result in submodule pointer updates committed on `main` with the request's trailers. At approval time, the changes inside the submodule are committed, pushed to the target remote, and optionally opened as a PR — the submodule pointer update in the bureaucracy records which target commit was produced.

```json
// In request.json after completion
{
  "commits": [
    {
      "submodule": "projects/telomere",
      "range": "abc1234..def5678",
      "branch": "main",
      "pr": "https://github.com/modulecollective/telomere/pull/42"
    }
  ]
}
```

**Project-level concerns (stored in the bureaucracy repo under `requests/<project>/`):**

Most project-level artifacts are **persistent documents** in the document graph — they're updated when requests merge. See **The Document Graph** section for the full definition.

- **Changelog**: Persistent document, accumulated from request release notes.
- **Decision Log**: Persistent document, accumulated from all requests including scrapped ones.
- **API Reference**: Persistent document, updated as API surface changes.
- **User Guide**: Persistent document, updated as features ship.
- **Architecture Overview**: Persistent document, kept in sync as design evolves.
- **Dependency Manifest**: Persistent structured data (JSON), queryable across products.
- **Ops Runbook**: Persistent document, updated as infrastructure changes.
- **Backlog**: The one manually maintained product-level artifact. Lightweight request ideas — a title and a paragraph, not a full spec.

### Request

A unit of work against a project. Has a defined lifecycle and terminates.

```json
// requests/telomere/runs/fix-timeout-bug/request.json
{
  "id": "fix-timeout-bug",
  "project": "telomere",
  "title": "Fix timeout bug from overnight alert",
  "status": "in_progress",
  "created": "2026-04-12",
  "origin": "ops_alert",
  "priority": "high",
  "documents": {
    "spec":           { "session": "9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0" },
    "architecture":   { "session": "1c8e2b9f-3441-4d5a-8e23-9d0f7c2b3a14" },
    "implementation": { "session": "7d2a5e1c-90b3-4c11-a4d2-2e5b1c0a9f33" }
  }
}
```

Request statuses: `in_progress | approved | scrapped`. Documents have no status field — they are just files on disk (`content.md`), and a document's history is its commit history. The only per-document data in `request.json` is the Claude Code session id so `moe work` can resume the same conversation.

**Gates live at stage transitions, not on every document.** The human-in-the-loop pauses are `moe sign <project> <request> <stage>` — one call per lifecycle checkpoint. `moe unsign` reverses a stage. A sign is recorded as a commit on main with `MoE-Stage-Signed: <stage>` in the trailers; a stage is "signed" iff its most recent signed trailer is newer than its most recent unsigned trailer (or there's no unsign). No status field to keep in sync; the journal is the source of truth.

**Unsign cascades.** Reopening a stage invalidates anything that required it. If `pr` was signed because `design` was signed, then `moe unsign design` automatically unsigns `pr` in the same command — as a separate commit, with its own trailer, so the journal shows exactly what happened. The cascade walks the dependency graph breadth-first and only touches stages that are currently signed, so running unsign twice is still a safe no-op.

Known stages:

- `design` — design is settled; implementation can start
- `pr` — ready to open a PR on the target repo (requires `design`; flips request status to `approved`; PR-opening side-effect is TBD)
- `review`, `test`, `retro`, `deploy` — reserved names; signable today (trailer only), behavior to be wired in later

**Request progress is driven by the operator, not a rigid phase sequence.** Small bug fixes might sign nothing but `pr`. Larger work signs `design` first. The stage vocabulary is a closed set so history stays comparable across requests; additions are a deliberate change, not a config knob.

The document graph within a request defines the natural ordering:

| Document | Upstream Dependencies |
|----------|----------------------|
| Spec | (none — origin) |
| Architecture | Spec |
| Implementation | Architecture, Spec |
| Test Plan | Spec, Implementation |
| Deploy Plan | Implementation |
| (Ancillary) | (configurable) |

Phase transitions are **explicit but lightweight** — they happen when the operator runs `moe sign <stage>`. Stages are checkpoints, not a state machine layered over every document.

**Request rollup on completion:**

When `moe sign <project> <request> pr` runs:
- Precondition: `design` must already be signed. The command refuses otherwise.
- The request's status flips to `approved` in `request.json` and the change is committed on main with `MoE-Stage-Signed: pr` in the trailers; history stays browsable via `git log --grep="MoE-Request: <id>"`
- Future behavior (not yet implemented): push the target submodule, open a PR, generate derived artifacts, update product-level persistent documents. Those will become side-effects of `moe sign pr` — a single atomic gate the operator crosses once per request.

When `moe scrap <request> "reason"` runs:
- Decision rationale is recorded as a derived artifact
- Request remains in the product's history as institutional memory
- No code or config changes are applied

### Derived Artifacts

When a request is approved (via `moe sign pr`), downstream nodes in the document graph will be triggered — both request-level derived documents and product-level persistent documents. See **The Document Graph** section under Document for the full picture. Derivation is explicit: the sign-pr side-effect will walk the graph, identify which derived/persistent nodes have their dependencies satisfied, and generate each in turn (using the guidance in `docs/<derived-doc>.md`). Nothing runs automatically in the background. (Not yet implemented — `moe sign pr` today only flips request status and records the trailer.)

### Document

The primary unit of work within a request. A document is both a **living artifact** (the spec, the architecture design, the test plan) and a **conversation space** where you collaborate with agents to produce and refine it.

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

All commits land directly on `main`. There are no per-request branches. Each commit carries structured trailers so any one request's history can be reconstructed programmatically:

```
work: update spec

MoE-Request: add-batch-support
MoE-Project: telomere
MoE-Document: spec
MoE-Session: 9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0
```

A request's full audit trail is `git log --grep="MoE-Request: add-batch-support"`. A document's history is the same plus `MoE-Document: spec`. The atomic-review surface lost by skipping a feature branch is reconstructed by `moe review`, which prints the request's commits and the current state of each document.

```
main (the only branch)
  ├── commit: "Open request telomere/add-batch-support: …"
  ├── commit: "work: update spec"
  ├── commit: "work: update spec"
  ├── commit: "work: update architecture"
  ├── commit: "work: update architecture"
  ├── commit: "work: update implementation"
  ├── commit: "work: update test-plan"
  ├── commit: "sign: design"                    (MoE-Stage-Signed: design)
  └── commit: "sign: pr"                        (MoE-Stage-Signed: pr, status→approved)
```

Stage sign-offs (`design`, `pr`, `review`, `test`, `retro`, `deploy`) are the coordination signals; they live only in the git log as `MoE-Stage-Signed` / `MoE-Stage-Unsigned` trailers, flipped by `moe sign` / `moe unsign`. `moe sign pr` is the hard gate that will eventually trigger the submodule push and derived-artifact generation.

**The ripple is operator-driven, not background.** Agents only act when `moe work` is invoked. "Rippling through documents" means the operator walking the graph — typically via `moe next`, which dispatches the obvious action for each attention-triggering doc. There is no background worker; there is a human pressing Enter through an attention queue.

- **In progress**: The operator is invoking `moe work …`. Each turn appends a trailer-tagged commit to main.
- **Design signed**: `moe sign <proj> <req> design` records the checkpoint. Implementation work can start in earnest. `moe review` is the per-request synthesized view.
- **Approved**: `moe sign <proj> <req> pr` flips the request status and (eventually) pushes the submodule, generates derived artifacts — all as further commits on main.

Human review is required by default before `moe sign pr`. A future "yolo mode" can auto-sign for well-understood request types where the agents consistently produce good output.

**Ripple flow in practice:**

1. `moe request new telomere "Add batch support"` — opens the request, commits the scaffolding to main.
2. `moe work telomere add-batch-support spec` — you collaborate with Claude on the spec; commit lands on main with trailers.
3. `moe work telomere add-batch-support architecture` — with the spec in upstream context, Claude drafts the architecture; commit.
4. `moe work telomere add-batch-support implementation` — with both upstream, Claude drafts the implementation plan; commit.
5. The implementation session flags a concern about the architecture — you carry it back into `moe work … architecture` with "the implementation session says the interface needs pagination, reconsider". Two more turns, two more trailer-tagged commits.
6. Everything settles. `moe review telomere add-batch-support` — you read the request's commits and current document state.
7. `moe sign telomere add-batch-support design` — you cross the design gate.
8. `moe sign telomere add-batch-support pr` — request status flips; (future) submodule code is pushed, derived artifacts generate. Done.

For well-scoped requests with good guidance files, this converges quickly. You start a conversation, the agents ripple through the documents you drive them through, and you come back to a complete package. One conversation, one review, one sign-pr.

Recovery and rewriting are also branchless. Rollback is `git revert <sha>`. Per-request history is `git log --grep`. Rewinding a stuck conversation is `git reset --soft` — the same regardless of which request you're working on.

**Document types** are defined in the **document graph** (see below) — a workspace-level library of known document types, their typical relationships, and their agents. This is a parts bin, not a rigid structure. The operator assembles whatever subset and wiring fits each request.

**Ancillary documents** can be added to any request for cross-cutting concerns. They participate in the request's ripple graph like any other document.

Each document is just a directory on disk — `documents/<name>/content.md` — and an entry in the parent `request.json` carrying the Claude Code session id. The document's "state" is its content and its commit history; there is no sidecar status file.

**Ripple mechanism:**

When a document is updated, `moe status` reports all transitive downstream documents as `stale`. Staleness is derived from the graph + the request's filtered commit log (`git log --grep="MoE-Request: <id>"`) — nothing is mutated in the background. The operator decides when to reconcile:

```
Spec updated (v3: added batch ops)
  → Architecture [STALE: spec changed, revise next]
      → Implementation Plan [implicitly stale, waiting on architecture]
      → Test Plan [STALE: spec added acceptance criteria]
```

The operator controls the pace. You can reconcile downstream docs immediately (`moe work …`), let them sit stale while you focus on something else, or decide a particular downstream doesn't need to be touched.

**Cross-document agent collaboration:**

A downstream agent reviewing an upstream change can flag concerns, which the operator carries back into the upstream document's conversation. For example: while working on the architecture, Claude reads a spec change and says "A batch size of 10,000 will require a completely different storage architecture — can we cap at 1,000?" — you copy that pushback into the spec thread (`moe work telomere add-batch spec`) with "the architecture session flagged …", Claude responds on the spec, and you review the result. The operator is the messenger; the sessions don't talk to each other directly.

This turns the document graph into a genuine collaborative workspace rather than a one-way waterfall. Upstream docs get better because downstream agents pressure-test them — the same way a good implementation engineer pushes back on a design that won't hold up. Your role as operator is to arbitrate and mediate, which you'd do anyway.

**Ripple summary view:**

`moe review` synthesizes the per-request view by filtering the main-branch log on the request's trailer:

- The request's commits in order, grouped by document (spec changes, architecture changes, implementation changes, etc.)
- Current `content.md` of each document at HEAD
- Unresolved staleness: "architecture has been touched since the test plan was signed"

This is the primary review surface. One synthesized view, organized so you can see the coherent picture across all documents. When it looks good, `moe sign pr`.

**The Document Graph:**

The workspace defines a **document graph** — a library of known document types, their typical relationships, and their behavior. This is **guidance, not a rulebook**. It represents institutional knowledge about what good process looks like — the forms the Ministry knows how to handle.

It spans three scopes:

1. **Request documents** (conversational): You chat with agents to produce them. Spec, Architecture, Implementation, etc. They live on `main` like everything else; the request's trailers scope them to the request.
2. **Request-level derived documents** (auto-generated at approval): Structured data and summaries extracted from the request's work. Generated by `moe sign pr`, committed to the request.
3. **Product-level persistent documents** (long-lived): API docs, user guides, architecture overview, changelog. These live at the product level and get incrementally updated when requests are approved. They outlive any individual request.

All three types are nodes in the same graph with the same dependency mechanics. The only differences are scope, lifecycle, and whether they're conversational or derived.

The graph is stored in `document-graph.conf` — a block format parsed by Go's `text/scanner`:

```
# document-graph.conf — the parts bin
#
# Node types:
#   conversational - human chats with agent to produce content
#   derived        - auto-generated at approval from upstream nodes
#   persistent     - lives at product level, incrementally updated across requests
#
# For conversational docs, depends_on is GUIDANCE for the operator — the
# operator assembles whatever graph fits the situation. For derived/persistent
# docs, depends_on is mechanical — these trigger at approval based on what
# the request produced.

# === Request documents (conversational) ===

pitch {
    type          conversational
    scope         request
    description   "One-pager. What is this, why does it matter, who is it for."
}

spec {
    type          conversational
    scope         request
    depends_on    pitch
    description   "Requirements, acceptance criteria, constraints."
}

architecture {
    type          conversational
    scope         request
    depends_on    spec
    description   "System design, interfaces, tradeoffs, ADRs."
}

implementation {
    type          conversational
    scope         request
    depends_on    architecture, spec
    description   "Task breakdown, code, tests, CI config."
}

test-plan {
    type          conversational
    scope         request
    depends_on    spec, implementation
    description   "Test strategy, cases, coverage."
}

deploy-plan {
    type          conversational
    scope         request
    depends_on    implementation
    description   "Deploy steps, migrations, rollback."
}

security-review {
    type          conversational
    scope         request
    depends_on    architecture, spec
    description   "Threat model, attack surface, mitigations."
}

migration-plan {
    type          conversational
    scope         request
    depends_on    architecture
    description   "Migration steps, data strategy, rollback."
}

cost-analysis {
    type          conversational
    scope         request
    depends_on    spec
    description   "Infrastructure costs, API costs, ROI."
}

# === Request-level derived documents (generated at moe sign pr) ===
# Triggered when ANY dependency is present. Generation handles partial context gracefully.

release-notes {
    type          derived
    depends_on    spec, implementation
    format        markdown
}

decisions {
    type          derived
    depends_on    spec, architecture, implementation
    format        json
}

references {
    type          derived
    depends_on    spec, architecture
    format        json
}

dependencies {
    type          derived
    depends_on    implementation
    format        json
}

api-changes {
    type          derived
    depends_on    implementation, spec
    format        json
}

# === Product-level persistent documents (updated at moe sign pr) ===

product-changelog {
    type          persistent
    depends_on    release-notes
    format        markdown
}

product-decision-log {
    type          persistent
    depends_on    decisions
    format        json
}

product-api-docs {
    type          persistent
    depends_on    api-changes, spec
    format        markdown
}

product-user-guide {
    type          persistent
    depends_on    spec, release-notes
    format        markdown
}

product-architecture-overview {
    type          persistent
    depends_on    architecture
    format        markdown
}

product-dependency-manifest {
    type          persistent
    depends_on    dependencies
    format        json
}

ops-runbook {
    type          persistent
    depends_on    deploy-plan
    format        markdown
}
```

This is the parts bin plus the mechanical layer. For each request, the operator selects conversational nodes and wires them. When `moe sign pr` runs, derived nodes generate and persistent product-level nodes are incrementally updated. The mechanical parts (derived, persistent) trigger deterministically. The creative parts (conversational) are assembled by judgment.

**Entry points — a request can start from anything:** A pitch, a bug report, a customer request, a screenshot, a code snippet, or nothing at all. The operator looks at the input and picks the right set of documents.

**The Clerk (operator role):**

The Clerk is the role that decides what a request needs. **Initially, this is the human operator.** When you start a request with `moe request new <project> "title" [--id slug]`, you read `document-graph.conf` for guidance, look at your input, and make an engineering judgment call about what documents this request needs. Common shapes:

- "Add batch support to the timeout API" → Pitch → Spec → Architecture → Implementation → Test Plan → Deploy Plan
- "Fix the health check timeout bug" → Implementation → Test Plan (no spec or architecture needed)
- "I just want to think through a storage architecture" → Architecture (single doc, no dependencies)
- "Explore three competing approaches to caching" → three Architecture docs, no dependencies between them
- "I have an idea for a new caching layer" → Pitch (just capture it)

Templates can be saved as named presets for common shapes, but they're just `moe new` flags that pre-populate the documents list — not a separate system. A future evolution automates the Clerk as an LLM agent that reads the input and proposes the document set. For v1, the human is the Clerk.

Derived and persistent documents are triggered based on which conversational documents the request produced. If the request has an implementation document, the dependency manifest gets generated. If the request has a spec and implementation, the API docs get updated. All of this runs at `moe sign pr` time.

**Two layers — flexible planning, deterministic mechanics.** The operator decides what documents a request needs (flexible, informed by the graph but not bound by it). Once the documents exist, staleness propagation and upstream-context assembly are deterministic.

**`depends_on` wears two hats, and the boundary matters.** On conversational nodes in `document-graph.conf`, `depends_on` is *guidance for the Clerk* selecting a doc set for a new request — "if you pick a spec, you probably want an architecture downstream." Once the operator commits to a doc set, those same edges become *mechanical* within the request: staleness BFS and upstream-context DFS both walk them. On derived/persistent nodes, `depends_on` is always mechanical — it determines which derivation runs fire at `moe sign pr`. Rule of thumb: graph-level edges are guidance for *future* requests; within a live request the graph is concrete.

**Structured data:** Derived documents can be prose (markdown) or structured (JSON). Over time, product-level persistent documents accumulate structured data across all requests — "which products depend on this library?" becomes a query across `product-dependency-manifest` files. The knowledge graph emerges from shipping requests.

**Index files:**

Agents and tools need to understand workspace state without parsing every file in the tree. Index files are committed JSON files that provide a pre-computed view of the workspace — the "table of contents" — an agent or CLI reads one file to know what exists, what's active, and where to look deeper.

Two levels:

1. **`index.json`** (workspace root) — all projects, their statuses, and counts of in-progress/approved/scrapped requests per project. This is what `moe status` reads to summarize the landscape.

2. **`requests/<project>/index.json`** (per-project) — all requests for this project with their current status, documents in each request's graph, and which persistent documents have been updated recently.

```json
// requests/telomere/index.json
{
  "project": "telomere",
  "status": "live",
  "active_requests": [
    {
      "id": "add-batch-support",
      "status": "in_progress",
      "documents": ["spec", "architecture", "implementation", "security-review"],
      "current_document": "implementation"
    },
    {
      "id": "fix-timeout-bug",
      "status": "in_progress",
      "documents": ["implementation"],
      "current_document": "implementation"
    }
  ],
  "approved_requests": 12,
  "scrapped_requests": 1,
  "persistent_docs": {
    "changelog": { "updated": "2026-03-01" },
    "api_reference": { "updated": "2026-02-28" },
    "architecture_overview": { "updated": "2026-02-15" },
    "dependency_manifest": { "updated": "2026-03-01" }
  }
}
```

Index files are maintained by `moe` — updated on every request state change (creation, approval, scrap). They are deterministically derivable by globbing `requests/*/runs/*/request.json`, so `moe reindex` rebuilds them from scratch if they drift. Because `moe` is the only writer under normal operation, they stay in sync.

The key constraint: **no binary databases, no SQLite, no caches that live outside git.** Index files are plain text, committed, diffable, and version-controlled like everything else. An agent or human can `cat index.json` and know the state of the world. Git blame shows who changed what and when. This is consistent with the "repo is the source of truth" principle — if it's not in git, it doesn't exist.

**Future: knowledge graph as text files.** A future evolution adds explicit knowledge graph files (`knowledge/entities.json`, `knowledge/relationships.json`) — committed JSON tracking semantic relationships across documents and products. Enables content lineage tracing and cross-product queries. Same rules as index files: plain text, committed, diffable, rebuildable.

**Growth:**

The document graph evolves. You start simple and grow it as your practice matures. Add "Performance Analysis" as a conversational node when you find yourself needing it. Add "License Audit" as a derived node when you want automated compliance checks. Products can have graph overrides that add domain-specific document types. The graph is versioned in git like everything else.

### Thread

A conversation within a document. The primary mechanism is **Claude Code's native session**: each document maps to one `--session-id`, and multi-turn continuity is server-side. `moe work telomere add-batch spec` invoked a second time resumes that session — the agent remembers the prior turns.

For auditability, each `moe work` invocation appends a JSONL record to the document's local thread log:

```jsonl
{"id": "blip-001", "author": "james", "type": "human", "ts": "…", "content": "We need to support batch operations for the timeout API. Users are calling it in a loop and hitting rate limits."}
{"id": "blip-002", "author": "agent", "type": "agent", "ts": "…", "content": "That makes sense. I'd suggest a POST /batch endpoint that accepts an array of timeout configs. A few questions: should batch operations be atomic (all-or-nothing) or partial (best-effort)?"}
{"id": "blip-003", "author": "james", "type": "human", "ts": "…", "parent": "blip-002", "content": "Partial. Return individual results for each item."}
{"id": "blip-004", "author": "agent", "type": "agent", "ts": "…", "content": "Got it. I've updated the spec document with the batch endpoint definition, partial semantics, and new acceptance criteria.", "document_version": 3}
```

Stored at `requests/<project>/runs/<request>/documents/<doc>/thread.jsonl`. Append-only. JSONL because it's the most stdlib-friendly streaming format: `encoding/json` + `bufio.Scanner`.

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
Request: "Add batch support"

Working on the spec:
  - Guidance: soul.md + stages/design.md + docs/spec.md + project overrides
  - Request description (lightweight)
  - Product-level context (project.json, existing architecture doc)

Working on the architecture:
  - Guidance: soul.md + stages/design.md + docs/architecture.md + project overrides
  - Upstream Spec document (current content at HEAD; tagged "design-signed" if the design stage is signed)
  - Product-level context
  - Existing codebase structure from product repo

Working on the implementation:
  - Guidance: soul.md + stages/design.md (until PR stage) + docs/implementation.md + project overrides
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
│   └── pr.md                      # "Before PR, verify tests, rationale captured, scope disciplined."
├── docs/                          # Per-document guidance (what good looks like for this kind of doc)
│   ├── spec.md                    # "Specs name the problem, the acceptance criteria, and the non-goals."
│   ├── architecture.md            # "Architecture docs explain why, not just what. Include tradeoffs."
│   └── implementation.md          # "Keep the task list short. One change per commit."
├── agents.conf                    # Model + allowedTools routing, keyed by document (and/or stage)
├── requests/
│   ├── telomere/
│   │   ├── overrides/             # Project overrides (optional)
│   │   │   └── implementation.md  # "Use Deno, not Node. Deploy to Fly.io."
│   │   └── runs/
│   │       └── add-batch-support/
│   │           └── overrides/     # Request overrides (rare, optional)
│   │               └── implementation.md   # "This request touches billing, extra caution"
```

**Layer resolution:** request-level → project-level → global. Same pattern as gitconfig, CSS specificity, or Consul's config hierarchy. `moe work` concatenates whichever override files exist in most-specific-first order.

**`soul.md`** is the equivalent of Codex's AGENTS.md or Claude Code's CLAUDE.md. It's the general guidance every invocation gets: your engineering philosophy, how you like things communicated, quality standards, what to escalate vs. decide autonomously. This is the document that captures your engineering judgment in a form agents can consume.

**Stage fragments** (`stages/<stage>.md`) shape behavior by *where* the work sits in the lifecycle. `stages/design.md` emphasizes exploration and tradeoff-surfacing; `stages/pr.md` emphasizes rigor and scope discipline. A fragment applies when the request has entered that stage but not yet signed the next one.

**Document fragments** (`docs/<doc>.md`) shape behavior by *what* is being written. `docs/spec.md` says what a good spec looks like; `docs/architecture.md` says what a good architecture document looks like. These are the closest thing we keep to role definitions — and they're indexed by the artifact, not by a role taxonomy.

**Project and request overrides** let you tailor behavior locally. A prototype might have relaxed standards. A fork of a safety-critical system might have stricter review requirements.

Because these are markdown files in git, they evolve through the same mechanism as everything else. An agent makes a recurring mistake → you update the relevant fragment → that mistake doesn't happen again. **The harness improves every time you use it.** Agents can even propose updates to their own guidance fragments (reviewed as a diff, just like any other document change).

### Agent Context Assembly

When the operator runs `moe work <project> <request> <document>`, the CLI assembles the invocation's context from the guidance layer plus the document model:

```
Context = soul.md
        + stages/<current-stage>.md   (the earliest unsigned active stage)
        + docs/<document>.md          (if present)
        + project overrides           (if any)
        + request overrides           (if any)
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

Following Anthropic's harness research, each request maintains:

- **`request.json`** (request root): Tracks which documents exist and their Claude Code session ids. That's all — no document statuses; a document's state is its content plus its commit history.
- **`features.json`** (inside implementation document directory): Implementation-specific session continuity. Detailed feature list with completion status, test coverage, and quality grades. Lives with the implementation doc because it's only relevant to code-editing work.
- **Claude Code sessions**: The `--session-id` keeps multi-turn conversation state server-side. Resumption is transparent.
- **Git history**: The ultimate source of truth for what changed and when.

Most documents get continuity from their Claude Code session and their own `content.md` — they pick up where they left off by reading their output. Implementation work additionally needs `features.json` because it involves complex multi-step work spanning sessions (implementing features, running tests, fixing failures).

The first implementation session in a request uses an **initializer prompt** that sets up the environment, writes the initial feature files, and makes the first commit. Subsequent sessions use the **continuation prompt** that reads progress, picks up where the last session left off, and makes incremental progress.

### Model Agnosticism

Swap a model per document type by editing `agents.conf`. The `model` field is passed verbatim to `claude --model` (or to whatever CLI the backend exposes). A future backend abstraction could route `ollama:qwen3` to a local Ollama invocation instead of Claude Code, but for v1, Claude Code is the backend.

### Claude Code Headless as the Primary Backend

**Claude Code in headless mode (`claude -p`)** is MoE's primary agent backend. It is invoked by `moe work` as a subprocess for **interactive, operator-driven runs only** — a human at a keyboard kicking off `moe work` under their own Claude Code install.

**Compliance boundary (read this before changing the backend logic).** Anthropic's Consumer Terms (clarified February 2026, enforced April 4, 2026) draw a bright line around Claude Code + Pro/Max subscriptions:

- ✅ **Allowed:** Spawning the real `claude` CLI as a subprocess from your own script, with OAuth flowing through Claude Code normally, as part of an interactive developer session on a local machine.
- ❌ **Banned:** Extracting OAuth tokens from Claude Code and replaying them against `api.anthropic.com`. Using Pro/Max OAuth tokens with the Agent SDK. Driving Claude Code under a subscription from scheduled jobs, always-on services, multi-tenant deployments, or anything that looks like production automation. Third-party "harnesses" that wrap or impersonate Claude Code under a subscription (the OpenClaw pattern Anthropic actively blocked).

MoE's position: `moe work` spawning `claude -p` from an operator-initiated command is in the allowed bucket — real CLI, real OAuth, no token extraction, single human driver. **Any future workflow that runs without a human at the other end must route to the Claude API backend under Commercial Terms instead, regardless of cost.** See Review Notes for the routing rules.

> ⚠️ **Never** read `~/.claude` auth material from `moe`, re-use Claude Code's OAuth tokens against the API, or pipe Pro/Max credentials through the Anthropic SDK. These are the patterns Anthropic detects and blocks — do not cross this line to "optimize" anything.

`moe work` invokes Claude Code with assembled context:

```bash
claude -p \
  --session-id "moe/telomere/add-batch-support/spec" \
  --append-system-prompt "$(cat assembled-guidance.md)" \
  --allowedTools "Read,Grep,WebSearch,Edit(requests/telomere/runs/add-batch-support/documents/spec/content.md)" \
  --output-format stream-json \
  --verbose \
  --model claude-opus-4-6 \
  < prompt.md
```

**Key mechanics:**

- **Multi-turn sessions via `--session-id`**: Each document maps to a Claude Code session keyed by a per-document UUID stored in `request.json` (Claude Code requires UUIDs as session ids; MoE generates one on first use). Context is preserved server-side across turns. The operator posts a new turn via `moe work`, the agent responds, the CLI appends the response to the thread log and commits any document changes on `main`.
- **Context injection via `--append-system-prompt`**: `moe work` assembles guidance (soul.md + applicable stage fragment + applicable doc fragment + project/request overrides) and upstream documents (tagged with stage-signed state), then injects them as the system prompt.
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
moe                                         # print usage + a hint: "try 'moe dash'"
moe dash                                    # dashboard — the home screen
moe next                                    # triage mode — do the top attention item, then the next
moe init                                    # scaffold a new MoE workspace
moe project add <repo-url>                  # register a target repo as submodule
moe request new <project> "title" [--id slug]      # open a new request, scaffold its dir
moe status <project> <request>              # per-request view: document graph, staleness, last turns
moe work <project> <request> <document>     # the main command — work on a document
moe show <project> <request> <document>     # render current content.md + tail of thread.jsonl
moe sign <project> <request> <stage>        # sign a lifecycle stage (design, pr, review, test, retro, deploy)
moe unsign <project> <request> <stage>      # reverse moe sign
moe review <project> <request>              # synthesize per-request view (filtered log + doc snapshots)
moe scrap <project> <request> "reason"      # close without merging, record rationale
moe flag <project> <request> ["note"]       # mark as needing attention on the dashboard
moe unflag <project> <request>              # clear the flag
moe history [project]                       # past requests (git log + cost aggregates)
moe project list                            # registered submodules
moe reindex                                 # rebuild index.json files from request.json glob
```

~18 commands. The ones you live in are `moe dash`, `moe next`, and `moe work`, with `moe sign` sprinkled between turns.

### `moe work` — The Core Loop

```bash
moe work telomere add-batch-support spec
```

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
   - `stages/<current-stage>.md` (earliest unsigned active stage for the request)
   - `docs/<document>.md` (per-document guidance, if present)
   - `requests/telomere/overrides/<document>.md` (project-level, if exists)
   - `requests/telomere/runs/add-batch-support/overrides/<document>.md` (request-level, if exists)
   - Upstream documents (e.g., if working on architecture, include the spec — tagged with stage-signed state)
   - Current document content (if resuming)
   - Request context (`request.json`)
   - The operator's scratch turn (as the user message)
5. **Invoke Claude Code** with the per-document UUID session id stored in `request.json`:
   ```bash
   claude -p \
     --session-id "9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0" \
     --append-system-prompt "$(cat assembled-guidance.md)" \
     --allowedTools "Read,Grep,Glob,WebSearch,Edit(requests/telomere/runs/add-batch-support/documents/spec/content.md)" \
     --output-format stream-json \
     --verbose \
     --model claude-opus-4-6 \
     < prompt.md
   ```
   (Claude Code requires `--session-id` to be a UUID; MoE generates one per document on first use and stores it in `request.json` so subsequent turns resume the same conversation.)
6. **Stream output to the operator** via `json.NewDecoder` on the subprocess's stdout. Ctrl-C aborts cleanly — the session stays intact on Anthropic's side; nothing is committed locally.
7. **Commit the result** on `main` with structured trailers:
   ```
   work: update spec

   MoE-Request: add-batch-support
   MoE-Project: telomere
   MoE-Document: spec
   MoE-Session: 9b6c0f2a-e041-4d35-9b1a-1ae0f7b1c2f0
   MoE-Cost: $0.12
   ```
8. **Persist any session-id updates** to `request.json` if EnsureDocument minted a new UUID. Saved with the same commit.
9. **Append the turn to `thread.jsonl`** for audit.
10. **Fire a desktop notification** if the run exceeded a threshold (default 30s) — `osascript` on macOS, `notify-send` on Linux, via `os/exec`.

The operator reads the output, decides what to do:

- Run `moe work telomere add-batch-support spec` again to continue the conversation (same `--session-id`, picks up where it left off).
- Run `moe work telomere add-batch-support architecture` to start the next document (spec's current `content.md` becomes upstream context).
- `moe show …` to re-read the current document and recent thread without invoking an agent.
- Edit `content.md` directly and commit (human steering is first-class).
- `moe review telomere add-batch-support` to see the full diff.

### Multi-Turn Continuity

A per-document UUID stored in `request.json` (under `documents.<doc>.session`) is passed as `--session-id` to Claude Code. Each `moe work` invocation on the same document reuses the same UUID, so Claude Code resumes the server-side conversation. The operator posts a message, the agent responds, the document evolves. This is the "conversation is the document" model — using Claude Code's native session management.

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
  ├── commit: "Open request telomere/add-batch-support: …"   trailers: MoE-Request, MoE-Project
  ├── commit: "work: update spec"                             trailers: + MoE-Document, MoE-Session
  ├── commit: "work: update spec"
  ├── commit: "work: update architecture"
  ├── commit: "work: update implementation"                   ← includes submodule pointer update
  ├── commit: "work: update test-plan"
  ├── commit: "sign: design"                                  trailers: + MoE-Stage-Signed: design
  └── commit: "sign: pr"                                      trailers: + MoE-Stage-Signed: pr (status→approved)
```

- One branch (`main`). Per-request scoping comes from commit trailers, not branches.
- `moe sign pr` runs the submodule push, generates derived artifacts, and flips the request status — all as further commits on main. `moe scrap` records rationale on main.
- `moe review` is `git log --grep="MoE-Request: <id>"` plus a render of each document's current state.
- Rewinding is `git reset --soft`; reverting is `git revert <sha>`.
- No custom checkpoint format. Git is the checkpoint.

The branch model belongs *inside* each target submodule — that's where actual code review happens via PRs. The bureaucracy itself is append-only narrative, like a wiki or a lab notebook.

**Concurrent work on the same project:**

Without per-request branches, the bureaucracy working tree is no longer the contention point — different requests' files don't overlap. The remaining contention is the submodule checkout under `projects/<project>/`, which can only be on one target-repo branch at a time. Two layers handle this:

1. **Default: flock.** `moe work` takes a filesystem lock at `.moe/locks/<project>.lock` for the duration of the Claude Code invocation when the session needs the submodule (i.e. implementation turns). A second submodule-touching `moe work` on the same project blocks briefly, or exits with "telomere is busy on request `req-a`; wait, or use `--worktree`."
2. **Opt-in: git worktrees** — for the submodule, not the bureaucracy. `moe work --worktree <project> <request> <doc>` materializes a parallel checkout of the target repo at `.moe/worktrees/<project>-<request>/` so two implementation sessions can run on the same target in parallel. The Claude Code subprocess runs with `cwd` set to the worktree; the invocation's `Bash` and `Edit` tools are scoped to that directory. `moe sign pr` and `moe scrap` clean up the worktree.

Document-only sessions (spec, architecture, test-plan, deploy-plan) only edit one markdown file in the bureaucracy repo and don't touch the submodule, so they have no contention at all and run freely in parallel.

**Submodule handling:**

- `moe work` runs `git submodule update --init projects/$target` before invoking Claude Code (only when the session will touch code).
- Code changes inside `projects/$target/` result in submodule pointer updates committed to main with the request's trailers.
- `moe sign pr` does: commit inside submodule → push to target remote → update pointer in bureaucracy repo → generate derived/persistent docs → flip request status → optionally open a PR on the target repo. All bureaucracy-side commits land on main.

**`moe sign pr` is resumable.** The cascade can invoke 5-10+ derivation steps (once per derived doc, once per affected persistent product doc). Each step writes its output as a separate commit with a `MoE-Approve-Step: <node-id>` trailer alongside the request's normal trailers. If a step fails (network blip, generation error, push rejected), `moe sign pr` exits with a clear error. `moe sign pr --resume` filters main's log for the request's approve steps, skips ones already produced, and continues from the first missing step. The status flip happens last so partial-approve state is always "request still in_progress; some derived commits on main." Safe to re-run, safe to abandon.

### Request State

Flat JSON, committed to main with the request's trailers (see the **Request** section for the schema). `moe status` reads it. `moe work` updates it. No databases — just glob `requests/*/runs/*/request.json` and aggregate.

---

## Session UX

Across many registered projects, only a handful of requests are actually in motion at any time, and on any given morning you're the blocker for a small subset of those. The UX job is to surface that subset fast and get the operator back into flow.

Nothing runs in the background in v1 — agents act only when `moe work` is invoked. So the problem is **prioritization and resumption**, not live updates. That framing keeps the interface a shell tool, not a long-lived process.

### The daily loop

```
$ moe dash                            # what needs me today
$ moe status telomere add-batch       # drill in: per-request graph + last turns
$ moe work telomere add-batch spec    # compose a turn in $EDITOR, stream response
$ moe show telomere add-batch spec    # re-read current state without invoking an agent
$ moe review telomere add-batch       # full request diff before approve
$ moe sign pr telomere add-batch      # merge, derive, push
```

Most sessions are: `moe dash` → pick one → `moe work …` → read → repeat or move on. For a focused triage pass through everything demanding attention, skip the browsing step and use `moe next`.

### The dashboard (`moe dash`)

The home screen. One-shot, non-interactive, sorted by attention.

```
Ministry of Everything                                   2026-04-12  09:47

NEEDS ATTENTION (3)
  telomere    add-batch-operations       architecture stale after spec update
  telomere    fix-timeout-bug            design signed, ready for pr
  photo-arc   exif-import                implementation: tests failing, 2 turns ago

ACTIVE (4)
  telomere    add-batch-operations       design unsigned · working on architecture
  telomere    fix-timeout-bug            design signed · pr unsigned
  photo-arc   exif-import                design signed · working on implementation
  punchcard   oauth-flow                 design unsigned · last touched 6d ago

RECENT (last 7 days)
  telomere    rate-limit-fix             approved 3d ago    $4.21
  spam-fight  gmail-filter-v2            scrapped 5d ago    "wrong abstraction"

47 projects registered · 4 active · [moe project list] to browse
```

Implementation: `tabwriter` + ANSI color. Reads the workspace and per-project `index.json`, applies the attention filter, sorts. ~150 lines.

### The attention filter

A request lands in **NEEDS ATTENTION** when any of the following are true:

1. **Pending turn** — an agent posed a direct question (detected by the last blip's type + content) and hasn't been answered.
2. **Settled-upstream stale** — a document is stale *and* all of its upstream docs are part of a signed stage. The clean reconciliation case: no ambiguity about what to do next.
3. **Ready to sign** — the active stage's documents are coherent and not stale; `moe sign design` or `moe sign pr` is the obvious next move.
4. **Explicit flag** — the operator ran `moe flag <project> <request> "note"`. The note shows in the dashboard. Self-left breadcrumbs.
5. **Failed run** — the last `moe work` crashed, hit a test failure, or had a submodule conflict. Exit status and last error are recorded in `request.json`.

A request *not* in NEEDS ATTENTION is still discoverable under ACTIVE — it just isn't demanding anything from the operator right now. Dormant requests (no activity in 30+ days) collapse out of the default view; `moe dash --all` shows everything.

### Triage mode (`moe next`)

The dashboard is passive — it tells you what's there. `moe next` is active — it drives you through the NEEDS ATTENTION list one item at a time, picking the right action for each. Think email's "next message" versus its inbox view.

`moe next` is a **dispatcher**, not a loop. Each attention trigger maps to the right action:

| Trigger | Action |
|---------|--------|
| Pending turn | `moe work <project> <request> <doc>` — opens the editor |
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
moe next --only ready            # only ready-to-sign requests (sign-off day)
moe next --only stale            # only stale reconciliations
moe next --dry-run               # preview the queue without taking action
```

The skip list is in-memory only — it resets when you quit. Quitting mid-session is safe: nothing is held open, nothing rolls back.

**Why this matters at scale.** Browsing the dashboard to pick what to touch is fine when three things need attention. When twelve do, the browsing itself becomes the friction. `moe next` is the "inbox zero" path — sit down, Enter-Enter-Enter through the obvious ones, skip what needs thought, come out the other side with a smaller list.

### Per-request view (`moe status <project> <request>`)

Graph-aware, not a flat list:

```
telomere / add-batch-operations                     opened 2026-04-08
in_progress · created 2026-04-10 · 14 turns · $2.83

STAGES
  design          unsigned
  pr              unsigned

DOCUMENTS
  pitch           5 turns, last 2d ago
  spec            6 turns, last 2h ago
  architecture    3 turns, last 3h ago  (spec changed since — consider revising)
  implementation  (not started — consider after architecture settles)
  test-plan       (not started)
  deploy-plan     (not started)

LAST ACTIVITY
  2h ago  work: update spec
  4h ago  work: update architecture
  6h ago  work: update spec

NEXT  moe work telomere add-batch-operations architecture
```

The "NEXT" hint is advisory — the operator can ignore it. Staleness ("spec changed since …") is derived from the request's filtered commit log relative to the document graph.

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

Adds or removes a note attached to the request in `request.json`. Flagged requests always show in NEEDS ATTENTION with the note rendered inline. This is the operator's own Post-it — no magic, just a visible reminder.

---

## Implementation

The `moe` CLI is Go stdlib plus `git` and `claude` on PATH. Estimated ~2000-2500 lines once all phases ship, including a modest document-graph module, stream-JSON parsing, three config parsers, the attention-filter dashboard, and the `moe next` dispatcher. Phase 1 (single-document end-to-end) is ~600-800 of that. No external dependencies.

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
| `request.json`, `index.json`, `features.json`, `project.json` | `moe` CLI | `moe status`, agents | JSON | `encoding/json` |
| `agents.conf` | Human (rare) | `moe work` | INI | `bufio.Scanner` + `strings.Cut`, ~20 lines |
| `document-graph.conf` | Human (evolves over time) | `moe work`, `moe status`, `moe sign pr` | Block | `text/scanner`, ~40 lines |
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

Block parser uses `text/scanner` with a small state machine — identifiers, strings, punctuation, ~40 lines. Gives humans comments, no-quoting for single words, quoted strings when needed, comma-separated lists, clean block structure. Reads like a config file, not a data serialization format.

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
    out, err := exec.Command("git", "log", "--grep=MoE-Request: "+reqID).Output()
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

`os.ReadFile` + `strings.Builder`. Concatenate soul.md + applicable stage fragment + applicable doc fragment + overrides (most-specific-first) + upstream documents (tagged with stage-signed state) + current content + request context. ~50 lines.

### File Discovery

`filepath.Glob("requests/*/runs/*/request.json")` for aggregation. One line.

### Document Graph Operations

~100-150 lines. The graph is small (3-10 nodes per request); textbook BFS/DFS over adjacency maps:

```go
type DocGraph struct {
    Nodes map[string]*DocNode
}

type DocNode struct {
    ID        string
    Agent     string
    Type      string // conversational | derived | persistent
    DependsOn []string
}

func (g *DocGraph) Downstream(id string) []string { … }     // invert edges
func (g *DocGraph) UpstreamDocs(id string) []string { … }   // DFS up DependsOn
func (g *DocGraph) MarkStale(updatedID string) { … }        // BFS downstream
func (g *DocGraph) TopoSort() ([]string, error) { … }       // Kahn's, detects cycles
func (g *DocGraph) ReadyDerivedNodes(present []string) []string { … } // for moe sign pr
```

Staleness propagation is BFS downstream from the updated node. Upstream collection for `moe work` context assembly is DFS up the `DependsOn` edges. Topological sort falls out of Kahn's algorithm and detects cycles for free.

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
├── internal/state/              # request.json, index.json, thread.jsonl I/O (~150 lines)
├── internal/graph/              # document graph, staleness, topo (~150 lines)
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

- **Ministry Home**: Bureau list with status chips, active request counts, alert badges.
- **Bureau Dashboard**: Project-level view — changelog, ops health, active requests, backlog, decision log.
- **Request View**: Google Wave-inspired document workspace. Document graph visualization, document selector, split view of document content + thread log, stale indicators.
- **Context Panel**: Side panel showing upstream docs, code, test results relevant to the active document.
- **Request Review**: Per-request synthesized view — filtered commit log + current document content + staleness indicators.

Three reusable components compose all views: **document viewer** (rendered markdown), **commit-log viewer** (filtered by request trailer), and **thread viewer** (JSONL log).

### Interaction Model

**Batch-with-gates, not real-time chat.** Each `moe work` invocation runs an agent to completion — it reads context, produces a draft, commits. Between invocations the operator can:

- **Continue**: Run `moe work` again on the same document to extend the conversation (same `--session-id`).
- **Revise**: Seed the next turn with feedback (piped in via stdin or an editor).
- **Steer**: Edit the document directly and commit (human authorship is first-class).
- **Advance**: Run `moe work` on the next document to pull the current one in as upstream context.

The thread log accumulates across invocations. The "chat with agents" experience is the sequence of these invocations across a request's lifecycle. True real-time co-editing is a future evolution.

---

## Architecture

### How a Session Works

1. Operator invokes `moe work <project> <request> <document>`
2. `moe` inits the target submodule if the session needs code access; otherwise no submodule work
3. `moe` assembles the prompt from `soul.md`, applicable stage/doc fragments, overrides, upstream documents, current content
4. `moe` invokes `claude -p` with the assembled prompt, the document's per-document UUID `--session-id`, `--allowedTools`, and the document's configured model
5. `moe` streams Claude Code's output to the operator and appends to `thread.jsonl`
6. When Claude Code finishes, `moe` commits the resulting changes (document content.md, request.json status update, and any target-submodule pointer updates) on `main` with structured trailers
7. Operator inspects and decides the next move: continue, advance to another document, edit manually, review, approve, scrap

### Process Model

**The human is the scheduler.** One agent at a time under the operator's attention. Multiple terminal sessions work in parallel across different requests — they don't touch each other on the bureaucracy side because all commits land on `main` with distinct trailers.

The contention point is the submodule checkout under `projects/<project>/`. Concurrent implementation sessions on the same project are a `moe work --worktree` opt-in; the default flock at `.moe/locks/<project>.lock` keeps two implementation turns from stepping on each other's submodule state. Document-only sessions don't touch the submodule and run freely in parallel. See the **Git Model** section for the concrete mechanism. Docker or SSH wrappers remain a Phase-5 option for implementation isolation but aren't the concurrency mechanism.

**Cross-document negotiation is operator-mediated.** When the architecture session pushes back on the spec ("batch size 10,000 needs different storage"), the operator carries that pushback into the spec conversation. No automated inter-agent messaging. A future evolution could add a `for` loop in `moe work --negotiate` to iterate across documents until settled or max iterations — it's a `for` loop, not a DAG engine.

### Technology

CLI: Go stdlib. Persistence: git + committed JSON/JSONL + INI + block-format config. Agent backend: Claude Code headless (`claude -p`). Sandbox for implementation work: `os/exec` shells out to `docker`/`ssh`/`daytona` when needed. Everything else runs in the operator's local shell.

### Git Model

**Two repos, two audiences.** The bureaucracy repo (back office) contains guidance fragments, request state, run history — all on a single `main` branch, scoped per-request via commit trailers. Target project repos (front office) are git submodules — clean code, no MoE artifacts; their own branch model lives there, where code review actually happens via PRs. The `moe` CLI itself lives in its own repo — tool and state are separate. See the **Hierarchy** and **Git Model** subsections for the full picture.

---

## Bootstrap Plan

The Ministry's first project is itself. Build the minimum that can manage one request end-to-end, then dogfood.

### Phase 0: Workspace Scaffolding

- [ ] `moe init` scaffolds the directory structure (`stages/`, `docs/`, `projects/`, `requests/`, `soul.md`, `agents.conf`, `document-graph.conf`)
- [ ] Write initial `soul.md` and one or two fragments (`stages/design.md`, `docs/spec.md`)
- [ ] Seed `document-graph.conf` with pitch, spec, architecture, implementation, test-plan, deploy-plan
- [ ] Seed `agents.conf` with the minimal per-doc model + tools entries
- [ ] `moe project add` adds a submodule under `projects/` and scaffolds `requests/<project>/project.json`

### Phase 1: Single-Document End-to-End

- [ ] `moe request new <project> "title" [--id slug]` scaffolds `request.json` and commits it on main with the request's trailers
- [ ] `moe work <project> <request> spec` — prompt assembly, `claude -p` invocation, streaming, commit with trailers
- [ ] `moe status` reads `request.json` glob, renders with `tabwriter`
- [ ] `moe review` filters `git log --grep="MoE-Request: <id>"` and renders each document's current content
- [ ] Run the loop manually against a real request. Iterate on the guidance fragments.

### Phase 2: Full Request Lifecycle

- [ ] `moe sign pr` — commit+push target submodule, generate derived artifacts, flip request status (all on main)
- [ ] `moe scrap` — record rationale, flip request status (all on main)
- [ ] `moe history` — `git log` aggregation with cost totals from commit trailers
- [ ] Additional per-doc fragments (implementation, test-plan, deploy-plan)
- [ ] Project-level overrides

**Self-hosting checkpoint**: MoE manages its own development via `moe work`.

### Phase 3: Document Graph Mechanics

- [ ] `internal/graph/` module: adjacency maps, staleness propagation, upstream collection, topological sort
- [ ] `moe status` shows stale documents with reasons
- [ ] `moe work` auto-collects upstream documents (tagged with stage-signed state) as context
- [ ] Ripple summary in `moe review`

### Phase 4: Derived & Persistent Documents

- [ ] `docs/derived.md` guidance for generating release notes, decision logs, etc.
- [ ] `moe sign pr` walks the graph and generates each derived node with satisfied deps
- [ ] Persistent product-level documents (changelog, decision log, API docs, etc.) updated incrementally at approve
- [ ] Index file maintenance (`moe reindex` rebuilds from scratch; normal operation keeps them in sync)

### Phase 5: Code Editing

- [ ] `docs/implementation.md` with initializer/continuation pattern guidance
- [ ] `features.json` session continuity
- [ ] `moe work <project> <request> implementation` with expanded `--allowedTools` scoped to `projects/$target`
- [ ] Submodule commit/push/PR flow at `moe sign pr`
- [ ] Optional: Docker/SSH wrapper around `claude -p` for isolation

**Self-hosting checkpoint**: MoE can plan *and* execute its own development.

### Phase 6: Ops Hooks (Future)

- [ ] `docs/ops.md` / `stages/*` guidance for monitoring-driven requests
- [ ] External triggers (cron, alerts) that spawn requests — **any trigger without a human at the keyboard must route to the Claude API under Commercial Terms, not Claude Code headless.** See Review Notes.
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
| Automated DAG execution across documents | Deferred | Human sequences work. A `for` loop in `moe work --negotiate` handles cross-doc iteration if needed. |
| Web UI with real-time streaming | Deferred | Read-only layer over git state, `net/http` + SSE. Post-CLI-stabilization. |
| Scheduled runs / ops alert triggers | Deferred to Phase 6+ | Must route to Claude API backend, not Claude Code headless (compliance). |
| Parallel agent execution | Deferred | Multiple terminal sessions cover current needs. Goroutines + `sync.WaitGroup` when automated. |
| DuckDB / SQL-queryable run history | Deferred | `git log` + commit trailers cover v1. Shell out to `sqlite3` or DuckDB later if needed. |
| Anthropic Managed Agents backend | Rejected for core, narrow-yes for implementation work | See Review Notes. |
| Managed sandbox providers (Docker, Daytona) | Deferred | `os/exec` wrapper around `claude -p` is sufficient when needed. |
| Clerk as LLM agent | Deferred | Human operator is the Clerk in v1. |
| Cross-project knowledge queries | Deferred | Emerges naturally once persistent documents accumulate. |
| Explicit knowledge graph files | Deferred | Plain text JSON in `knowledge/` when the pattern stabilizes. |
| "Yolo mode" auto-merge | Deferred | Every request passes a human review gate in v1. |
| Interactive TUI | Deferred | One-shot `moe dash` covers the prioritization/resumption problem. Revisit if navigation friction justifies a Bubble Tea or raw-termios TUI — but that would breach the stdlib-only stance, so only if the one-shot dashboard is demonstrably insufficient. |

---

## Review Notes

_Open items from spec review — April 2026._

**[Q] Cascading partial-context degradation.** Disjunctive triggers at `moe sign pr` mean a bug fix with only an implementation doc can cascade thin derived artifacts through to persistent product docs. Not blocking — graceful degradation is the right default — but worth monitoring. If persistent doc quality drifts, consider adding a minimum-context threshold or human review gate for derived docs generated from a single weak input.

**[Q] Anthropic Managed Agents — evaluated, rejected for core, narrow-yes for implementation.** [Anthropic Managed Agents](https://www.anthropic.com/engineering/managed-agents) (launched April 2026) provisions a per-session container as the agent's workspace, with Anthropic running the agent loop on its orchestration layer. Evaluated against MoE and rejected for the document-centric core because it conflicts with four design principles: (1) it's per-token API billing — the opposite of the Claude Code headless cost model; (2) it's Claude-only and first-party-only, violating model-agnosticism; (3) it keeps session state server-side via SSE, violating "repo is the source of truth"; (4) it duplicates sandbox functionality already covered by `os/exec` wrappers. Beyond principles, the vast majority of MoE's workload is document-editing sessions that only read the repo and write one markdown file — none of which benefit from a server-hosted container with bash/code execution. **Narrow-yes case:** Phase 5 implementation work could treat Managed Agents as one sandbox option alongside a Docker/SSH wrapper, selected per invocation. Not a priority; revisit only if hosted-for-clients scenarios emerge where Max/Pro subscriptions don't transfer.

**[Q] Subscription vs. API backend routing — compliance constraint.** Anthropic's Consumer Terms (clarified Feb 2026, enforced April 4, 2026) permit Claude Code headless under a Pro/Max subscription **only for interactive, operator-driven runs** — a human kicking off `moe work` from their own terminal under their own Claude Code install. Scheduled runs, ops-triggered runs, multi-tenant deployments, always-on services, and anything resembling production automation must route through the Claude API under Commercial Terms, regardless of cost. This is a bright line — Anthropic actively detects and blocks the forbidden pattern (OpenClaw was cut off on April 4, 2026). Resolve before Phase 6 (the first place scheduled triggers appear). **Additional constraints regardless of trigger:** never read `~/.claude` auth material from `moe`, never re-use Claude Code's OAuth tokens against the API, never pipe Pro/Max credentials through the Anthropic SDK. **Phase 7 open-source framing:** the README must not encourage new users to point subscription-backed automation at employer codebases — the public framing is "interactive use under your own Claude Code install, or API keys for everything unattended."

---

## Open Questions

1. **Multi-user future**: The initial design is single-operator. If consulting clients want visibility, do they get read-only access via the eventual web UI? Worth modeling in the data layer even if not built yet.

2. **Request dependencies**: Requests can spawn other requests. How are cross-request dependencies tracked? Likely lightweight — a `spawned_from` field in `request.json` with a reference to the source request. Blocking dependencies (request B can't start until request A merges) can be enforced by `moe work` refusing to advance B's documents until A is approved.

3. **When does the Clerk become an agent?** Document the heuristic for promoting the human Clerk to an LLM agent (e.g., "after N requests of the same shape, propose a template; after M successful template uses, propose automating the template selection"). Avoids premature automation and makes the path visible.

---

## References

- [OpenAI: Harness Engineering](https://openai.com/index/harness-engineering/) — Codex team's methodology for agent-first development
- [Anthropic: Effective Harnesses for Long-Running Agents](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents) — Initializer/coder pattern, progress files, multi-session continuity
- [Gas Town](https://github.com/steveyegge/gastown) — Multi-agent orchestration with Git-backed state (Steve Yegge)
- [OpenClaw](https://github.com/openclaw/openclaw) — Autonomous agent framework (Peter Steinberger); the pattern Anthropic's Feb 2026 Consumer Terms clarification targeted
- [Google Wave](https://en.wikipedia.org/wiki/Google_Wave) — The original "equal parts conversation and document" platform
- [Martin Fowler: Harness Engineering](https://martinfowler.com/articles/exploring-gen-ai/harness-engineering.html) — Analysis of harness patterns and categories
- [The Emerging Harness Engineering Playbook](https://www.ignorance.ai/p/the-emerging-harness-engineering) — Cross-cutting patterns from OpenAI, Stripe, and OpenClaw
