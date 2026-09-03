---
name: moe-bureaucracy
description: How to write the bureaucracy's own artifacts — twin observations (decisions that should edit the digital twin), portable lore (facts that apply across projects), out-of-scope followups, project knowledge topics, and hook/chore definitions. Use when you notice something worth recording for future runs but it's out of scope for the current canvas edit, or when the work itself is authoring a knowledge topic, a hook script, or a chore.
---

# Writing the bureaucracy's own artifacts

You are inside a Ministry of Everything (MoE) bureaucracy session. While doing
your stage's work, you may notice things *out of scope* for this turn but
worth recording for future runs. MoE keeps three places for this:

- **Twin feedback** (`feedback/twin.md`) records edits the digital twin
  (`projects/<project>/digital-twin/` — `vision.md`, `architecture.md`,
  `patterns.md`, `operations.md`, `glossary.md`) needs but this run shouldn't
  make. The twin records what a project *is* and *how it works*. Your stage may
  edit those files directly; this channel is for what you'd edit if it were
  yours.
- **Lore** (`lore/`) records portable operational facts that apply across
  multiple projects, not just this one — things like "this kind of sandbox
  needs that kind of proxy." One fact per file with an `applies-when:`
  heuristic so future agents know whether to open it.
- **Followups** (`followups.md`) records work that's worth doing but out of
  scope for the current canvas. The operator triages at close; survivors
  become idea runs.

All three fan out when the run reaches a terminal status — close, merge, or
push. A conversational session (`moe idea edit --chat`, `moe intent edit
--chat`, `moe chat`) is the exception: its run may never reach one, so they
harvest at *session end* instead, with no editor review — what you write fans
out the moment the operator exits. Write accordingly.

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

The digital twin is an ordinary write target for an sdlc stage. If your
run has the evidence and the edit is yours to make, **edit
`projects/<project>/digital-twin/` in place** — your turn's commit picks
it up, and the `moe-twin` skill has the writing contract.

This channel is for the other case: a twin edit you can see but this run
shouldn't make. Thin evidence, a call that's the operator's, or simply
out of scope for the change you're landing. Append an entry to:

  {{.TwinFeedback}}

Format: the same `- [ ] `slug` — Title` grammar as followups below,
parsed by the same parser, with the same three load-bearing tokens: the
`- [ ]` checkbox, the backtick-quoted `slug`, and the em-dash `—`
before a terse title. Follow it with an indented body (two-space
indent). Name the twin doc and any file:line refs — that's what makes
the note actionable later:

  - [ ] `architecture-clone-gc-stale` — architecture.md still says reflect owns clone gc

    operations.md §"Sandbox clones" moved gc to `moe clone gc` in
    2026-07; architecture's component list never caught up. Decide
    which is canon and cut the loser.

Unchecked entries are harvested at termination into idea runs, one per
entry, each opening with a line naming this run and the twin dir acting
on it edits. Tag `(sdlc)` under the same rule as a followup — mechanical,
bounded, verifiable — and leave it untagged when a human should decide.
Being out of scope for the change you're landing is a reason to file
here, not a reason to leave the tag off: a note that is out of scope but
still mechanical, bounded, and verifiable carries `(sdlc)` so the next
pulse takes it. Only thin evidence or a call that's the operator's
leaves it untagged. No other tag is valid: twin-ness rides in the idea's
body, not the tag.

A twin slug is a bare slug. The cross-project `<project>/` prefix
followups allow is rejected here — a twin note is about *this* project's
twin.

Content written in any other shape is **rejected at termination**, not
silently dropped.

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
terse title. Follow it with an indented body of a sentence or two
(two-space indent, blank lines between paragraphs):

  - [ ] `cleanup-foo` — Clean up foo helper

    Why: bar/baz both reach into foo's internals; foo.go:42 is
    the load-bearing assumption. Fix sketch: <one sentence>.

**The body is the seed, not decoration.** An unchecked entry is
harvested at close into an idea canvas *verbatim* — `# Title` plus
exactly the body you wrote, and nothing else. So a bodyless entry
becomes a one-line idea, and when it is promoted weeks later the
design stage has to rediscover what you already knew. Write to the
floor: a title that names the change in the artifact's own terms,
plus a sentence or two of context — the symptom and why it matters,
not just a path. Paths go stale between filing and promotion;
symptoms survive a rename.

**Every entry decides whether to tag.** A workflow tag in parentheses
after the closing backtick is the machine's license to start the work:

  - [ ] `cleanup-foo` (sdlc) — Clean up foo helper

Tag `(sdlc)` when the fix is mechanical, bounded, and verifiable — all
three. You have the work context right now; that judgment is worth more
than an operator's later guess from the title alone. Leave it untagged
when it needs a human decision: investigations, policy calls,
speculative work, anything where "what should this even do" is still
open. Untagged is valid, and it is the safe side — no pulse will ever
propose an untagged idea, whoever filed it. What isn't valid is
skipping the decision. An untagged entry should mean "a human should
look at this", not "I didn't think about it".

The tag licenses; it does not schedule. A tagged idea is proposed by a
future pulse under its own slug, and the survey still decides whether
and when it rides. Tags are validated at close against staged,
chainable workflows; unknown or non-chainable tags are rejected rather
than silently ignored. A missed tag is recoverable — the operator can
stamp or clear one later with `moe idea tag` / `moe idea untag` — but
it strands the work until someone notices.

Content written in any other shape — plain bullets, prose, or a
hyphen where the em-dash belongs — is **rejected at close**, not
silently dropped: the harvest fails loud so you (or the operator)
can fix the shape and re-run, rather than losing the idea.

**The file is the claim.** Never write "filed as followup `x`" in a
canvas without the matching `followups.md` line — a canvas that
reports a filing it didn't make loses the item *and* convinces every
later reader it's tracked. Close warns about claims it can't verify.

The body carries the *why*, file:line refs where they earn their
keep, and a one-sentence approach sketch when you have one. An entry
whose title genuinely is the whole spec can stay short — but that is
the exception you justify to yourself, not the default. The operator
reviews and prunes these at termination.

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
