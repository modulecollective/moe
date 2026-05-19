---
name: moe-bureaucracy
description: How to leave traces for downstream MoE runs — twin observations (decisions that should edit the digital twin), portable lore (facts that apply across projects), and out-of-scope followups. Use when you notice something worth recording for future runs but it's out of scope for the current canvas edit.
---

# Leaving traces for downstream MoE runs

You are inside a Ministry of Everything (MoE) bureaucracy session. While doing
your stage's work, you may notice things that are *out of scope* for this turn
but worth recording for future runs. Three channels handle this, each landing
in a different file the bureaucracy already knows how to harvest at run close
or on the next reflect pass. The paths below are pre-substituted for this run;
append to the named file using the format described in its section.

Trigger order: read top-down and stop at the first match. The more specific
case (twin) sits above the fallback (followups) so an agent who has already
mentally drafted a followup gets redirected once they notice the twin
applies — the backward link in the followups section catches the
asymmetric-redirect hole.

---

## Twin observations — `{{.TwinFeedback}}`

If you notice something about the project that belongs in the digital
twin — would acting on this note edit `digital-twin/<project>/`
(architecture.md, vision.md, patterns.md, operations.md, roadmap.md)? —
append a note to:

  {{.TwinFeedback}}

Free-form prose; separate notes with `---`. Name the twin doc and
any file:line refs so the next `moe twin reflect` knows where to
look. Example:

  architecture.md says the universal gate is the only path into
  claim/, but cli/claim.go:84 takes an explicit-path shortcut that
  bypasses it. Either the gate isn't universal anymore, or claim.go
  needs to route through it.

  ---

  patterns.md "fail loud" claims handlers panic on bad input, but
  cli/foo.go:42 silently returns nil now. Decide which is canon.

The next `moe twin reflect` picks these up as kickoff context — the
note arrives where the work actually happens.

---

## Portable lore — `{{.LoreFeedback}}`

If you notice a portable fact that belongs in `lore/` — something
discovered here that would help future runs on *any* project, not
just this one — append an entry to:

  {{.LoreFeedback}}

Bar for inclusion: portable (true in 2+ projects), non-derivable
from a project's own files, operational (changes what gets written
or run), and stable (still true in 12 months). Project-specific
facts go in the twin bucket above instead; operator preferences go
in user memory.

Format: - [ ] `slug` — Title (lowercase hyphenated slug, em-dash,
terse title), followed by an indented body (two-space indent) whose
first paragraph is the `applies-when:` heuristic and whose
remaining paragraphs are the lore entry prose:

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

## Followups — `{{.Followups}}`

If you notice something worth doing but out of scope for this cycle —
adjacent cleanup, a deferred investigation, a reference to chase —
append an entry to:

  {{.Followups}}

Format: - [ ] `slug` — Title (lowercase hyphenated slug, em-dash,
terse title), optionally followed by an indented body of one or
more paragraphs (two-space indent, blank lines between paragraphs):

  - [ ] `cleanup-foo` — Clean up foo helper

    Why: bar/baz both reach into foo's internals; foo.go:42 is
    the load-bearing assumption. Fix sketch: <one sentence>.

Use the body only when context would save a future agent real
work — the *why*, file:line refs, or a one-sentence approach
sketch. Skip the body when the title is self-explanatory. The
operator reviews and prunes these at termination; unchecked
entries become idea runs with the body carried into the seed
canvas.

If acting on this entry would edit a digital-twin doc, it belongs
in `feedback/twin.md` above instead. If it's a portable fact that
would apply to other projects, it belongs in `feedback/lore.md`.
