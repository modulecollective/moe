# Ministry of Everything (MoE)

**A bureaucracy-themed agent harness for the full lifecycle of anything.**

Module Collective LLC · April 2026

---

## Vision

The Ministry of Everything is a CLI-first agent harness where a human operator collaborates with AI agents through threaded conversations attached to living documents. It manages the full lifecycle of products and projects — from ideation through design, implementation, deployment, and operations — through a document graph that ripples changes between interconnected artifacts.

The core insight: **the document and the conversation about the document are the same thing.** The spec *is* the conversation with the Dept. of Ideas. The architecture *is* the threaded discussion with the Dept. of Architecture. Documents compress conversations into clean artifacts that become context for downstream departments.

The Ministry is designed for a single operator managing multiple products with agent assistance. It is domain-agnostic — software development is the first ministry to open, but the same machinery handles any workflow that produces interconnected documents. The bureaucracy is the feature.

**"Please take a number."**

### Design Principles

1. **The human is the workflow engine.** No orchestrator, no DAG executor, no scheduler. The operator looks at the request, decides what to work on next, and tells `moe` to do it. A small CLI assembles prompts, invokes Claude Code, and tracks state in git. The harness gets out of the way.
2. **The repo is the source of truth.** All state — documents, decisions, conversations, progress — lives in git. Nothing lives in Slack, Google Docs, or people's heads. The bureaucracy repo is the "back office" (workflows, handbooks, run history). Target project repos stay clean. The `moe` CLI lives in its own repo — tool and state are separate so the CLI can be open-sourced without leaking private bureaucracy contents.
3. **Agents are participants, not tools.** Agents join conversations, contribute to documents, and have defined roles — organized into departments with handbooks.
4. **Requests terminate, products persist.** Units of work (requests) have a lifecycle and end. Products are the long-lived entities that accumulate completed work.
5. **Loose recipes, deterministic mechanics.** The document graph defines known document types and typical relationships as guidance. The operator assembles whatever subset fits the situation. Within a request, staleness propagation and upstream-context assembly are deterministic — given the graph and the events, the state is always derivable.
6. **Model-agnostic.** The harness works with Claude Code, Ollama/Qwen, Codex, or any LLM backend. A simple department → model config routes each department to the right model. The harness is the moat, not the model.
7. **Minimize entropy.** Every agent mistake becomes a handbook update. The system improves every time you use it.
8. **One repo, one history.** All request state and run metadata live in the bureaucracy repo. Branches (`moe/<project>/<request>`) accumulate as the operational audit trail. Queryable with `git log` and, later, SQL tools over exported JSON if needed.
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
├── departments/                   # Markdown handbooks (ideator, architect, coder, …)
├── departments.conf               # Department → model + allowedTools (INI)
├── document-graph.conf            # Document type library (block format)
├── projects/                      # Git submodules pointing at target repos
│   ├── telomere/                  # → github.com/modulecollective/telomere
│   └── next-idea/                 # → github.com/modulecollective/next-idea
├── requests/                      # Request state, document graph artifacts
│   ├── telomere/
│   │   ├── project.json           # Project metadata
│   │   ├── overrides/             # Project-level department overrides (optional)
│   │   └── runs/
│   │       ├── add-batch-support/ # in_progress
│   │       ├── fix-timeout-bug/   # review
│   │       ├── mvp-build/         # deployed
│   │       └── websocket-eval/    # denied
│   └── next-idea/
│       └── …
└── Request branches (moe/<project>/<request>) — the operational audit trail
```

### Two Repos: CLI and Bureaucracy

MoE is split across two independent git repos, cloned side-by-side, in the same relationship as `git` ↔ a repository or `hugo` ↔ a site:

- **`moe/`** — the CLI repo. Go source for the `moe` binary. No private data, no pointer to the bureaucracy. Open-source-eligible. Installed to `$PATH` like any other tool.
- **`bureaucracy/`** — the private state repo, cloned at the same level as `moe/`. Holds handbooks, department config, the document graph definition, `requests/`, run history, and `projects/*` submodules pointing at target repos. The `moe` binary operates on whichever bureaucracy directory it's invoked from (discovered via `$PWD` walk or `$MOE_HOME`).

Upgrading `moe` is a `go install`; the bureaucracy is untouched. Matches principle 11 — the CLI is just a tool on `$PATH`.

### The Bureaucracy Repo

The bureaucracy repo holds handbooks, the document graph definition, request state, and run history across all projects. There is one unified history — one set of request IDs, one `git log`. Dashboards, portfolio views, and cross-project queries are all views over the same repo.

### Project (Target Repo)

A long-lived entity representing a software product or project. Registered as a git submodule under `projects/`. Born from `moe add-project <repo-url>`, persists as long as the project is managed.

**Target project repos are registered as git submodules under `projects/`.** The bureaucracy repo stores all orchestration metadata — handbooks, request state, run history. The target repo stores only its own code. This separation means:

- Projects can pre-exist MoE. Fork an interesting open source project, `moe add-project` it, and start managing requests against it.
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

During a session, `moe work` runs `git submodule update --init projects/$target` so agents can read code and (for the coder department) edit it. Code changes result in submodule pointer updates committed to the bureaucracy request branch. At approval time, the changes inside the submodule are committed, pushed to the target remote, and optionally opened as a PR — the submodule pointer update in the bureaucracy records which target commit was produced.

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
  "branch": "moe/telomere/fix-timeout-bug",
  "created": "2026-04-12",
  "origin": "ops_alert",
  "priority": "high",
  "documents": {
    "spec":           { "agent": "ideator",  "status": "ok",        "session": "moe/telomere/fix-timeout-bug/spec" },
    "architecture":   { "agent": "architect","status": "draft",     "session": "moe/telomere/fix-timeout-bug/architecture" },
    "implementation": { "agent": "coder",    "status": "pending",   "session": "moe/telomere/fix-timeout-bug/implementation" }
  }
}
```

Statuses: `in_progress | review | approved | denied | deployed | scrapped`. Document statuses: `pending | draft | in_review | ok | stale`.

**Document-level status is a soft gate set by the operator.** After a `moe work` turn the operator runs `moe ok <project> <request> <document>` to flip a document from `draft`/`in_review` to `ok` — signaling "I'm happy with this; downstream agents can treat it as settled context." `moe ok` is the per-document counterpart to `moe approve` (the request-level hard gate). No agent invocation, no merge — just a status update committed to the request branch. `moe unok` reverses it. Downstream agents consume upstream documents that are `ok`; documents that are still `draft` flow as "current content, not settled" with a warning in the assembled prompt.

**Request progress is determined by its documents, not a rigid phase sequence.** A request is "in progress" as long as any document is in draft or review. It's ready for final review when all required documents are `ok`. Small bug fixes might only have an implementation document. Large features might have spec, architecture, implementation, test plan, deploy plan, and ancillary documents.

The document graph within a request defines the natural ordering:

| Document | Typical Agent | Upstream Dependencies |
|----------|---------------|----------------------|
| Spec | Ideator | (none — origin) |
| Architecture | Architect | Spec |
| Implementation Plan | Coder | Architecture, Spec |
| Test Plan | Reviewer | Spec, Implementation |
| Deploy Plan | Deployer | Implementation |
| (Ancillary) | (varies) | (configurable) |

Phase transitions are **implicit** — they happen when the operator marks documents `ok` and starts drafting their downstream. `moe ok` is the soft gate; `moe approve` at the request level is the hard gate.

**Request rollup on completion:**

When `moe approve` runs:
- Code is merged to the product repo's main branch (for implementation requests)
- Derived artifacts are generated (release notes, decisions, dependencies, etc.) — see below
- Product-level persistent documents are updated
- The request branch is merged into MoE's `main`; the request is marked complete but remains browsable for history

When `moe scrap <request> "reason"` runs:
- Decision rationale is recorded as a derived artifact
- Request remains in the product's history as institutional memory
- No code or config changes are applied

### Derived Artifacts

When a request is approved, downstream nodes in the document graph are triggered — both request-level derived documents and product-level persistent documents. See **The Document Graph** section under Document for the full picture. Derivation is explicit: `moe approve` walks the graph, identifies which derived/persistent nodes have their dependencies satisfied, and invokes the archivist to produce each in turn. Nothing runs automatically in the background.

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

**Document lifecycle — one request branch per request:**

All agents in a request work on a **single request branch** (`moe/<project>/<request>`). The approved state is what's on `main`. The request branch is the work-in-progress. Review is a diff of the entire request branch against `main`. Approval merges the whole thing.

```
main (approved state)
  └── moe/telomere/add-batch-support (the request branch)
       ├── commit: "ideator: draft initial spec"
       ├── commit: "ideator: add batch size limits per conversation"
       ├── commit: "architect: draft system design for batch support"
       ├── commit: "architect: revise storage approach after pushback from coder"
       ├── commit: "coder: draft implementation plan and task breakdown"
       └── commit: "reviewer: draft test plan with acceptance criteria"
          → human reviews full diff against main
          → approves → merge to main
```

Each commit carries structured trailers so history can be filtered programmatically:

```
ideator: draft spec for batch operations

MoE-Request: add-batch-support
MoE-Document: spec
MoE-Agent: ideator
MoE-Session: moe/telomere/add-batch-support/spec
```

This is a key simplification. The internal ripple — agents cascading through the document graph, pushing back on each other, iterating — all happens on the request branch. Document statuses (draft, `ok`, stale) are soft coordination signals set by the operator, not merge gates. The human review point is the request as a whole at `moe approve` time.

**The ripple is operator-driven, not background.** Agents only act when `moe work` is invoked. "Rippling through documents" means the operator walking the graph — typically via `moe next`, which dispatches the obvious action for each attention-triggering doc. There is no background worker; there is a human pressing Enter through an attention queue.

- **In progress**: The operator is invoking `moe work …` against documents on the branch. The operator can watch, converse, steer between invocations.
- **Ready for review**: Documents have settled. The full diff is ready.
- **Approved**: `moe approve` merges the branch to main, runs derived-artifact generation, and updates persistent product docs.

Human review of the request diff is required by default. A future "yolo mode" can auto-merge for well-understood request types where the agents consistently produce good output.

**Ripple flow in practice:**

1. You describe what you want in the spec thread (`moe work telomere add-batch spec`)
2. The ideator agent drafts the spec, commits to the request branch
3. `moe work telomere add-batch architecture` — the architect agent reads the spec, drafts the architecture, commits
4. `moe work telomere add-batch implementation` — the coder agent reads both, drafts the implementation plan, commits
5. The coder pushes back on the architecture — you tell the architect "the coder says the interface needs pagination, reconsider". The architect revises, commits. The coder updates its plan, commits.
6. Everything settles. `moe review telomere add-batch` — you review the full request diff against main.
7. `moe approve` — one merge. Derived artifacts generate. Done.

For well-scoped requests with good guidance files, this converges quickly. You start a conversation, the agents ripple through the documents you drive them through, and you come back to a complete package. One conversation, one review, one merge.

No custom versioning needed. Diff is `git diff main...moe/<project>/<request>`. Rollback is `git revert`. History is `git log` on the request branch. Rewind is `git reset --soft`. Fork is `git branch moe/<project>/<request>-v2 moe/<project>/<request>`.

**Document types** are defined in the **document graph** (see below) — a workspace-level library of known document types, their typical relationships, and their agents. This is a parts bin, not a rigid structure. The operator assembles whatever subset and wiring fits each request.

**Ancillary documents** can be added to any request for cross-cutting concerns. They participate in the request's ripple graph like any other document.

```json
// requests/telomere/runs/add-batch-support/documents/spec/document.json
{
  "id": "spec",
  "type": "spec",
  "title": "Batch Operations Spec",
  "agent": "ideator",
  "status": "ok",
  "version": 3,
  "upstream": [],
  "downstream": ["architecture", "test-plan"],
  "content_file": "content.md"
}
```

**Ripple mechanism:**

When a document is updated on the request branch, `moe status` reports all transitive downstream documents as `stale`. Staleness is derived from the graph + the branch's commit history — nothing is mutated in the background. The operator decides when to reconcile:

```
Spec updated (v3: added batch ops)
  → Architecture [STALE: spec changed, revise next]
      → Implementation Plan [implicitly stale, waiting on architecture]
      → Test Plan [STALE: spec added acceptance criteria]
```

The operator controls the pace. You can reconcile downstream docs immediately (`moe work …`), let them sit stale while you focus on something else, or decide a particular downstream doesn't need to be touched.

**Cross-document agent collaboration:**

A downstream agent reviewing an upstream change can flag concerns, which the operator carries back into the upstream document's conversation. For example: the architect reads a spec change, responds "A batch size of 10,000 will require a completely different storage architecture — can we cap at 1,000?" — you copy that pushback into the spec thread (`moe work telomere add-batch spec`) with "the architect says …", the ideator responds, and you review the result. The operator is the messenger; the agents don't talk to each other directly.

This turns the document graph into a genuine collaborative workspace rather than a one-way waterfall. Upstream docs get better because downstream agents pressure-test them — the same way a good implementation engineer pushes back on a design that won't hold up. Your role as operator is to arbitrate and mediate, which you'd do anyway.

**Ripple summary view:**

Since all agents work on a single request branch, the ripple summary is just the request diff organized by document. `moe review` shows:

- The request branch diff against main, grouped by document (spec changes, architecture changes, implementation changes, etc.)
- Summary of recent per-document commits
- Unresolved staleness: "architecture has been touched since the test plan was ok'd"

This is the primary review surface. One diff, organized so you can see the coherent picture across all documents. When it looks good, one merge.

**The Document Graph:**

The workspace defines a **document graph** — a library of known document types, their typical relationships, and their behavior. This is **guidance, not a rulebook**. It represents institutional knowledge about what good process looks like — the forms the Ministry knows how to handle.

It spans three scopes:

1. **Request documents** (conversational): You chat with agents to produce them. Spec, Architecture, Implementation, etc. These live on the request branch and merge with the request.
2. **Request-level derived documents** (auto-generated at approval): Structured data and summaries extracted from the request's work. Generated by `moe approve`, committed to the request.
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
    agent         ideator
    scope         request
    description   "One-pager. What is this, why does it matter, who is it for."
}

spec {
    type          conversational
    agent         ideator
    scope         request
    depends_on    pitch
    description   "Requirements, acceptance criteria, constraints."
}

architecture {
    type          conversational
    agent         architect
    scope         request
    depends_on    spec
    description   "System design, interfaces, tradeoffs, ADRs."
}

implementation {
    type          conversational
    agent         coder
    scope         request
    depends_on    architecture, spec
    description   "Task breakdown, code, tests, CI config."
}

test-plan {
    type          conversational
    agent         reviewer
    scope         request
    depends_on    spec, implementation
    description   "Test strategy, cases, coverage."
}

deploy-plan {
    type          conversational
    agent         deployer
    scope         request
    depends_on    implementation
    description   "Deploy steps, migrations, rollback."
}

security-review {
    type          conversational
    agent         reviewer
    scope         request
    depends_on    architecture, spec
    description   "Threat model, attack surface, mitigations."
}

migration-plan {
    type          conversational
    agent         architect
    scope         request
    depends_on    architecture
    description   "Migration steps, data strategy, rollback."
}

cost-analysis {
    type          conversational
    agent         ideator
    scope         request
    depends_on    spec
    description   "Infrastructure costs, API costs, ROI."
}

# === Request-level derived documents (generated at moe approve) ===
# Triggered when ANY dependency is present. Agent handles partial context gracefully.
# All use agent: archivist.

release-notes {
    type          derived
    agent         archivist
    depends_on    spec, implementation
    format        markdown
}

decisions {
    type          derived
    agent         archivist
    depends_on    spec, architecture, implementation
    format        json
}

references {
    type          derived
    agent         archivist
    depends_on    spec, architecture
    format        json
}

dependencies {
    type          derived
    agent         archivist
    depends_on    implementation
    format        json
}

api-changes {
    type          derived
    agent         archivist
    depends_on    implementation, spec
    format        json
}

# === Product-level persistent documents (updated at moe approve) ===

product-changelog {
    type          persistent
    agent         archivist
    depends_on    release-notes
    format        markdown
}

product-decision-log {
    type          persistent
    agent         archivist
    depends_on    decisions
    format        json
}

product-api-docs {
    type          persistent
    agent         archivist
    depends_on    api-changes, spec
    format        markdown
}

product-user-guide {
    type          persistent
    agent         archivist
    depends_on    spec, release-notes
    format        markdown
}

product-architecture-overview {
    type          persistent
    agent         archivist
    depends_on    architecture
    format        markdown
}

product-dependency-manifest {
    type          persistent
    agent         archivist
    depends_on    dependencies
    format        json
}

ops-runbook {
    type          persistent
    agent         archivist
    depends_on    deploy-plan
    format        markdown
}
```

This is the parts bin plus the mechanical layer. For each request, the operator selects conversational nodes and wires them. When `moe approve` runs, derived nodes generate and persistent product-level nodes are incrementally updated. The mechanical parts (derived, persistent) trigger deterministically. The creative parts (conversational) are assembled by judgment.

**Entry points — a request can start from anything:** A pitch, a bug report, a customer request, a screenshot, a code snippet, or nothing at all. The operator looks at the input and picks the right set of documents.

**The Clerk (operator role):**

The Clerk is the role that decides what a request needs. **Initially, this is the human operator.** When you start a request with `moe new <project> "description"`, you read `document-graph.conf` for guidance, look at your input, and make an engineering judgment call about what documents this request needs. Common shapes:

- "Add batch support to the timeout API" → Pitch → Spec → Architecture → Implementation → Test Plan → Deploy Plan
- "Fix the health check timeout bug" → Implementation → Test Plan (no spec or architecture needed)
- "I just want to think through a storage architecture" → Architecture (single doc, no dependencies)
- "Explore three competing approaches to caching" → three Architecture docs, no dependencies between them
- "I have an idea for a new caching layer" → Pitch (just capture it)

Templates can be saved as named presets for common shapes, but they're just `moe new` flags that pre-populate the documents list — not a separate system. A future evolution automates the Clerk as an LLM agent that reads the input and proposes the document set. For v1, the human is the Clerk.

Derived and persistent documents are triggered based on which conversational documents the request produced. If the request has an implementation document, the dependency manifest gets generated. If the request has a spec and implementation, the API docs get updated. All of this runs at `moe approve` time.

**Two layers — flexible planning, deterministic mechanics.** The operator decides what documents a request needs (flexible, informed by the graph but not bound by it). Once the documents exist, staleness propagation and upstream-context assembly are deterministic.

**`depends_on` wears two hats, and the boundary matters.** On conversational nodes in `document-graph.conf`, `depends_on` is *guidance for the Clerk* selecting a doc set for a new request — "if you pick a spec, you probably want an architecture downstream." Once the operator commits to a doc set, those same edges become *mechanical* within the request: staleness BFS and upstream-context DFS both walk them. On derived/persistent nodes, `depends_on` is always mechanical — it determines which archivist runs fire at `moe approve`. Rule of thumb: graph-level edges are guidance for *future* requests; within a live request the graph is concrete.

**Structured data:** Derived documents can be prose (markdown) or structured (JSON). Over time, product-level persistent documents accumulate structured data across all requests — "which products depend on this library?" becomes a query across `product-dependency-manifest` files. The knowledge graph emerges from shipping requests.

**Index files:**

Agents and tools need to understand workspace state without parsing every file in the tree. Index files are committed JSON files that provide a pre-computed view of the workspace — the "table of contents" — an agent or CLI reads one file to know what exists, what's active, and where to look deeper.

Two levels:

1. **`index.json`** (workspace root) — all projects, their statuses, and counts of active/completed/denied requests per project. This is what `moe status` reads to summarize the landscape.

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
  "completed_requests": 12,
  "denied_requests": 1,
  "persistent_docs": {
    "changelog": { "updated": "2026-03-01" },
    "api_reference": { "updated": "2026-02-28" },
    "architecture_overview": { "updated": "2026-02-15" },
    "dependency_manifest": { "updated": "2026-03-01" }
  }
}
```

Index files are maintained by `moe` — updated on every request state change (creation, approval, denial). They are deterministically derivable by globbing `requests/*/runs/*/request.json`, so `moe reindex` rebuilds them from scratch if they drift. Because `moe` is the only writer under normal operation, they stay in sync.

The key constraint: **no binary databases, no SQLite, no caches that live outside git.** Index files are plain text, committed, diffable, and version-controlled like everything else. An agent or human can `cat index.json` and know the state of the world. Git blame shows who changed what and when. This is consistent with the "repo is the source of truth" principle — if it's not in git, it doesn't exist.

**Future: knowledge graph as text files.** A future evolution adds explicit knowledge graph files (`knowledge/entities.json`, `knowledge/relationships.json`) — committed JSON tracking semantic relationships across documents and products. Enables content lineage tracing and cross-product queries. Same rules as index files: plain text, committed, diffable, rebuildable.

**Growth:**

The document graph evolves. You start simple and grow it as your practice matures. Add "Performance Analysis" as a conversational node when you find yourself needing it. Add "License Audit" as a derived node when you want automated compliance checks. Products can have graph overrides that add domain-specific document types. The graph is versioned in git like everything else.

### Thread

A conversation within a document. The primary mechanism is **Claude Code's native session**: each document maps to one `--session-id`, and multi-turn continuity is server-side. `moe work telomere add-batch spec` invoked a second time resumes that session — the agent remembers the prior turns.

For auditability, each `moe work` invocation appends a JSONL record to the document's local thread log:

```jsonl
{"id": "blip-001", "author": "james", "type": "human", "ts": "…", "content": "We need to support batch operations for the timeout API. Users are calling it in a loop and hitting rate limits."}
{"id": "blip-002", "author": "ideator", "type": "agent", "ts": "…", "content": "That makes sense. I'd suggest a POST /batch endpoint that accepts an array of timeout configs. A few questions: should batch operations be atomic (all-or-nothing) or partial (best-effort)?"}
{"id": "blip-003", "author": "james", "type": "human", "ts": "…", "parent": "blip-002", "content": "Partial. Return individual results for each item."}
{"id": "blip-004", "author": "ideator", "type": "agent", "ts": "…", "content": "Got it. I've updated the spec document with the batch endpoint definition, partial semantics, and new acceptance criteria.", "document_version": 3}
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

Ideator agent receives:
  - Guidance: soul.md + departments/ideator.md + project overrides
  - Request description (lightweight)
  - Product-level context (project.json, existing architecture)

Architect agent receives:
  - Guidance: soul.md + departments/architect.md + project overrides
  - Upstream Spec document (current content on the request branch; marked "settled" if ok, "draft" otherwise)
  - Product-level context
  - Existing codebase structure from product repo

Coder agent receives:
  - Guidance: soul.md + departments/coder.md + project overrides
  - Upstream Architecture + Spec documents (current content, status-tagged)
  - Relevant source files from product repo
  - progress.json from prior sessions

Reviewer agent receives:
  - Guidance: soul.md + departments/reviewer.md + project overrides
  - Upstream Spec + Architecture (status-tagged)
  - Code diffs from implementation
  - Test results
```

Each agent gets guidance files plus dense, current documents rather than raw conversation history. Upstream content is always the document on the request branch, tagged with its soft status (`ok` or `draft`) so the agent knows whether the operator has blessed it. This keeps context windows focused and signal-rich. Thread logs are available for human review but are not fed to downstream agents unless explicitly requested.

**Session-expiry fallback.** Multi-turn continuity relies on Claude Code's server-side session (keyed by `--session-id`). If a resume returns an empty or missing session (rotation, expiry, or a different machine), `moe work` falls back to injecting the last N turns from `thread.jsonl` as a compact recap in the user message, then continues. The audit log is the durable record; the server session is an optimization.

---

## Agent Architecture

### Agent Guidance Layer

Agent behavior is controlled by **markdown files in the repo** — the same files agents work with, reviewed and versioned the same way. This is the harness. Three layers, most specific wins:

```
moe/
├── soul.md                        # Global: philosophy, quality bar, tone
├── departments/                   # Department handbooks
│   ├── ideator.md                 # Role: how to write specs, what to explore
│   ├── architect.md               # Role: design principles, preferred patterns
│   ├── coder.md                   # Role: coding standards, test expectations
│   ├── reviewer.md                # Role: review criteria, what to flag
│   ├── deployer.md                # Role: deploy procedures, safety checks
│   ├── ops.md                     # Role: monitoring philosophy, alert triage
│   └── archivist.md               # Role: how to write release notes, decision logs, etc.
├── departments.conf               # Department → model + allowedTools (INI)
├── requests/
│   ├── telomere/
│   │   ├── overrides/             # Project overrides (optional)
│   │   │   └── coder.md           # "Use Deno, not Node. Deploy to Fly.io."
│   │   └── runs/
│   │       └── add-batch-support/
│   │           └── overrides/     # Request overrides (rare, optional)
│   │               └── coder.md   # "This request touches billing, extra caution"
```

**Layer resolution:** request-level → project-level → global. Same pattern as gitconfig, CSS specificity, or Consul's config hierarchy. `moe work` concatenates whichever override files exist in most-specific-first order.

**`soul.md`** is the equivalent of Codex's AGENTS.md or Claude Code's CLAUDE.md. It's the general guidance every agent gets: your engineering philosophy, how you like things communicated, quality standards, what to escalate vs. decide autonomously. This is the document that captures your engineering judgment in a form agents can consume.

**Role-specific files** define how each agent type should approach its work. The ideator file might say "always identify at least three risks and ask about edge cases before drafting." The coder file might say "write tests first, prefer composition over inheritance, keep functions under 30 lines."

**Project overrides** let you tailor agent behavior per project. A prototype might have relaxed standards. A fork of a safety-critical system might have stricter review requirements.

Because these are markdown files in git, they evolve through the same mechanism as everything else. An agent makes a recurring mistake → you update the guidance file → that mistake doesn't happen again. **The harness improves every time you use it.** Agents can even propose updates to their own guidance files (on a branch, reviewed as a diff, just like any other document change).

### Agent Roles

All roles in the Ministry at a glance:

| Role | Ministry Name | Scope | Responsibilities | Key Permissions |
|------|--------------|-------|-----------------|-----------------|
| **Clerk** | The Clerk | Request lifecycle | Decides what documents a request needs, which to work on next, and when to mediate cross-document pushback. **Initially the human operator.** | Human (v1); future LLM agent (read repo) |
| **Ideator** | Dept. of Ideas | Pitch, Spec, Cost Analysis | Expand rough ideas into specs, identify risks, define acceptance criteria, research prior art | Read repo, web search, edit own content.md |
| **Architect** | Dept. of Architecture | Architecture, Migration Plan | Design system structure, define interfaces, evaluate tradeoffs, produce ADRs | Read repo, web search, edit own content.md |
| **Coder** | Dept. of Implementation | Implementation Plan | Write code, tests, CI config. Follows initializer/coder pattern for multi-session work | Read repo, web search, read/write product repo, shell exec, edit own content.md |
| **Reviewer** | Dept. of Review | Test Plan, Security Review | Validate code against spec, run tests, check for regressions, security audit | Read repo, web search, read product repo, edit own content.md |
| **Deployer** | Dept. of Deployment | Deploy Plan | Execute deploy pipeline, run migrations, validate health checks | Read repo, web search, scoped shell exec, edit own content.md |
| **Ops** | Dept. of Operations | Product-level | Monitor health, triage alerts, spawn incident requests, maintain runbooks | Read repo, web search, read product repo, edit own content.md |
| **Archivist** | Dept. of Records | Derived artifacts, ripple summaries | Generate release notes, decision logs, reference lists, dependency manifests at `moe approve`. Synthesize ripple summaries for review view. Organized as `departments/archivist/<node-id>.md` sub-handbooks so each derived/persistent doc type gets its own focused prompt, sharing the department's model + tool config. | Read repo |

**Cross-cutting meta-departments** manage the bureaucracy itself:

- **HR** (Dept. of Human Resources): Model selection and routing lives in `departments.conf`. A future evolution reviews historical agent performance and proposes config updates — itself a request with its own workflow.
- **Finance** (Dept. of Finance): Cost tracking is per-invocation. Each `moe work` run records the session's cost (from Claude Code's output) in the commit trailer or a sidecar file. `moe history` aggregates.
- **QA** (Dept. of Quality Assurance): Cross-request quality monitoring. Tracks patterns in review feedback and rework. Feeds back into department handbooks. Runs as its own request against the bureaucracy repo itself.

### Agent Context Assembly

When the operator runs `moe work <project> <request> <document>`, the CLI assembles the agent's context from the guidance layer plus the document model:

```
Agent context = soul.md
              + department handbook (e.g., departments/architect.md)
              + project overrides (if any)
              + request overrides (if any)
              + upstream documents (current content, tagged ok/draft)
              + current document content (what it's working on)
              + progress.json (multi-session state)
```

The assembly is a string-concatenation of markdown files injected via `--append-system-prompt`. Multi-turn conversation history comes from Claude Code's session store (keyed by `--session-id`), not from replaying the JSONL thread — the thread is for human auditability.

This is the full context engineering pipeline. Dense, relevant, layered. No stale Slack threads, no giant monolithic instruction file, no raw conversation dumps from upstream phases.

### Agent Configuration

Agent configuration lives in two places:

1. **`departments.conf`** (INI format): Department → model + allowedTools mapping.

   ```ini
   # departments.conf

   [ideator]
   model = claude-opus-4-6
   tools = Read,Grep,Glob,WebSearch

   [architect]
   model = claude-opus-4-6
   tools = Read,Grep,Glob,WebSearch

   [coder]
   model = claude-opus-4-6
   tools = Read,Grep,Glob,WebSearch,Edit,Write,Bash

   [reviewer]
   model = claude-opus-4-6
   tools = Read,Grep,Glob,WebSearch

   [deployer]
   model = claude-opus-4-6
   tools = Read,Grep,Glob,WebSearch,Bash

   [archivist]
   model = claude-haiku-4-5-20251001
   tools = Read
   ```

2. **Department handbooks** (`departments/*.md`): The guidance content — what the agent should know and how it should behave. Injected via `--append-system-prompt`.

Edits to either file show up immediately in the next `moe work` invocation — no rebuild, no restart.

### Session Continuity & Coordination State

Following Anthropic's harness research, each request maintains:

- **`progress.json`** (request root): Tracks which documents exist, their statuses, and multi-session continuity hints. JSON (not markdown) because agents are less likely to clobber structured data. This is the same file as `request.json`'s `documents` map, kept in sync by `moe work`.
- **`features.json`** (inside implementation document directory): Implementation-specific session continuity. Detailed feature list with completion status, test coverage, and quality grades. Lives with the implementation doc because it's only relevant to the coder agent.
- **Claude Code sessions**: The `--session-id` keeps multi-turn conversation state server-side. Resumption is transparent.
- **Git history**: The ultimate source of truth for what changed and when.

Document agents get continuity from their Claude Code session and their own `content.md` — they pick up where they left off by reading their output. The coder agent additionally needs `features.json` because implementation involves complex multi-step work spanning sessions (implementing features, running tests, fixing failures).

The first session in a request uses an **initializer prompt** that sets up the environment, writes the initial progress and feature files, and makes the first commit. Subsequent sessions use the **coder prompt** that reads progress, picks up where the last session left off, and makes incremental progress.

### Model Agnosticism

Swap a model per department by editing `departments.conf`. The `model` field is passed verbatim to `claude --model` (or to whatever CLI the backend exposes). A future backend abstraction could route `ollama:qwen3` to a local Ollama invocation instead of Claude Code, but for v1, Claude Code is the backend.

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

- **Multi-turn sessions via `--session-id`**: Each document maps to a Claude Code session keyed by `moe/<project>/<request>/<document>`. Context is preserved server-side across turns. The operator posts a new turn via `moe work`, the agent responds, the CLI appends the response to the thread log and commits any document changes.
- **Context injection via `--append-system-prompt`**: `moe work` assembles guidance (soul.md + department handbook + project/request overrides) and upstream documents (tagged with ok/draft status), then injects them as the system prompt.
- **Streaming JSON output**: Responses stream back as newline-delimited JSON, which `moe` parses with `json.NewDecoder` over stdout and displays to the operator in real time.

**Permission model — principle of least privilege per department:**

Each department gets exactly the permissions it needs, enforced natively by Claude Code's `--allowedTools` flag. Document agents get a tight, safe sandbox. Only the implementation department gets the scary permissions.

| Department | Read bureaucracy | Web search | Edit own content.md | Read target project | Write target project | Shell exec |
|-----------|---------------|------------|---------------------|--------------------|--------------------|------------|
| Ideas/Spec | ✓ | ✓ | ✓ | | | |
| Architecture | ✓ | ✓ | ✓ | | | |
| Security Review | ✓ | ✓ | ✓ | ✓ (code audit) | | |
| Test Plan | ✓ | ✓ | ✓ | ✓ | | |
| Implementation | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Deployment | ✓ | ✓ | ✓ | | | scoped to deploy scripts |

The vast majority of the Ministry's work happens in the top rows — agents that can read everything but only write to one markdown file. The worst a misbehaving document agent can do is write a bad paragraph on a request branch. The dangerous permissions are concentrated in one department, which can have stricter review requirements (the coder's output always lands under a human gate at `moe review`).

The coding department is **optional**. MoE works as a document-only planning tool — pitch, spec, architecture, test plan — with manual implementation. Coding automation is an upgrade you add when ready.

For additional isolation of the coder department (Docker, Daytona, SSH hosts), wrap the `claude -p` invocation with `docker run` or `ssh`. Not needed for v1 — stdlib `os/exec` handles all of these uniformly.

---

## CLI

### Commands

```
moe                                         # print usage + a hint: "try 'moe dash'"
moe dash                                    # dashboard — the home screen
moe next                                    # triage mode — do the top attention item, then the next
moe init                                    # scaffold a new MoE workspace
moe add-project <repo-url>                  # register a target repo as submodule
moe new <project> "description"             # create a request, pick document set
moe status <project> <request>              # per-request view: document graph, staleness, last turns
moe work <project> <request> <document>     # the main command — work on a document
moe show <project> <request> <document>     # render current content.md + tail of thread.jsonl
moe ok <project> <request> <document>       # mark a document as settled (soft gate)
moe unok <project> <request> <document>     # reverse moe ok
moe review <project> <request>              # diff the request branch against main
moe approve <project> <request> [--resume]  # merge request branch, generate derived/persistent docs
moe scrap <project> <request> "reason"      # close without merging, record rationale
moe flag <project> <request> ["note"]       # mark as needing attention on the dashboard
moe unflag <project> <request>              # clear the flag
moe history [project]                       # past requests (git log + cost aggregates)
moe list-projects                           # registered submodules
moe reindex                                 # rebuild index.json files from request.json glob
```

~18 commands. The ones you live in are `moe dash`, `moe next`, and `moe work`, with `moe ok` sprinkled between turns.

### `moe work` — The Core Loop

```bash
moe work telomere add-batch-support spec
```

This does:

1. **Check out the request branch** (`moe/telomere/add-batch-support`) or create it from main.
2. **Init the submodule** (`git submodule update --init projects/telomere`).
3. **Print a one-screen context header** — current `content.md` snippet, upstream doc list, last turn timestamp, staleness reason if any.
4. **Open `$EDITOR` on a scratch turn file** (`.moe/scratch/<session>.md`) seeded with a template:
   ```
   # Your turn. Save and close to send. Empty file cancels.
   #
   # Document: spec (ideator)
   # Upstream: pitch (ok)
   # Last agent turn (2026-04-12 09:42):
   #   > I've drafted the batch endpoint spec. A few open questions…
   ```
   Empty file = abort cleanly, no invocation, no commit. Shortcut: `moe work … -m "quick note"` skips the editor for one-liners.
5. **Assemble the prompt** from the guidance layer:
   - `soul.md` (global)
   - `departments/<agent>.md` (role — `spec` maps to `ideator` via `document-graph.conf`)
   - `requests/telomere/overrides/<agent>.md` (project-level, if exists)
   - `requests/telomere/runs/add-batch-support/overrides/<agent>.md` (request-level, if exists)
   - Upstream documents (e.g., if working on architecture, include the spec — tagged ok or draft)
   - Current document content (if resuming)
   - Request context (`request.json`)
   - The operator's scratch turn (as the user message)
6. **Invoke Claude Code**:
   ```bash
   claude -p \
     --session-id "moe/telomere/add-batch-support/spec" \
     --append-system-prompt "$(cat assembled-guidance.md)" \
     --allowedTools "Read,Grep,Glob,WebSearch,Edit(requests/telomere/runs/add-batch-support/documents/spec/content.md)" \
     --output-format stream-json \
     --verbose \
     --model claude-opus-4-6 \
     < prompt.md
   ```
7. **Stream output to the operator** via `json.NewDecoder` on the subprocess's stdout. Ctrl-C aborts cleanly — the session stays intact on Anthropic's side; nothing is committed locally.
8. **Commit the result** to the request branch with structured trailers:
   ```
   ideator: draft spec for batch operations

   MoE-Request: add-batch-support
   MoE-Document: spec
   MoE-Agent: ideator
   MoE-Session: moe/telomere/add-batch-support/spec
   MoE-Cost: $0.12
   ```
9. **Update document state** in `request.json` (status: pending → draft → in_review).
10. **Append the turn to `thread.jsonl`** for audit.
11. **Fire a desktop notification** if the run exceeded a threshold (default 30s) — `osascript` on macOS, `notify-send` on Linux, via `os/exec`.

The operator reads the output, decides what to do:

- Run `moe work telomere add-batch-support spec` again to continue the conversation (same `--session-id`, picks up where it left off).
- Run `moe work telomere add-batch-support architecture` to start the next document (spec's current `content.md` becomes upstream context).
- `moe show …` to re-read the current document and recent thread without invoking an agent.
- Edit `content.md` directly and commit (human steering is first-class).
- `moe review telomere add-batch-support` to see the full diff.

### Multi-Turn Continuity

`--session-id "moe/<project>/<request>/<document>"` gives multi-turn continuity for free. Each `moe work` invocation on the same document resumes the conversation. The operator posts a message, the agent responds, the document evolves. This is the "conversation is the document" model — using Claude Code's native session management.

### Per-Department Permissions

The `--allowedTools` flag scopes what each department can do:

| Department | `--allowedTools` |
|------------|------------------|
| Ideator | `Read,Grep,Glob,WebSearch,Edit(…/spec/content.md)` |
| Architect | `Read,Grep,Glob,WebSearch,Edit(…/architecture/content.md)` |
| Reviewer | `Read,Grep,Glob,WebSearch,Edit(…/test-plan/content.md)` |
| Coder | `Read,Grep,Glob,WebSearch,Edit,Write,Bash` (scoped to `projects/$target`) |
| Deployer | `Read,Grep,Glob,WebSearch,Bash` (scoped to deploy scripts) |

Document agents can only write to their own `content.md`. The coder gets the scary permissions. Enforcement is by Claude Code, not a custom sandbox.

### Git Model

```
main (approved state)
  └── moe/telomere/add-batch-support (request branch)
       ├── commit: "ideator: draft spec"
       ├── commit: "ideator: revise spec after feedback"
       ├── commit: "architect: draft architecture"
       ├── commit: "coder: implement batch endpoint"  ← includes submodule pointer
       └── commit: "reviewer: draft test plan"
```

- One branch per request. All documents on the same branch.
- `moe approve` merges to main and runs derived-artifact generation. `moe scrap` records rationale and deletes the branch.
- `moe review` is `git diff main...moe/<project>/<request>`.
- Rewind is `git reset --soft` on the request branch.
- Fork is `git branch moe/<project>/<request>-v2 moe/<project>/<request>`.
- No custom checkpoint format. Git is the checkpoint.

**Concurrent work on the same project:**

Two requests against the same project contend for two resources: the bureaucracy repo's working tree (only one request branch checked out at a time) and the submodule checkout under `projects/<project>/` (only one target-repo branch checked out at a time). Two layers handle this, from light to heavy:

1. **Default: flock.** `moe work` takes a filesystem lock at `.moe/locks/<project>.lock` for the duration of the Claude Code invocation. A second `moe work` on the same project blocks briefly, or exits with "telomere is busy on request `req-a`; wait, or use `--worktree`." Handles the common case where the operator drives one session at a time and just wants protection against foot-guns.
2. **Opt-in: git worktrees.** `moe work --worktree <project> <request> <doc>` materializes a parallel workspace via:
   ```
   git worktree add .moe/worktrees/<project>-<request> moe/<project>/<request>
   git -C .moe/worktrees/<project>-<request> submodule update --init projects/<project>
   ```
   Each worktree has its own MoE-repo working tree and its own submodule checkout. The Claude Code subprocess runs with `cwd` set to the worktree; the coder agent's `Bash` and `Edit` tools are scoped to that directory. `moe approve` and `moe scrap` run `git worktree remove` as a cleanup step.

Document-only sessions (ideator, architect, reviewer, deployer) only edit one markdown file in the bureaucracy repo and don't touch the submodule. They rarely need `--worktree` — the flock + "only one coder at a time per project" constraint is usually enough. `--worktree` earns its complexity when two coder sessions genuinely need to run in parallel on the same target.

**Submodule handling:**

- `moe work` runs `git submodule update --init projects/$target` before invoking Claude Code.
- Code changes inside `projects/$target/` result in submodule pointer updates.
- `moe approve` does: commit inside submodule → push to target remote → update pointer in bureaucracy repo → merge bureaucracy request branch to main → generate derived/persistent docs → optionally open a PR on the target repo.

**`moe approve` is resumable.** The cascade can invoke the archivist 5-10+ times (once per derived doc, once per affected persistent product doc). Each step writes its output as a separate commit with a `MoE-Approve-Step: <node-id>` trailer. If a step fails (network blip, archivist error, push rejected), `moe approve` exits with a clear error. `moe approve --resume` reads the trailers on the request branch, skips nodes already produced, and continues from the first missing step. The merge to main happens last, after all derived/persistent nodes succeed — so partial-approve state is always "request branch has some extra archivist commits; main is untouched." Safe to re-run, safe to abandon.

### Request State

Flat JSON, committed to the request branch (see the **Request** section for the schema). `moe status` reads it. `moe work` updates it. No databases — just glob `requests/*/runs/*/request.json` and aggregate.

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
$ moe approve telomere add-batch      # merge, derive, push
```

Most sessions are: `moe dash` → pick one → `moe work …` → read → repeat or move on. For a focused triage pass through everything demanding attention, skip the browsing step and use `moe next`.

### The dashboard (`moe dash`)

The home screen. One-shot, non-interactive, sorted by attention.

```
Ministry of Everything                                   2026-04-12  09:47

NEEDS ATTENTION (3)
  telomere    add-batch-operations       architect stale after spec v3
  telomere    fix-timeout-bug            ready to merge
  photo-arc   exif-import                coder: tests failing, 2 turns ago

ACTIVE (4)
  telomere    add-batch-operations       ●●●○○  spec ok, architect in_review
  telomere    fix-timeout-bug            ●●●●○  review
  photo-arc   exif-import                ●●●○○  implementation
  punchcard   oauth-flow                 ●○○○○  pitch, last touched 6d ago

RECENT (last 7 days)
  telomere    rate-limit-fix             approved 3d ago    $4.21
  spam-fight  gmail-filter-v2            scrapped 5d ago    "wrong abstraction"

47 projects registered · 4 active · [moe list-projects] to browse
```

Implementation: `tabwriter` + ANSI color. Reads the workspace and per-project `index.json`, applies the attention filter, sorts. ~150 lines.

### The attention filter

A request lands in **NEEDS ATTENTION** when any of the following are true:

1. **Pending turn** — an agent posed a direct question (detected by the last blip's type + content) and hasn't been answered.
2. **Settled-upstream stale** — a document is stale *and* all of its upstream docs are `ok`. The clean reconciliation case: no ambiguity about what to do next.
3. **Review-ready** — all documents are `in_review` or `ok`, no stale markers, never merged. `moe review` and `moe approve` are the obvious next moves.
4. **Explicit flag** — the operator ran `moe flag <project> <request> "note"`. The note shows in the dashboard. Self-left breadcrumbs.
5. **Failed run** — the last `moe work` crashed, hit a coder test failure, or had a submodule conflict. Exit status and last error are recorded in `request.json`.

A request *not* in NEEDS ATTENTION is still discoverable under ACTIVE — it just isn't demanding anything from the operator right now. Dormant requests (no activity in 30+ days) collapse out of the default view; `moe dash --all` shows everything.

### Triage mode (`moe next`)

The dashboard is passive — it tells you what's there. `moe next` is active — it drives you through the NEEDS ATTENTION list one item at a time, picking the right action for each. Think email's "next message" versus its inbox view.

`moe next` is a **dispatcher**, not a loop. Each attention trigger maps to the right action:

| Trigger | Action |
|---------|--------|
| Pending turn | `moe work <project> <request> <doc>` — opens the editor |
| Settled-upstream stale | `moe work` on the stale doc (upstream is settled, so the work is clear) |
| Review-ready | `moe review`, then prompt: `[a]pprove / [s]crap / [w]ork / skip` |
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
moe next --only review           # only review-ready requests (approve day)
moe next --only stale            # only stale reconciliations
moe next --dry-run               # preview the queue without taking action
```

The skip list is in-memory only — it resets when you quit. Quitting mid-session is safe: nothing is held open, nothing rolls back.

**Why this matters at scale.** Browsing the dashboard to pick what to touch is fine when three things need attention. When twelve do, the browsing itself becomes the friction. `moe next` is the "inbox zero" path — sit down, Enter-Enter-Enter through the obvious ones, skip what needs thought, come out the other side with a smaller list.

### Per-request view (`moe status <project> <request>`)

Graph-aware, not a flat list:

```
telomere / add-batch-operations                     branch: moe/telomere/add-batch-operations
in_progress · created 2026-04-10 · 14 turns · $2.83

DOCUMENTS
  pitch           ok           (v1, 2d ago)
  spec            ok           (v3, 2h ago)
  architecture    stale        spec changed after last architect turn (2h ago)
  implementation  pending      waiting on architecture
  test-plan       stale        spec + implementation unsettled
  deploy-plan     pending

LAST ACTIVITY
  2h ago  ideator    spec: revise batch size cap per architect pushback
  4h ago  architect  architecture: propose storage split for batches ≥ 1000
  6h ago  ideator    spec: draft v2 with partial-semantics endpoint

NEXT  moe work telomere add-batch-operations architecture
```

The "NEXT" hint comes from the graph: earliest stale document whose upstream deps are all settled, or earliest `pending` document whose deps are all `ok`. Advisory only — the operator can ignore it.

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
- **Let it run**. For the coder department, a turn can take 10+ minutes. `moe work` fires a desktop notification on completion if the wall-clock exceeds a threshold (`osascript` on macOS, `notify-send` on Linux, configurable in `departments.conf`). Keeps you from staring at the terminal.

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

`os.Args` switch + `flag.FlagSet` per subcommand. ~30 lines of routing replaces a framework.

```go
func main() {
    if len(os.Args) < 2 {
        usage(); os.Exit(1)
    }
    switch os.Args[1] {
    case "init":         cmdInit(os.Args[2:])
    case "add-project":  cmdAddProject(os.Args[2:])
    case "new":          cmdNew(os.Args[2:])
    case "work":         cmdWork(os.Args[2:])
    case "ok":           cmdOk(os.Args[2:])
    case "unok":         cmdUnok(os.Args[2:])
    case "status":       cmdStatus(os.Args[2:])
    case "review":       cmdReview(os.Args[2:])
    case "approve":      cmdApprove(os.Args[2:])
    case "scrap":        cmdScrap(os.Args[2:])
    case "history":      cmdHistory(os.Args[2:])
    case "list-projects":cmdListProjects(os.Args[2:])
    case "reindex":      cmdReindex(os.Args[2:])
    default:             usage(); os.Exit(1)
    }
}
```

### Configuration Files — Three Audiences, Three Formats

No YAML dependency. The files a human edits regularly are never JSON. The files a machine manages are never a custom format.

| File | Writer | Reader | Format | Parser |
|------|--------|--------|--------|--------|
| `request.json`, `progress.json`, `index.json`, `features.json`, `project.json` | `moe` CLI | `moe status`, agents | JSON | `encoding/json` |
| `departments.conf` | Human (rare) | `moe work` | INI | `bufio.Scanner` + `strings.Cut`, ~20 lines |
| `document-graph.conf` | Human (evolves over time) | `moe work`, `moe status`, `moe approve` | Block | `text/scanner`, ~40 lines |
| `soul.md`, `departments/*.md`, overrides | Human (frequent) | Agents | Markdown | No parsing — concatenated |
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
func gitCheckout(branch string) error {
    return exec.Command("git", "checkout", branch).Run()
}

func gitDiff(base, head string) (string, error) {
    out, err := exec.Command("git", "diff", base+"..."+head).Output()
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

`os.ReadFile` + `strings.Builder`. Concatenate soul.md + department handbook + overrides (most-specific-first) + upstream documents (tagged ok/draft) + current content + request context. ~50 lines.

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
    Status    string // pending | draft | in_review | ok | stale
}

func (g *DocGraph) Downstream(id string) []string { … }     // invert edges
func (g *DocGraph) UpstreamDocs(id string) []string { … }   // DFS up DependsOn
func (g *DocGraph) MarkStale(updatedID string) { … }        // BFS downstream
func (g *DocGraph) TopoSort() ([]string, error) { … }       // Kahn's, detects cycles
func (g *DocGraph) ReadyDerivedNodes(present []string) []string { … } // for moe approve
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
- **Request Review**: Full request branch diff against main organized by document.

Three reusable components compose all views: **document viewer** (rendered markdown), **diff viewer** (branch vs main), and **thread viewer** (JSONL log).

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
2. `moe` checks out the request branch (creates it from main if new), inits the target submodule
3. `moe` assembles the prompt from `soul.md`, department handbook, overrides, upstream documents, current content
4. `moe` invokes `claude -p` with the assembled prompt, `--session-id`, `--allowedTools`, and the department's configured model
5. `moe` streams Claude Code's output to the operator and appends to `thread.jsonl`
6. When Claude Code finishes, `moe` commits the resulting changes (document content.md, and any target-submodule pointer updates) with structured trailers
7. `moe` updates `request.json` document status and refreshes `index.json`
8. Operator inspects and decides the next move: continue, advance to another document, edit manually, review, approve, scrap

### Process Model

**The human is the scheduler.** One request, one branch, one agent at a time under the operator's attention. Multiple terminal sessions work in parallel for different requests — they're independent branches, they don't touch each other.

Concurrent work on the same project is a `moe work --worktree` opt-in. The default single checkout enforces one active session per project via a flock under `.moe/locks/`. Worktrees materialize a parallel MoE working tree + independent submodule checkout under `.moe/worktrees/<project>-<request>/`; `moe approve` and `moe scrap` clean them up. See the **Git Model** section for the concrete mechanism. Docker or SSH wrappers remain a Phase-5 option for coder isolation but aren't the concurrency mechanism.

**Cross-document negotiation is operator-mediated.** When the architect pushes back on the spec ("batch size 10,000 needs different storage"), the operator carries that pushback into the spec conversation. No automated inter-agent messaging. A future evolution could add a `for` loop in `moe work --negotiate` to iterate architect/coder/reviewer until settled or max iterations — it's a `for` loop, not a DAG engine.

### Technology

CLI: Go stdlib. Persistence: git + committed JSON/JSONL + INI + block-format config. Agent backend: Claude Code headless (`claude -p`). Sandbox for coder: `os/exec` shells out to `docker`/`ssh`/`daytona` when needed. Everything else runs in the operator's local shell.

### Git Model

**Two repos, two audiences.** The bureaucracy repo (back office) contains handbooks, request state, run history. Target project repos (front office) are git submodules — clean code, no MoE artifacts. Request branches (`moe/<project>/<request>`) accumulate on the bureaucracy repo; `main` reflects settled state. The `moe` CLI itself lives in its own repo — tool and state are separate. See the **Hierarchy** and **Git Model** subsections for the full picture.

---

## Bootstrap Plan

The Ministry's first project is itself. Build the minimum that can manage one request end-to-end, then dogfood.

### Phase 0: Workspace Scaffolding

- [ ] `moe init` scaffolds the directory structure (`departments/`, `projects/`, `requests/`, `soul.md`, `departments.conf`, `document-graph.conf`)
- [ ] Write initial `soul.md` and two handbooks (`departments/ideator.md`, `departments/architect.md`)
- [ ] Seed `document-graph.conf` with pitch, spec, architecture, implementation, test-plan, deploy-plan
- [ ] Seed `departments.conf` with the minimal set of agents
- [ ] `moe add-project` adds a submodule under `projects/` and scaffolds `requests/<project>/project.json`

### Phase 1: Single-Document End-to-End

- [ ] `moe new <project> "description"` creates a request branch, scaffolds `request.json` with the chosen document set
- [ ] `moe work <project> <request> spec` — prompt assembly, `claude -p` invocation, streaming, commit with trailers, status update
- [ ] `moe status` reads `request.json` glob, renders with `tabwriter`
- [ ] `moe review` shells out to `git diff main...`
- [ ] Run the loop manually against a real request. Iterate on handbooks and the guidance layer.

### Phase 2: Full Request Lifecycle

- [ ] `moe approve` — merge request branch, commit+push target submodule, optionally open PR
- [ ] `moe scrap` — record rationale, delete branch
- [ ] `moe history` — `git log` aggregation with cost totals from commit trailers
- [ ] Additional department handbooks (coder, reviewer, deployer, archivist)
- [ ] Project-level department overrides

**Self-hosting checkpoint**: MoE manages its own development via `moe work`.

### Phase 3: Document Graph Mechanics

- [ ] `internal/graph/` module: adjacency maps, staleness propagation, upstream collection, topological sort
- [ ] `moe status` shows stale documents with reasons
- [ ] `moe work` auto-collects upstream documents (tagged ok/draft) as context
- [ ] Ripple summary in `moe review`

### Phase 4: Derived & Persistent Documents

- [ ] Archivist handbook
- [ ] `moe approve` walks the graph, invokes archivist for each derived node with satisfied deps
- [ ] Persistent product-level documents (changelog, decision log, API docs, etc.) updated incrementally at approve
- [ ] Index file maintenance (`moe reindex` rebuilds from scratch; normal operation keeps them in sync)

### Phase 5: Coder Department

- [ ] `departments/coder.md` with initializer/coder pattern
- [ ] `features.json` session continuity
- [ ] `moe work <project> <request> implementation` with expanded `--allowedTools` scoped to `projects/$target`
- [ ] Submodule commit/push/PR flow at `moe approve`
- [ ] Optional: Docker/SSH wrapper around `claude -p` for isolation

**Self-hosting checkpoint**: MoE can plan *and* execute its own development.

### Phase 6: Ops Hooks (Future)

- [ ] Ops department handbook
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
| Anthropic Managed Agents backend | Rejected for core, narrow-yes for coder | See Review Notes. |
| Managed sandbox providers (Docker, Daytona) | Deferred | `os/exec` wrapper around `claude -p` is sufficient when needed. |
| Clerk as LLM agent | Deferred | Human operator is the Clerk in v1. |
| Cross-project knowledge queries | Deferred | Emerges naturally once persistent documents accumulate. |
| Explicit knowledge graph files | Deferred | Plain text JSON in `knowledge/` when the pattern stabilizes. |
| "Yolo mode" auto-merge | Deferred | Every request passes a human review gate in v1. |
| Interactive TUI | Deferred | One-shot `moe dash` covers the prioritization/resumption problem. Revisit if navigation friction justifies a Bubble Tea or raw-termios TUI — but that would breach the stdlib-only stance, so only if the one-shot dashboard is demonstrably insufficient. |

---

## Review Notes

_Open items from spec review — April 2026._

**[Q] Cascading partial-context degradation.** Disjunctive triggers at `moe approve` mean a bug fix with only an implementation doc can cascade thin derived artifacts through to persistent product docs. Not blocking — graceful degradation is the right default — but worth monitoring. If persistent doc quality drifts, consider adding a minimum-context threshold or human review gate for derived docs generated from a single weak input.

**[Q] Anthropic Managed Agents — evaluated, rejected for core, narrow-yes for coder.** [Anthropic Managed Agents](https://www.anthropic.com/engineering/managed-agents) (launched April 2026) provisions a per-session container as the agent's workspace, with Anthropic running the agent loop on its orchestration layer. Evaluated against MoE and rejected for the document-centric core because it conflicts with four design principles: (1) it's per-token API billing — the opposite of the Claude Code headless cost model; (2) it's Claude-only and first-party-only, violating model-agnosticism; (3) it keeps session state server-side via SSE, violating "repo is the source of truth"; (4) it duplicates sandbox functionality already covered by `os/exec` wrappers. Beyond principles, the vast majority of MoE's workload is document agents that only read the repo and write one markdown file — none of which benefit from a server-hosted container with bash/code execution. **Narrow-yes case:** Phase 5 coder could treat Managed Agents as one sandbox option alongside a Docker/SSH wrapper, selected per invocation. Not a priority; revisit only if hosted-for-clients scenarios emerge where Max/Pro subscriptions don't transfer.

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
