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

New session worktrees omit other runs' raw transcripts and saved prompts
from disk. Canvases, metadata and project context remain present, as does
all of the active run. The log command reads omitted transcripts from
this session's Git index without restoring files. For a committed prompt
snapshot, use `git show HEAD:projects/<p>/runs/<run>/documents/<stage>/prompt-<agent>.md`
(or the corresponding diagnostic path). Stay in the session repository
so the history agrees with the snapshot you are working against.

To discover relevant omitted transcripts, use the indexed search in the
worked sequence below. Rendered log output elides long tool results, so
grepping that rendering cannot establish that a term is absent from the
raw transcript.

## Read, don't write

Anything under `projects/<p>/runs/<other-slug>/` is somebody else's
run — read for context, never edit. `lore/<slug>.md` is decided state
the operator updates through lore promotion, not through inline edits.
If you find something in another run that ought to change, leave a
trace via the `moe-bureaucracy` skill instead of editing the source.

`projects/<p>/digital-twin/` is the exception, and only for an sdlc
stage: it is a write target for a run whose own evidence moved the
project's recorded intent. The `moe-twin` skill has the contract.

Your own canvas lives at the path the always-on stage prompt named. The
stage prompt also owns any additional write targets for this turn; this
read-oriented skill grants none.

## Worked shape

Before committing to a direction, check whether a past run already
considered it. Run these searches from `{{.BureaucracyRoot}}`, not the
project source clone. Start with this project's checked-out canvases and
metadata; widen to other projects when the question warrants it:

```sh
rg -l -i -e 'robot.?pm' \
  -g 'content.md' -g 'run.json' \
  'projects/{{.Project}}/runs'
```

Replace the example expression with the topic's keywords or synonyms.
Inspect matching canvases and `run.json` first. For historical questions,
use the journal/trailer recipes above too; for example,
`git log --all --grep='MoE-Document: design' --grep='<keyword>' --all-match`
finds design commits whose messages mention the keyword. Journal and
canvas matches are leads, not complete transcript discovery.

When those leads are insufficient, search indexed transcript blobs:

```sh
git grep --cached -l -i -E -e 'robot.?pm' -- \
  ':(glob)projects/{{.Project}}/runs/*/documents/*/thread*.jsonl' \
  ':(glob)projects/{{.Project}}/runs/*/diagnostics/*/*/thread*.jsonl'
```

The second path covers failed attempts under
`diagnostics/<stage>/<attempt-id>/`. Quoting leaves expansion to Git;
`:(glob)` keeps each `*` within a path component. `--cached` searches this
session's index, including sparse-omitted blobs, without restoring files.
It does not search every historical revision or unstaged changes. `-l`
prints filenames rather than enormous JSONL records. Exit 1 means no
matches; an error is not evidence of absence.

Group candidate paths by `projects/<project>/runs/<slug>` (pipe the
filename output through `cut -d/ -f1-4 | sort -u`), retaining the original
paths for stage and backend selection. Inspect each candidate's
`run.json` for its workflow, then read the matching session:

```sh
MOE_HOME='{{.BureaucracyRoot}}' moe <workflow> log <project>/<slug> <stage> --agent <claude-or-codex>
```

The angle-bracket fields are placeholders. Pin `MOE_HOME` as shown:
CLI discovery checks it before cwd, and a stage's dev environment may
point it elsewhere. Choose the backend from the matched filename;
`--agent` is required when both backends have a normal stage transcript.
Read the relevant conversation alongside its canvas, if one exists.
Record whether the run actually attempted the idea, merely discussed it,
or only quoted related material. Copied documents, injected context,
tool results and prior search output can all create raw matches; neither
a hit nor the number of matching files establishes an attempt.

Keep these evidence limits in view:

- Saved `prompt-*.md` snapshots are excluded by default because they
  repeat steering and upstream context. Use the targeted `git show`
  recipe above for questions specifically about a saved prompt.
- Rendered `moe ... log` output elides long tool results; grepping it is
  not an exhaustive raw-transcript search.
- A diagnostic hit can name an older attempt. `log` selects the current
  normal transcript or latest applicable failure in journal order; it
  has no attempt selector. It does not render every matching diagnostic
  file. If the selected log does not resolve an older hit, retain the
  exact path and mark the evidence unresolved.
- Index search is lexical and snapshot-scoped, not an exhaustive account
  of every historical discussion. Say what was searched when reporting
  no evidence.

Either supersede the prior thinking, branch from it, or note in your own
canvas that you reviewed it and why you're taking a different turn.
