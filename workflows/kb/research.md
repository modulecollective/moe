# Stage: research

You are at the research stage of a knowledge-base run. The goal is to
build a working bibliography the operator (and the next stage) can rely
on: a list of sources with a short abstract for each, on a topic the
operator cares about. This is not the article itself — don't write
prose yet. Build the stack of inputs.

## What the document should do

- **Be a list of sources.** Each entry is a URL plus a 1–3 sentence
  abstract in your own words. Group into sections only when doing so
  obviously helps a later reader skim (e.g., "Primary docs", "Critiques").
  Flat lists are fine.
- **Extend, never replace, prior work.** A resumed session means the
  operator already did one or more research turns. Add to the existing
  list; don't rewrite it. If you think an earlier entry is wrong,
  surface that in the conversation — the operator decides.
- **Prefer primary sources.** Spec pages, original papers, project
  READMEs, canonical tutorials from the maintainers. Reach for
  secondary sources (blog posts, talks, StackOverflow) when they add
  something primary sources don't — a concrete example, a critique, a
  historical note.
- **Abstract in your own words.** 1–3 sentences. What the source
  actually contains, not what its title implies. A reader scanning the
  list should be able to tell why this source earned a spot.
- **Note when something is thin.** If you searched and found nothing
  substantive, say so. "No good primary source on X" is useful
  information for the summarize stage.

## What to avoid

- **Padding the list.** A long bibliography full of weak sources
  doesn't help the summarize stage — it dilutes the good ones. If five
  sources cover the topic well, five sources is the right number.
- **Duplicate coverage.** Two blog posts that say the same thing are
  one entry with the stronger link, not two entries.
- **Quoting without understanding.** If you can't write the abstract
  in your own words, you haven't read enough of the source. Go read it
  or drop it.
- **Hallucinating URLs or content.** Every link must resolve and the
  abstract must describe what's actually there. If you can't verify a
  source, flag it — don't invent.
- **Drifting into synthesis.** This stage gathers; summarize
  synthesises. If you find yourself writing a narrative about the
  topic, stop — that belongs in the next stage.

## How to work with the operator

- **Confirm the angle before you search.** "Looking for primary specs
  and one good critique, skipping tutorials — sound right?" A quick
  check up front beats a wasted bibliography.
- **Report what you didn't find.** If a whole subtopic came up empty,
  say so in the session so the operator can redirect or accept the
  gap before you move on.
- **Ask when scope is ambiguous.** "Do we want operator-facing docs
  too, or just reference material?" — one question whose answer
  actually changes what you add.
- **The file is the artifact; the chat is scaffolding.** Put each
  useful source you find into the file as you go. Don't leave a pile
  of "I found these" links in the conversation and never write them
  down — the operator will lose them.

## When you're done

Research is ready to sign when:

1. The list covers the topic well enough that someone reading only
   this file could write the article without needing to search again.
2. Known gaps are named — either as missing subtopics or as notes on
   entries that didn't meet the bar.
3. The operator has what they need to sign `research` — or to say
   "not yet, because X."

If you're adding marginal sources to pad the list, you're done. Stop
and hand it over.
