---
name: moe-context
description: How to read the MoE bureaucracy as context — prior runs' canvases, journal trailers for slicing by run/doc/workflow, past stage transcripts, the digital twin, and lore. Use when you want to check whether a topic has been worked before, find prior decisions, or read past stage canvases.
---

# Reading the MoE bureaucracy as context

You are inside a Ministry of Everything (MoE) bureaucracy session. The
bureaucracy is a normal git repo full of prior runs, decided canvases,
twin feedback, lore, and a journal that pins every turn to its run,
doc, and workflow. Before you ask the operator a question whose answer
might already be there, walk that trail.

The sibling skill `moe-bureaucracy` teaches you how to *write* into
this same bureaucracy (twin observations, portable lore, followups).
This one teaches you how to *read* it.

## The two roots in this run

You are running with up to two trees in scope:

- **Bureaucracy session worktree** — `{{.BureaucracyRoot}}` —
  everything below is rooted here. In document-only stages this is
  your cwd; in sandboxed stages you reach it via the absolute paths
  named in this skill.
{{if .HasClone}}- **Project source clone** — `{{.ClonePath}}` — a
  per-run copy of the target project's source. Your edits there are
  isolated from the canonical submodule until the run pushes. Reachable
  via `--add-dir`; your always-on stage prompt names it too.
{{else}}- *No project source clone in this stage.* Document-only stages
  run with the bureaucracy worktree as the only tree.
{{end}}
## File layout under the bureaucracy root

The shape under `{{.BureaucracyRoot}}` is the same for every project:

- `projects/<p>/runs/<slug>/documents/<stage>/content.md` — the canvas
  every prior run committed for that stage. Grep here for prior
  thinking on a topic.
- `projects/<p>/runs/<slug>/feedback/twin.md`,
  `feedback/lore.md` — observations prior runs left for the operator
  to triage.
- `projects/<p>/runs/<slug>/followups.md` — deferred work each run
  noticed but didn't act on.
- `projects/<p>/digital-twin/*.md` — the project's vision,
  architecture, patterns, operations, glossary. Your
  always-on stage prompt already points here; named again so you
  don't re-discover it.
- `projects/<p>/runs/<slug>/documents/intent/content.md` — an **intent**:
  a short, operator-authored statement of where the project is going (a
  theme, a bet, a direction), parked while it's relevant. Intents are
  runs in the single-stage `intent` workflow; `moe intent list <p>`
  names the open ones and `moe intent cat <p>/<slug>` dumps one. They're
  read-only context — the operator authors them, agents never do (see
  the write-side rule in `moe-bureaucracy`). When you're making a
  judgment call about direction (what to propose, what to prioritise),
  the open intents are the aim; read the ones that bear on it. The
  always-on stage prompt lists them in a catalog, and the pulse's
  fragment makes reading them mandatory.
- `projects/<p>/knowledge/topics/` — the project's open-schema wiki.
- `lore/<slug>.md` — cross-project facts; the always-on stage prompt
  catalog lists which apply to which contexts.

## Slicing the journal

Every commit in the bureaucracy carries `MoE-Run`, `MoE-Document`,
and `MoE-Workflow` trailers. That lets you slice the journal by run,
by stage, or by workflow with no extra tooling. Run these from
`{{.BureaucracyRoot}}` (or use `git -C` if you're elsewhere):

- `git log --all --grep='MoE-Run: {{.Run}}'` — every turn of *this*
  run, newest first.
- `git log --all --grep='MoE-Document: design'` — every design commit
  across every project and every run.
- `git log --all --grep='MoE-Workflow: sdlc'` — every commit driven
  by the `sdlc` workflow.
- `git log --all -- projects/{{.Project}}/runs/` — file-scoped walk
  of this project's run history.
- `rg '<topic>' projects/*/runs/*/documents/*/content.md` — prose
  search across every prior canvas.

For portable problems — sandbox quirks, agent ergonomics, recurring
patterns — widen the grep to other projects' runs before drafting new
prose. The fact you need may already have a canvas under a sibling
project.

## Reading past stage transcripts

The raw thread JSONL is awkward; `moe <workflow> log {{.Project}}/<run> <stage>`
renders a past stage's session the way the operator reads it — plain
text, turn boundaries, tool calls collapsed. Reach for that, not
`cat`, when you want to follow how a past stage arrived at its
canvas.

## Read, don't write

Anything under `projects/<p>/runs/<other-slug>/` is somebody else's
run — read for context, never edit. Same for
`projects/<p>/digital-twin/` and `lore/<slug>.md`: those are decided
state the operator updates through `moe twin reflect` and lore
promotion, not through inline edits. If you find something in another
run that ought to change, leave a trace via the `moe-bureaucracy`
skill instead of editing the source.

Your own canvas lives at the path the always-on stage prompt named;
that is the only file under the bureaucracy root you write to.

## Worked shape

Before committing to a direction, check whether a past run already
considered it. Two passes are usually enough:

1. `git log --all --grep='MoE-Document: design' --grep='<keyword>'
   --all-match` — every design commit whose subject or trailer
   mentions the keyword.
2. `rg '<keyword>' projects/*/runs/*/documents/design/content.md` —
   every design canvas with the keyword in prose.

A hit gives you a run slug; from there
`moe sdlc log {{.Project}}/<slug> design` renders the transcript, and
`projects/{{.Project}}/runs/<slug>/documents/design/content.md` is the
decided canvas. Either supersede the prior thinking, branch from it,
or note in your own canvas that you reviewed it and why you're taking
a different turn.
