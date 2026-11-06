---
name: moe-context
description: How to read the MoE bureaucracy as context — project knowledge, prior runs' canvases, journal trailers for slicing by run/doc/workflow, past stage transcripts, the digital twin, and lore. Use when you want to discover relevant knowledge, check whether a topic has been worked before, find prior decisions, or read past stage canvases.
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
  the open intents are the aim. The always-on stage prompt inlines
  their bodies, so you already have them; the pulse's fragment makes
  steering by them mandatory.
- `projects/<p>/knowledge/index.md` — the project's knowledge catalog;
  `knowledge/topics/` holds the topic bodies.
- `lore/<slug>.md` — cross-project facts; the always-on stage prompt
  catalog lists which apply to which contexts.

The always-on stage prompt names
`projects/{{.Project}}/knowledge/index.md` when it exists. If you have not
read it yet in this agent context, read it now, then open relevant topics.
Reuse that catalog read while it is current and available in context;
refresh it if the index changes or its contents are no longer available.
Do not reread it every conversational turn or load every topic.
For lore, use the injected catalog's applies-when hints to select bodies.

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
only under document and diagnostic artifact directories. Global lore,
all projects' knowledge indexes/topics and digital twins, run canvases,
metadata, intents, followups and feedback, hooks/chores, and the entire
active run stay on disk. Sparse omission preserves indexed blobs; it is
not deletion. Existing full worktrees stay full.

The log command reads omitted transcripts from this session's Git index
without restoring files. For a committed prompt snapshot, use
`git show HEAD:projects/<p>/runs/<run>/documents/<stage>/prompt-<agent>.md`
(or the corresponding diagnostic path). `HEAD:<path>` reads a committed
snapshot; `:<path>` reads the current index. Neither includes unstaged
edits. Stay in the session repository so the history agrees with the
snapshot you are working against.

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

Before committing to a direction, check relevant durable reference and
whether a past run already considered it. Run these searches from
`{{.BureaucracyRoot}}`, not the project source clone. After reading the
knowledge index, search the relevant context with filename-only output:

```sh
context_paths=()
for path in 'projects/{{.Project}}/knowledge' 'projects/{{.Project}}/digital-twin' 'projects/{{.Project}}/runs' lore; do
  if [ -d "$path" ]; then context_paths+=("$path"); fi
done
if [ "${#context_paths[@]}" -gt 0 ]; then
  rg -l -i -e 'robot.?pm' -g '*.md' \
    -g '!**/documents/*/prompt-*.md' -g '!**/diagnostics/*/*/prompt-*.md' \
    "${context_paths[@]}"
fi
```

This Bash example includes followups and both feedback channels. Missing
optional trees are normal; a real read/search error still needs attention.
Exit 1 from `rg` means no matches. Replace the expression with the topic's
keywords or synonyms and open relevant matches. Widen to other projects'
context when warranted. Then search this project's canvases and metadata:

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
  file. Use the exact indexed-artifact fallback below when the selected
  log does not resolve an older hit or elides the relevant tool result.
- Index search is lexical and snapshot-scoped, not an exhaustive account
  of every historical discussion. Say what was searched when reporting
  no evidence.

Either supersede the prior thinking, branch from it, or note in your own
canvas that you reviewed it and why you're taking a different turn.

## Exact indexed artifacts

For an older diagnostic attempt, substitute the exact candidate path and
read it from the session index into a fresh scratch file outside the
checkout. Check Git succeeded before interpreting the file:

```sh
artifact_file=$(mktemp /tmp/moe-context-thread.XXXXXX) || exit 1
if ! git -C '{{.BureaucracyRoot}}' show ':projects/<p>/runs/<run>/diagnostics/<stage>/<attempt>/thread-<agent>.jsonl' > "$artifact_file"; then
  rm -- "$artifact_file"
  exit 1
fi
```

Parse the scratch JSONL using the matched backend's record shape (the
Claude and Codex adapters in `internal/transcript` are references when
MoE source is available). Select relevant event types, turn/call IDs
and matching tool results; print only bounded excerpts of those records.
Set record and output-length limits before inspection. Do not dump the
whole blob into the conversation or equate a raw hit with an attempt.
If targeted inspection cannot resolve it, retain the exact artifact path
and report the uncertainty. Remove the scratch file when finished.

The same `git show ':<exact-path>'` fallback retrieves omitted prompts,
including diagnostic prompts. A missing indexed path is a Git failure,
not evidence of an empty artifact. These reads do not hydrate the checkout.

## When a tool needs the full checkout

Ordinary searches need no hydration. To search indexed transcripts across
all projects, widen the quoted pathspecs above from
`projects/{{.Project}}/runs/...` to `projects/*/runs/...`, keeping `:(glob)`
and the artifact-directory boundaries.

For a tool that requires files on disk, you may temporarily materialize
the session's full bureaucracy tree. This costs disk space and I/O; prefer
indexed search for ordinary greps. It restores this session snapshot,
not every historical revision or other projects' source submodules.
Scope every Git command to `{{.BureaucracyRoot}}`, never the source clone
or canonical main.

First check `git -C '{{.BureaucracyRoot}}' config --bool --get core.sparseCheckout`.
`true` means sparse; `false` or an unset key (exit 1, no output) means
already full, so leave it full without creating sparse configuration.
Treat other errors as failures. Only for a sparse session, save the
patterns to a fresh scratch file outside the checkout before disabling:

```sh
patterns_file=$(mktemp /tmp/moe-context-sparse.XXXXXX) || exit 1
if ! git -C '{{.BureaucracyRoot}}' sparse-checkout list > "$patterns_file" || ! test -s "$patterns_file"; then
  rm -- "$patterns_file"
  exit 1
fi
git -C '{{.BureaucracyRoot}}' sparse-checkout disable || exit 1
```

Keep the scratch filename available across tool calls. After the read-only
scan, restore the saved patterns and remove the scratch file only after
success (if using a new shell, set `patterns_file` to that saved filename):

```sh
git -C '{{.BureaucracyRoot}}' sparse-checkout set --no-cone --no-sparse-index --stdin < "$patterns_file" || exit 1
rm -- "$patterns_file"
```

Check each command's success. Preserve existing edits throughout: never
use reset, clean or force in this workflow. Reapplying sparsity may retain
modified paths on disk; report that outcome instead of discarding work.
Returning to sparsity never takes priority over preserving edits.
