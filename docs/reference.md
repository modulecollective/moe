# Reference

Look-things-up material: the command catalog, backend setup, shell completion,
hooks and environments, and cleanup. `moe help` and per-command usage are the
source of truth for the exact command surface; this page is a map.

## Command Catalog

### Re-Entry And Supervision

- `moe dash [--all] [--project <id>] [--workflow <name>]` prints the terminal
  dashboard, including a CHORES bucket for due project chores.
- `moe serve [--addr <host[:port]>] [--port <n>] [--insecure]` runs the local
  web UI, bound to `127.0.0.1:4242` by default. **Safe by default:** all views,
  idea capture/edit/close/reopen, and run close/edit/reopen work, but the
  run-spawning actions — opening new runs and plans, advancing a stage, and
  opening a due chore's run — refuse with 403. Pass `--insecure` (or set a
  non-empty `MOE_SERVE_INSECURE`) to enable them; anything that can reach the
  listener can then execute code.
- `moe chore list|check|open|skip` lists due project chores, dry-runs a chore
  definition, opens the run a due chore configures, or clears a due chore until
  it is next triggered.
- `moe where` prints the resolved bureaucracy path.
- `moe version` prints the moe version.
- `moe <workflow> cat <project>/<run> [<stage>]` prints a canvas.
- `moe <workflow> log <project>/<run> [<stage>]` renders a past stage
  transcript in workflow context.

### Project And Run Management

- `moe init [--remote <url>] [dir]` creates a bureaucracy.
- `moe project add <repo-url>` registers a target project.
- `moe project list` lists registered projects.
- `moe project remove <id>` unregisters a project when no named workspaces
  remain.
- `moe sync` explicitly reconciles bureaucracy history, pushed runs, and
  project submodule pointers.
- `moe chain edit` opens an editor over active SDLC runs; reorder lines to
  record a run chain in the bureaucracy journal.
- `moe chain clear [--yes]` drops every currently live run-chain edge.
- `moe <workflow> close [--no-edit] <project>/<run>` closes a run in any
  workflow; for `sdlc` it abandons the run instead of shipping it through
  `sdlc push`.

### Workflows

- `moe sdlc new|design|code|review|test|push|close|harvest|shell|reopen|cat|log`
  drives designed code work.
- `moe chat new|chat|close|harvest|cat|log` drives thinking-partner sessions.
- `moe pdlc new|frame|prd|chunk|close|harvest|cat|log` drives product plans.
- `moe kb new|research|summarize|close|harvest|cat|log|lint` drives project
  knowledge.
- `moe idea new|edit|close|list|move|reopen|cat|log` manages backlog notes.
- `moe twin reflect|vision|architecture|patterns|operations|glossary|finalize|close|harvest|cat|log`
  maintains recorded intent.
- `moe hooks new|code|close|harvest|cat|log` edits project hook scripts through a
  journaled run.
- `moe chores new|code|close|harvest|cat|log` edits project chore definitions
  through a journaled run.

`moe <workflow> harvest [--no-edit] <project>/<run>` re-runs a run's
`followups.md` harvest into ideas without closing it — the way to pick up
follow-ups a re-run regenerated after the run was already closed.

## Codex Setup

If you use the `codex` backend interactively, add this profile to
`~/.codex/config.toml`:

```toml
[permissions.workspace-git.filesystem]
":root" = "read"
":tmpdir" = "write"

[permissions.workspace-git.filesystem.":project_roots"]
"." = "write"
".git" = "write"
```

MoE selects it with `-c default_permissions=workspace-git`. Without the profile,
interactive Codex sessions can fail when Git needs to write
`<clone>/.git/index.lock`.

Separately, MoE pins `GIT_EDITOR=true` and `GIT_SEQUENCE_EDITOR=true` for every
Codex turn (interactive and headless): Codex never has a TTY for an editor, so a
Git operation that would open one — `git rebase --continue` finalizing a rebase,
`git commit` with no `-m` — otherwise hangs on vim and can leave a clone wedged
mid-rebase. Claude is unaffected: its commit flow is already non-interactive.

## Shell Completion

`moe completion <shell>` prints a completion script for `bash`, `zsh`, or
`fish`. Source it from your shell's startup file:

```sh
# bash — in ~/.bashrc
eval "$(moe completion bash)"

# zsh — in ~/.zshrc, after `autoload -U compinit && compinit`
eval "$(moe completion zsh)"

# fish — in ~/.config/fish/config.fish
moe completion fish | source
```

Completion covers verbs and subcommands (`moe sd⇥` → `sdlc`, `moe sdlc ⇥` →
`design code review test …`) and the `<project>/<run>` slug for run-taking verbs
(`moe sdlc code ⇥`), plus idea slugs (including `--from-idea`) and named
workspaces. The script itself never changes as commands are added — all the
logic lives in `moe` and is best-effort, so completion stays silent outside a
bureaucracy rather than erroring.

## Hooks And Environments

Project hooks live under `projects/<project>/hooks/<event>.d/*` in the
bureaucracy:

- `dev-env.d/*` emits `KEY=VALUE` lines that MoE caches and supplies to agent
  sessions and workspace shells.
- `dev-env-teardown.d/*` cleans up when a run or workspace closes.
- `pre-push.d/*` is an invocation-time ship gate; a failing script halts the
  push path and opens a recovery code session.

Use `moe hook fire <project> dev-env|dev-env-teardown|pre-push` to exercise one
event in a transient sandbox without creating a run.

### Per-project dev secrets

Dev and test runs often need secrets (API keys, DB URLs, tokens) that must never
be committed and must not leak across projects. The `dev-env.d` hook is the seam,
no new subsystem required. A script decrypts a per-project file and emits its
`KEY=VALUE` lines; MoE caches them at the tree's gitignored `.moe/dev-env.env`
and sources them into the agent session and `moe workspace shell`. Decryption
runs operator-side at stage open, before the agent subprocess exists, so the
agent receives only the decrypted vars for its own project and never reads the
key. Per-project scoping is structural: only that project's `dev-env.d` runs for
its trees.

Store the ciphertext as a sibling of the hook dir,
`projects/<project>/secrets.env.age`, encrypted with
[age](https://github.com/FiloSottile/age):

```sh
age-keygen -o /<volume>/age/keys.txt                             # one-time: prints age1... pubkey
age -r <pubkey> -o projects/<p>/secrets.env.age secrets.env  # encrypt, then git add the .age
```

```sh
# projects/<p>/hooks/dev-env.d/50-secrets.sh
age -d -i /<volume>/age/keys.txt \
  "$MOE_BUREAUCRACY/projects/$MOE_PROJECT/secrets.env.age"
# stdout: KEY=VALUE lines -> MoE caches and sources them
```

age decrypts with no passphrase, so the same hook survives the headless `!!!`
cascade, which has no operator to answer a prompt. Keep the keyfile outside the
bureaucracy (e.g. on a persistent volume, with the secret line backed up in a
password manager); a leaked bureaucracy clone is then ciphertext only. Rotating a
secret re-decrypts on the next run; a named workspace needs `moe workspace
refresh` to pick up new values. If a framework insists on reading a `.env` off
disk, redirect the same `age -d` output to `"$MOE_SANDBOX/.env"` instead — but
only when the target repo already gitignores that file, since `pre-push` refuses
to ship with any untracked file present.

## Cleanup And Recovery

- `moe session list|abandon|resolve|gc` inspects or cleans leftover stage
  session worktrees and branches.
- `moe clone list|gc` inspects or removes orphan per-run sandbox clones.
- `moe workspace release` clears a stale named-workspace claim.

Stage logic can recover orphaned Claude sessions from the Claude cache or from
mirrored transcript files when the normal close path was interrupted.
