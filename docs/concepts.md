# Concepts

The moving parts behind every workflow: where state lives, what agents may
touch, and how they are steered. For how to drive each workflow, see
[workflows.md](workflows.md); for the command catalog and environment
reference, see [reference.md](reference.md).

## Runs, Stages, And Canvases

A workflow is a small ladder of stages. A run is one pass through that
ladder. A stage has one canvas file at
`projects/<project>/runs/<slug>/documents/<stage>/content.md`. The agent
reads that file, talks with you, edits the file, and MoE commits the turn
with trailers like `MoE-Run`, `MoE-Document`, `MoE-Session`, and
`MoE-Workflow`.

Runs live under `projects/<project>/runs/<slug>/`. Each run has `run.json`
plus one document directory per stage. The canvas is the public artifact for
that stage; the raw transcript is stored beside it as agent-specific JSONL so
`moe <workflow> log` can render the conversation later.

Git is the checkpoint. Rewinding a bad turn is `git reset --soft`; undoing a
landed turn is `git revert`. There is no separate database that knows the
real history better than the journal.

`moe <workflow> cat <project>/<run> <stage>` prints a canvas. For one-stage
workflows, the stage can usually be omitted. `moe <workflow> log` renders the
transcript; `--agent claude|codex` disambiguates if both transcript files
exist.

## Bureaucracy Repo And Target Repos

MoE has two repos in play:

- `moe/` is the Go CLI. It is a thin wrapper around `git` and the selected
  agent backend.
- `bureaucracy/` is your private operating journal: registered projects,
  runs, stage documents, ideas, project twins, lore, hooks, and the markdown
  fragments that steer agents. MoE finds it by walking up from `$PWD` to a
  `bureaucracy.conf` marker, or by reading `$MOE_HOME`.

The bureaucracy is the journal. Target projects are registered as submodules
under it. MoE materializes a project before commands touch its source, so cold
projects pay one submodule checkout and warm projects are cheap.

Code-writing stages do not edit the canonical submodule directly. They use a
per-run sandbox clone under `.moe/clones/<project>/<run>/`, created from the
target project and isolated from other runs.

## Sandboxes And Workspaces

Per-run sandbox clones are disposable and scoped to one run. Named workspaces
are long-lived working trees for cases where setup cost matters:

```sh
moe workspace new <project>/<name>
moe workspace list [<project>]
moe workspace shell <project>/<name>
moe workspace refresh <project>/<name>
moe workspace release <project>/<name>
moe workspace remove <project>/<name>
```

A named workspace can be claimed by one run at a time, but the directory
survives run close. `refresh` rebuilds cached `dev-env.d/*` output in place;
`release` clears a stuck claim.

`moe sdlc shell <project>/<run>` drops you into the run's working tree (its
sandbox clone or named workspace, whichever it was opened with); `moe workspace
shell` does the same for a named workspace directly.

## Feedback Channels

MoE's memory improves through a few explicit channels:

- Followups are out-of-scope work noticed during a run. Agents write them to
  `followups.md`; close-time harvest promotes surviving entries to ideas, and
  `moe <workflow> harvest` re-runs that promotion without closing the run. A
  followup slug carrying a `<project>/` prefix routes the promoted idea to that
  project's backlog instead of the current one.
- The idea backlog holds work that is worth remembering but not ready for a
  full run.
- The digital twin records project intent in `vision`, `architecture`,
  `patterns`, `operations`, and `glossary` documents. When code and
  twin disagree, the twin wins until a deliberate edit updates it.
- Lore stores portable facts that apply across projects. Agents see a compact
  catalog and open entries only when the "applies when" hint matches.

## How Agents Are Steered

MoE assembles an instruction preamble fresh for every turn. The important
inputs are plain markdown:

- [`soul.md`](../soul.md) defines the general operating philosophy and
  quality bar.
- `workflows/<workflow>/<stage>.md` defines the lens for the current stage.
- The stage-location header says where the run is in the ladder and what the
  chain prompt will offer next.
- Project digital-twin documents point the agent at recorded intent.
- Lore and followup pointers tell the agent where to look and where to leave
  traces.
- Project-specific guidance such as `AGENTS.md` or `CLAUDE.md` is named
  explicitly because the agent's working directory may be the bureaucracy
  rather than the target repo.

The rule is simple: if the agent keeps making the same kind of mistake, prefer
editing the markdown it reads over adding Go code.

## Skills

Claude Code and Codex both support skills: named markdown files the backend can
load when their description matches the situation. MoE ships three:

- `moe-bureaucracy` teaches agents how to leave traces for downstream runs:
  followups, twin observations, and lore, without exceeding the current stage's
  scope.
- `moe-context` teaches agents how to read the bureaucracy as context: prior
  runs' canvases, journal trailers for slicing by run/doc/workflow, past
  transcripts, the twin, and lore.
- `moe-howto` teaches agents how to capture and groom the idea backlog from a
  chat session, the verb set chat uses on your behalf.

MoE materializes the relevant skills into the session's backend-specific skill
directory with paths already filled in for the current run.
