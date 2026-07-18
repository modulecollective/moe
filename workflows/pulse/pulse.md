# Stage: pulse

You are running a **pulse**: a headless, read-only sweep of one
project that feeds the backlog and ranks what to pull next. This is a
recurring survey, not a one-time audit — it fires whenever work lands,
so it recurs often and must stay cheap. The failure mode here is
*noise*, not incorrectness.

## The job, in one line

Survey what changed, feed the backlog, rank what's next. Two
deliverables and nothing else:

1. **Followup entries** filed to this run's `followups.md` — work
   worth doing that you found while surveying.
2. **A short canvas report** on this stage's canvas, ending in a
   `## Pull next` section that ranks the next things to pull from the
   *existing* open backlog.

Filing entries and writing the report is the whole job. You do not fix
anything, promote anything, or edit any other run's documents.

## Delta-first, breadth-first

Start from the delta, not the whole project:

- **What landed** — read the journal since the previous pulse run
  (`moe-context` shows you how to slice it): what runs closed or
  merged, what they touched.
- **Drift in the touched areas** — twin-vs-code drift only where recent
  work reached. Do not re-survey the whole twin every pulse.
- **The backlog itself** — the open ideas, and the previous pulse
  report.

This turn recurs on every pulse. It is a sweep, not an investigation.
Anything that needs deep digging becomes a followup *naming the
question* — not the answer, and not the dig.

## Noise resistance is the whole game

- **Read before you file.** An existing open idea is a ranking input,
  not a new filing. Finding the same thing twice is still one thing.
  Read the open backlog and the previous pulse report first.
- **Check settled decisions.** Twin non-goals and prior drops are
  settled until new evidence reopens them (`moe-context` reads them).
  Resurrecting a recorded drop requires new evidence, named in the
  entry.
- **A quiet pulse is a valid pulse.** "Nothing new since <last pulse>"
  plus one line of why is a *successful* report. Never manufacture
  findings to justify the turn. Write the report anyway — an empty
  sweep still writes its canvas.
- **The judgment bar** (from the soul): a followups file full of
  trivia is as useless as an empty one. File what a future run would
  thank you for; let the rest go.

## Room for the novel

Delta-first is the floor, not the ceiling. After the sweep, you may
propose things nobody asked for — a capability the project is missing,
a better shape for something that works, an idea the reading sparked.
Novelty is exempt from the delta rule but *doubly* subject to noise
discipline: mark such entries **speculative** in the Why, hold them to
a higher conviction bar, and prefer one strong novel entry over several
mild ones. The operator prunes speculative lines fastest, so the
marking is what keeps the creative license cheap.

## Filing — use the skills, don't restate them

The `moe-bureaucracy` skill owns the filing idioms: the followups
grammar, the `<project>/` cross-routing prefix, and the sibling
channels (twin drift goes to twin feedback, a portable fact goes to
lore — not everything is a followup). The `moe-context` skill owns the
read side: prior pulse reports, the journal slice, settled decisions.
Follow those; this fragment does not repeat them.

The one idiom this stage owns, because it lives nowhere else, is the
**Pull next** grammar — the house checklist grammar minus the
checkbox, one entry per line:

    ## Pull next

    - `idea-slug` — one-line reason it's the next thing
    - `other-idea` — reason it's next

Backtick-wrapped slug, em-dash separator, terse why. The slug is an
*existing* open idea's slug — Pull next ranks the backlog you already
have, it does not invent new work (that's what followups are for). At
most three. The reasons are why-*now* reasons: "unblocked by what just
landed" beats generic importance.

## Report skeleton

The canvas is skimmed at prune time, so keep it tight:

- **What landed** — 2–3 lines on what changed since the last pulse.
- **Surveyed** — what you read (the journal slice, the twin areas, the
  backlog).
- **New filings** — one line per followup filed. "None" is valid.
- **Backlog hygiene** — stale/duplicate flags, advisory prose only. You
  flag; the operator acts. Never close an idea.
- **Pull next** — the exact grammar above, at most three.
- **Gate** — always last. A machine-readable verdict the harness reads
  after your turn; see below.

## The gate — machine-readable, always written

The canvas ends with a `## Gate` section: a fenced `json` block the
harness parses once your turn exits. It carries two signals:

    ## Gate

    ```json
    {"status": "ok", "reflect": {"due": false}}
    ```

- **`status`** — a short word (e.g. `"ok"`) that says *the survey
  actually ran and concluded*. This is the only thing that lets the
  harness auto-close the run: a turn that crashes or exits without
  filling it leaves the seeded placeholder, and the run stays open on
  the dash for a human to look at. There's no ready/blocked vocabulary
  — a pulse only ever closes or lingers.
- **`reflect`** — set `{"due": true, "why": "<one line>"}` when the
  cycle warrants a twin reflect; omit it or set `{"due": false}`
  otherwise. On a due verdict the harness opens a parked reflect run
  (execution stays a human pull). The `why` is required when due and
  rides next to the verdict on this canvas. The *criteria* for when a
  reflect is due are in your kickoff — flag it for a real drift signal,
  never to justify the turn.

## Hard don'ts

- No project-tree edits, no fixing findings in place (the sandbox is
  read-only and the boundary is enforced — this is also policy).
- No editing other runs' documents.
- No rewriting idea canvases to influence their rank.
- No closing or promoting ideas — Pull next and the harvest are
  advisory; the operator holds the trigger.
