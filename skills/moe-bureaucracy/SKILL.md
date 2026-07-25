---
name: moe-bureaucracy
description: How to write the bureaucracy's own artifacts — twin observations (decisions that should edit the digital twin), portable lore (facts that apply across projects), out-of-scope followups, project knowledge topics, and hook/chore definitions. Use when you notice something worth recording for future runs but it's out of scope for the current canvas edit, or when the work itself is authoring a knowledge topic, a hook script, or a chore.
---

# Writing the bureaucracy's own artifacts

You are inside a Ministry of Everything (MoE) bureaucracy session. While doing
your stage's work, you may notice things *out of scope* for this turn but
worth recording for future runs. MoE keeps three places for this:

- **The digital twin** (`projects/<project>/digital-twin/`) records what a project *is*
  and *how it works* — vision, architecture, named patterns, operations,
  glossary. Code is the implementation; the twin is the intent. When
  the two disagree, the twin wins until someone updates it. Notes that would
  edit a twin doc go to twin feedback, below.
- **Lore** (`lore/`) records portable operational facts that apply across
  multiple projects, not just this one — things like "this kind of sandbox
  needs that kind of proxy." One fact per file with an `applies-when:`
  heuristic so future agents know whether to open it.
- **Followups** (`followups.md`) records work that's worth doing but out of
  scope for the current canvas. The operator triages at close; survivors
  become idea runs.

Two more sections at the end cover artifacts you write *as* the work rather
than as a note about it: **Knowledge** (the project's durable domain
reference) and **Hooks and chores** (the project's own automation). Those live
under `projects/<project>/` and ride your stage's per-turn commit.

The paths below are pre-substituted for this run. Read top-down and append to
the first matching channel.

One thing you **never** write: **intents**. An intent
(`projects/<project>/runs/<slug>/documents/intent/content.md`, the
single-stage `intent` workflow) is the operator's standing direction for
a project — where it's going, parked while it's relevant. Agents read
intents (the `moe-context` skill covers that); only the operator authors
or closes them. If you think a theme or direction is missing, say so in
your canvas or a report and let the operator decide whether to park it —
don't file it as a followup expecting it to become an intent (followups
become *ideas*, never intents), and never run `moe intent new`/`edit`
yourself.

---

## Twin observations

If you notice something about the project that belongs in the digital
twin — would acting on this note edit `projects/<project>/digital-twin/`
(architecture.md, vision.md, patterns.md, operations.md, glossary.md)? —
append a note to:

  {{.TwinFeedback}}

Free-form prose; separate notes with `---`. Name the twin doc and
any file:line refs so the next `moe twin reflect` knows where to
look. Example:

  <doc>.md says X is invariant, but <pkg>/<file>.go:<N> does Y.
  Decide which is canon.

  ---

  patterns.md "fail loud" claims handlers panic on bad input, but
  <some-handler>.go silently returns nil. Decide which is canon.

The next `moe twin reflect` picks these up as kickoff context — the
note arrives where the work actually happens.

---

## Portable lore

If you notice a portable fact that belongs in `lore/` — something
discovered here that would help future runs on *any* project, not
just this one — append an entry to:

  {{.LoreFeedback}}

Bar for inclusion: portable (true in 2+ projects), non-derivable
from a project's own files, operational (changes what gets written
or run), and stable (still true in 12 months). Project-specific
facts go in the twin bucket above instead; operator preferences go
in user memory.

Format: - [ ] `slug` — Title. The same three load-bearing tokens as
followups below, parsed by the same grammar: the `- [ ]` checkbox,
the backtick-quoted `slug` (lowercase, hyphenated), and the em-dash
`—` before a terse title. Any other shape is **rejected at close**,
not silently dropped. Lore entries never carry the optional workflow
tag described for followups; a tag here is rejected. Follow with an
indented body (two-space indent) whose first paragraph is the
`applies-when:` heuristic and whose remaining paragraphs are the lore
entry prose:

  - [ ] `compose-tailscale-binds` — Reaching compose ports from the laptop

    applies-when: project uses docker-compose on a fly-box reached
    via tailscale, with no fly.toml services

    Under userspace tailscale on fly with no `fly.toml` services,
    compose `0.0.0.0` binds aren't exposed to the tailnet. The
    canonical pattern is `127.0.0.1:HOST:CONTAINER` in compose +
    `tailscale ssh -L HOST:localhost:HOST dev@<box>` from the
    laptop. True for every fly-box + compose + tailscale project.

The operator reviews these at close; surviving unchecked entries
become `lore/<slug>.md` files and the next stage prompt's catalog
picks them up automatically.

To merge several existing entries into one, or amend an entry in
place, add an indented `supersedes:` paragraph after `applies-when:`.
Its value is a comma-separated list of existing lore slugs and may
wrap across lines:

  - [ ] `stage-sandbox-caches-readonly` — Stage-sandbox tool caches are read-only

    applies-when: a package or build tool fails with a read-only
    filesystem error on its cache directory in an MoE stage sandbox

    supersedes: go-build-cache-readonly-sandbox,
    go-module-cache-readonly-sandbox, pnpm-store-readonly-sandbox,
    uv-cache-readonly-sandbox

    Put the merged fact here.

The harvester writes the replacement first, then deletes the named
entries. A missing superseded file is treated as already done so a
partial attempt can be retried. When the new slug also appears in
`supersedes:`, the entry is amended in place instead of becoming a
`-2` sibling. An in-place amendment preserves the entry's original
`discovered-in` and appends the amending run to an `updated-in`
frontmatter list, so provenance survives the patch. If this paragraph
does not mention `updated-in`, the opening binary rewrites
`discovered-in` on amendment instead of preserving it. If this
section is absent from a materialized session skill, that session's
opening binary does not support superseding lore; do not submit merge
entries under that binary.

---

## Followups

If you notice something worth doing but out of scope for this cycle —
adjacent cleanup, a deferred investigation, a reference to chase —
append an entry to:

  {{.Followups}}

Format: - [ ] `slug` — Title. Three tokens are load-bearing and
parsed exactly: the `- [ ]` checkbox, the backtick-quoted `slug`
(lowercase, hyphenated), and the em-dash `—` between slug and a
terse title. Optionally follow with an indented body of one or more
paragraphs (two-space indent, blank lines between paragraphs):

  - [ ] `cleanup-foo` — Clean up foo helper

    Why: bar/baz both reach into foo's internals; foo.go:42 is
    the load-bearing assumption. Fix sketch: <one sentence>.

When the work is mechanical, bounded, verifiable, and an agent could
execute it without an operator decision, optionally tag the destination
workflow in parentheses after the closing backtick:

  - [ ] `cleanup-foo` (sdlc) — Clean up foo helper

The tag licenses a future pulse to promote the harvested idea under its
own slug; it does not schedule or start the work. Leave investigations,
policy calls, speculative work, and anything needing human judgment
untagged — untagged is the default and stays operator-triaged. Tags are
validated at close against staged, chainable workflows; unknown or
non-chainable tags are rejected rather than silently ignored.

Content written in any other shape — plain bullets, prose, or a
hyphen where the em-dash belongs — is **rejected at close**, not
silently dropped: the harvest fails loud so you (or the operator)
can fix the shape and re-run, rather than losing the idea.

**The file is the claim.** Never write "filed as followup `x`" in a
canvas without the matching `followups.md` line — a canvas that
reports a filing it didn't make loses the item *and* convinces every
later reader it's tracked. Close warns about claims it can't verify.

Use the body only when context would save a future agent real
work — the *why*, file:line refs, or a one-sentence approach
sketch. Skip the body when the title is self-explanatory. The
operator reviews and prunes these at termination; unchecked
entries become idea runs with the body carried into the seed
canvas.

To file a followup against a *different* project, prefix the slug
with `<project>/`. A bare slug (the default) files against the
current project; a prefixed slug routes the idea to the named
project, which must already be registered:

  - [ ] `claudia/inherit-nginx-identity` (sdlc) — Claudia should inherit the nginx identity injection

The line stays in *this* run's `followups.md` and is harvested at
close like any other; only the destination changes. Provenance
still records the source run, so the destination project sees where
the note came from.

If acting on this entry would edit a digital-twin doc, it belongs
in `feedback/twin.md` above instead. If it's a portable fact that
would apply to other projects, it belongs in `feedback/lore.md`.

---

## Knowledge

`projects/<project>/knowledge/` is the project's durable domain reference: what
you had to learn about the subject matter to do the work, written so a future
run can cite it instead of re-deriving it. Research findings, external surveys,
protocol and API notes, the shape of a third-party system. Not what the project
*is* (that's the twin), not portable ops facts (that's lore), not what happened
this run (that's your canvas).

Write here on your own initiative, not only when asked.

On-disk shape:

- `index.md` at the top of the dir is the catalog and the authority on
  grouping — its sections *are* the taxonomy. Bullets reference topics through
  the subfolder: `- [DNS basics](topics/dns-basics.md)`.
- `topics/<topic>.md`, one file per topic, flat. Cross-links between topics are
  plain siblings (`[other](other-topic.md)`); a link back up to the catalog is
  `../index.md`.

How to write it:

- **Read `index.md` first.** Skim the existing topics and form a map of what's
  there before deciding where new material belongs.
- **Fold into an existing topic before minting a new one.** A new source on DNS
  caching usually deepens `dns-basics.md` more than it warrants a separate
  `dns-caching.md`. Split only when the existing doc would lose coherence by
  absorbing the material.
- **Maintain `index.md` as you go.** Every topic on disk appears in the index.
  This is enforced: a stage whose commit touched `knowledge/` and left an
  unindexed topic, a broken relative link, or an empty doc **refuses to
  close**, and you fix it in the same turn.
- **Cite inline.** Attribute claims to specific sources — a URL, or a short
  `[source: <name>]` tag. Pick one form and keep it consistent within a doc so
  a reader can follow any claim back.
- **Prefer primary sources**: spec pages, original papers, maintainer docs.
  Reach for secondary sources when they add something primary ones don't.
- **Abstract in your own words.** If you can't, you haven't read enough of the
  source — read it or drop it. Every URL you write must resolve; never invent
  one.
- **Name the gaps.** "No good primary source on X" is useful to the next run.
  Leave an explicit TODO rather than papering over thin research with a
  confident sentence you can't support.
- **Tight over padded.** A 400-word topic that says the thing beats a
  2000-word one that restates it. No encyclopedia voice, no "is a term used to
  describe" ceremony.
- **Don't restructure because you'd have shaped it differently.** Surface a
  proposal to the operator first.

---

## Hooks and chores

`projects/<project>/hooks/` and `projects/<project>/chores/` are the project's
own automation, and they are bureaucracy files rather than project source: your
stage's per-turn commit lands them, no push involved.

### Hooks

Scripts under `projects/<project>/hooks/<event>.d/*`. Three events ship:
`dev-env`, `dev-env-teardown`, `pre-push`. Scripts run lex-sorted, so two-digit
prefixes (`10-`, `20-`, …) leave room for inserts.

- Shebang first line; the file must be executable (`chmod +x`).
- `set -euo pipefail` (or `set -eu` for `sh`). A hook that swallows a failure
  silently is worse than one that loudly stops the chain.
- `dev-env.d/*` emits `KEY=VALUE` on stdout, everything else on stderr.
  `dev-env-teardown.d/*` and `pre-push.d/*` are stream-through.
- Read the `MOE_*` env the harness exports (`MOE_PROJECT`, `MOE_RUN`,
  `MOE_BUREAUCRACY`, `MOE_SANDBOX`, optionally `MOE_WORKSPACE`,
  `MOE_TARGET_BRANCH`) — that's the contract. Don't reach outside it.
- `dev-env.d/*` must not mutate tracked files in `$MOE_SANDBOX`. Setup work
  writes to a path the script owns and emits (e.g. `MOE_DEV_TMPDIR=…`); the
  design stage's sandbox-boundary check would false-positive on a dirty tree.
- Self-contained. If `20-db.sh` reads a `$PORT` that `10-port.sh` set, make the
  ordering obvious in the filename and note the dependency in a one-line
  comment at the read site.

`moe hook fire <project> <event>` mints a transient sandbox and runs one
event's scripts once — use it instead of opening a run to test a 30-line bash
change. It leaves the sandbox on disk and prints the path. It is
fire-and-inspect, not a test harness: no assertions, no fixtures. Don't invent
one. And never `chmod -x` a failing script to make a chain green — fix the
script or the thing it caught.

### Chores

A chore is `projects/<project>/chores/<name>/` holding a `chore.json` of
scheduler scalars and a `prompt.md` seed.

`chore.json` — all keys optional, durations are strings like `"720h"` / `"30d"`:

- `trigger`: path glob, or `*` for any merged project change.
- `workflow`: workflow to open; defaults to `sdlc`.
- `cooldown`: minimum duration between completed chore runs.
- `cadence`: stale-by-time duration.
- `when`: a one-line prose due-condition the pulse survey judges against what
  landed. Exclusive with `trigger` and `cadence` — a chore is due mechanically
  or by judgment, not both. `cooldown` still applies.

Reach for `when` when the chore is due only if a judgment holds ("a landed
change made this artifact lie"); a `"trigger": "*"` plus a cooldown is the
shape that degrades into a weekly timer. Keep the criterion to one line: one
that needs paragraphs is too vague to judge.

`prompt.md` is the seed for the opened workflow's first canvas — a markdown
sibling, read verbatim, not folded into `chore.json`.

Use `moe chore check [--project <project>] [<project>/<name>]` as the dry-run
loop. Do not open a chore run just to test a definition.
