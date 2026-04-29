# Design: generic wiki engine + digital twin

Umbrella refactor. Generalizes today's kb machinery into a shared wiki
engine that backs two wiki instances per project: an open-schema kb
(today's research workflow) and a closed-schema twin (Karpathy-style
LLM wiki, applied to project intent).

Subordinate design sessions will fork from this doc — see "Forkable
design sessions" at the bottom. Source exploration:
`projects/moe/runs/generic-wiki-architecture-2026-04-27/documents/design/content.md`
in bureaucracy.

## The pull

Three threads point at the same refactor:

- The kb is already an LLM-maintained markdown corpus. The Karpathy
  "LLM wiki" framing names what it actually is and what the missing
  primitives are (ingest, query, lint; `index.md`, `log.md`).
- The digital twin proposal (James → James, 2026-04-24) describes a
  fixed-schema living model of a project — same shape as a wiki, with
  a different schema and a `reflect` cadence.
- The Phoenix Architecture lens reframes what a project's durable
  layer should preserve: not implementation, but intent / boundaries /
  evals / provenance.

Factor out the maintenance engine so the kb and the twin are both
instances of it.

## Concepts

A **wiki** in this codebase is a tuple:

```
(engine, schema-config, content-directory, checkpoint)
```

- **Engine** — shared infrastructure. Walks docs, runs prompts,
  ingests new content, advances checkpoints, appends to changelogs.
- **Schema config** — defines what docs exist and how they're walked.
  Two flavors:
  - *Closed schema* — fixed set of named docs with per-doc reflect
    prompts. Defined in MoE source.
  - *Open schema* — set of topic docs is emergent; corpus-level prompts
    decide what topics exist. Schema lives in the content itself
    (Karpathy's `index.md` is part of this).
- **Content directory** — per-instance markdown.
- **Checkpoint** — per-instance "last-ran sha" in bureaucracy.

Each project gets exactly two wiki instances:

- `bureaucracy/projects/{project}/knowledge/` — open-schema kb.
- `bureaucracy/projects/{project}/digital-twin/` — closed-schema twin.

Not arbitrary-N. Multi-kb-per-project was considered and dropped: it
adds an addressing layer (`--kb <name>`, create/remove commands,
default-kb concept) and forces the operator to categorize every piece
of research up front, all to solve a problem we don't have evidence
of. One kb per project; the existing kb commands work against it
implicitly.

## Twin schema

Closed schema, hard-fixed in MoE source. Four files in
`digital-twin/`:

### vision.md
- **Purpose:** what this project is trying to be, and why anyone
  should care. The north star.
- **Contains:** the problem being solved, who it's for, what success
  looks like, deliberate non-goals, the bets the project is making.
- **Reflect prompt shape:** *state vs. doc* — compare what the project
  is actually doing against what the vision claims, flag the gap.
  Asymmetric with the other three docs by design (vision drifts by
  silent erosion, not events).
- **Cadence:** rarely. Most reflect passes leave it untouched.

### architecture.md
- **Purpose:** the structural shape of the project — components,
  boundaries, the load-bearing decisions and their reasoning.
- **Contains:** what the major pieces are, how they fit, why the
  boundaries are where they are, decisions explicitly considered
  and rejected.
- **Reflect prompt shape:** "Did recent work introduce, remove, or
  reshape a component or boundary? Did a previously-recorded decision
  get revisited?"
- **Cadence:** when structural decisions land.

### patterns.md
- **Purpose:** the project's prose-form eval suite — what
  implementations must satisfy and what they must rule out.
- **Contains:** named patterns (positive: shapes that work),
  anti-patterns (negative: shapes that have been tried and rejected),
  durable learnings expressed as constraints on future implementation.
  Each entry: short description, when it applies, pointer to canonical
  example or to the run that established it.
- **Reflect prompt shape:** "Did recent work repeat a shape that
  should be named? Did it deviate from a recorded pattern in a way
  that's a deliberate choice vs. drift? Did anything get tried and
  rejected in a way that should be recorded as a negative eval?"
- **Cadence:** compounds slowly. A pattern shouldn't be promoted until
  it's appeared three or so times; an anti-pattern lands the first
  time something is decisively rejected.

### operations.md
- **Purpose:** how the project runs day-to-day. The runbook.
- **Contains:** workflows, tooling, rituals, cadence of recurring
  work, escalation paths, where things live.
- **Reflect prompt shape:** "Did recent activity change a workflow,
  introduce a new tool/ritual, or surface that something documented
  here is no longer true?"
- **Cadence:** the most active doc.

### Schema is hard-fixed (not profiled)

MoE ships exactly one twin schema. No per-project-type profiles, no
project-level overrides. Opinions are the product: if profiles are
configurable, MoE is a wiki engine; if the twin is one fixed shape,
MoE is *the way you make a project legible*. The engine factoring
keeps the door open — adding a profile later is "ship a second schema
config," not a rewrite. Trigger to revisit: a real project where
forcing it through the four docs makes the twin actively worse.

### Phoenix lens

Mapping Fowler's four primitives onto the schema:

- **Behavioral specification → vision.md.** Vision is the spec, not
  decorative documentation. Drift detection is state-vs-doc because
  vision is the generative input.
- **Context boundaries → architecture.md.** The load-bearing content
  is the boundaries between regeneration units, not interior
  structure.
- **Evaluations → patterns.md.** Patterns + anti-patterns are the
  project's prose-form eval suite — durable contracts on what
  implementations must satisfy and rule out.
- **Provenance → the dated changelog** (`reflect-YYYY-MM-DD.md`).
  Records the causal chain (what triggered the change, what evidence,
  what got updated), not a narrative summary.

## kb mode

User-facing shape stays the same as today: chat-driven research →
summarize → publish. The change is what publish does underneath —
instead of writing a single output, it invokes the engine's ingest
op, which does the schema work.

### Flow

- **research** (chat-driven, unchanged): conversation produces raw
  material — notes, findings, sources.
- **summarize** (chat-driven, unchanged): cleans the raw material into
  a coherent input the engine can ingest.
- **publish → ingest** (engine-backed): the summarized input gets
  handed to the engine. Running in open-schema mode, the engine
  decides what topic doc(s) the content belongs to (existing or new),
  updates them, maintains internal links, refreshes `index.md`, and
  appends a changelog entry.

### Open-schema ingest does

- Place the new content in the right topic doc(s), creating new ones
  when nothing fits.
- Apply schema-evolution primitives when warranted: split a doc
  that's gotten too broad, merge near-duplicates, rename when topic
  framing has shifted, retire docs that are no longer referenced.
- Keep cross-links between topic docs current as content moves.
- Maintain `index.md` (the catalog) and append to the dated changelog.

## Updating the twin

Two write paths. Both produce the same artifact (updated docs +
changelog entry).

### Observed updates — reflect

The twin learns about something that already happened: code shipped,
pattern crystallized, operations changed. Reflect walks each doc with
its prompt against events since the checkpoint and proposes updates.
Automated and periodic, but invokable on demand.

### Decided updates — direct edit

The operator decides something about the durable layer that isn't yet
manifest in reality: vision pivot, architectural intent, new
constraint. Reflect can't do this well — there's no event to walk
against; the decision *is* the change. Mechanism: edit the twin file.
The decision is authored, not derived.

### E2E example: strategic shift mid-run

1. Operator opens an idea for the pivot.
2. Design stage works through the new direction.
3. Once decided, the new vision is written into `twin/vision.md` (with
   a changelog entry).
4. Plan stage of the *same run* reads the twin → sees the new vision
   → plans against it.
5. Reflect, on its next pass, doesn't re-derive the vision change — it
   runs the state-vs-doc check and flags drift if the project hasn't
   started behaving in line with the new vision yet.

The twin isn't transactional with respect to a run: a stage can update
it, and downstream stages of the same run see the update.

### Changelog mechanism: detect and prompt for context

For decided updates, reflect's automatic-changelog story doesn't
apply. The mechanism is **detect-and-prompt**:

- The system detects that twin docs changed outside a reflect pass
  (on commit, on next moe command, or via a watcher — implementation
  detail).
- It prompts the operator for context: why did this change, what
  drove it.
- The prompt has access to the workflow docs of the active or recent
  run (design, plan, etc.) so it can pull motivation directly from
  what's already been written, rather than asking the operator to
  retype it.
- The synthesized changelog entry records: what changed (from the
  diff), why (from the prompt + workflow docs), and which run drove
  it (if any).

Why this shape: an explicit command is discipline-dependent —
operators edit markdown files directly out of habit. Pure auto-detect
is lossy because the system doesn't know intent. Detect-and-prompt
gets the safety net plus the context quality, with workflow docs
doing most of the work so the prompt isn't friction.

## Cross-cutting

- **Changelog file** (`reflect-YYYY-MM-DD.md`): audit trail of each
  reflect pass + each detected decided edit. Append-only.
- **Provenance:** every non-trivial claim in a twin doc should be
  traceable back to the run/conversation that introduced it.
  Mechanism TBD — inline footnote, a "since:" line, or recoverable
  from the changelog.
- **Open-schema-only primitives:** split / merge / rename / retire a
  doc. Closed-schema mode forbids these. The engine has to know which
  mode it's in to gate them.

## Forkable design sessions

This doc is the umbrella; the following are open threads that warrant
their own focused design sessions:

- **Engine surface area in detail.** What ops the engine exposes
  (ingest, reflect, walk-with-prompt, query?), their signatures, how
  schema-config files are structured, how closed-schema vs
  open-schema is selected.
- **First implementation step.** Smallest change that proves the
  engine factoring — likely "extract today's kb pipeline behind the
  engine interface, no behavior change yet."
- **Decided-update detect-and-prompt mechanism.** Where the detection
  hooks in (commit hook, next-moe-command, watcher), how the prompt
  is presented, how workflow docs are surfaced as context.
- **Provenance mechanism.** How twin claims trace back to their
  originating run. Inline footnotes vs "since:" frontmatter vs
  changelog-only.
- **Reflect cadence and triggers.** How often reflect runs by default,
  what triggers an on-demand pass, how multiple recent runs get
  integrated.
- **Twin-as-context for stages.** What design/plan/code stages
  actually receive from the twin (full docs? curated slice? per-stage
  picks?) and how that's wired.
