---
name: moe-bureaucracy
description: How to leave traces for downstream MoE runs — twin observations (decisions that should edit the digital twin), portable lore (facts that apply across projects), and out-of-scope followups. Use when you notice something worth recording for future runs but it's out of scope for the current canvas edit.
---

# Leaving traces for downstream MoE runs

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
not silently dropped. Follow with an indented body (two-space
indent) whose first paragraph is the `applies-when:` heuristic and
whose remaining paragraphs are the lore entry prose:

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

  - [ ] `claudia/inherit-nginx-identity` — Claudia should inherit the nginx identity injection

The line stays in *this* run's `followups.md` and is harvested at
close like any other; only the destination changes. Provenance
still records the source run, so the destination project sees where
the note came from.

If acting on this entry would edit a digital-twin doc, it belongs
in `feedback/twin.md` above instead. If it's a portable fact that
would apply to other projects, it belongs in `feedback/lore.md`.
