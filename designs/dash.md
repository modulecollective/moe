# Design: `moe dash`

## Problem

The README plans `moe dash` as the daily home screen — "what needs
me today" — with three buckets (NEEDS ATTENTION / ACTIVE / RECENT)
and an attention filter that routes each request into the right
one (README.md:692-729). Nothing is built yet. The short-term
point of the command isn't to light up every filter rule at once —
it's to stop making the operator hunt through `requests/*/runs/*/`
by hand to remember which request was where.

## Scope (what this first cut does)

One file under `internal/cli/dash.go`. Globs
`requests/*/runs/*/request.json`, derives per-request state from
data that already exists in the journal, buckets, sorts, prints.

Buckets:

- **NEEDS ATTENTION** — the request's active stage is **ready to
  sign**: the canonical document for the active stage has a
  `content.md` with non-zero size, *and* no prerequisite was
  re-signed after this document's last work turn (i.e., the
  `upstreamChangeBanner` wouldn't fire). This is the one
  attention-filter rule whose inputs are all present in the
  journal today.
- **ACTIVE** — request status is `in_progress`, not in NEEDS
  ATTENTION, and has touched the journal within the last 30 days.
- **RECENT (last 7 days)** — request status is `approved`
  (`moe sign code` sets it) within the last 7 days.

Flags:

- `--all` — include dormant requests (no activity in 30+ days) in
  ACTIVE. RECENT is unaffected; its own 7-day window is already
  the "anti-dormant" filter there.

Sort within each bucket: most recent activity first (committer
time of the newest commit carrying `MoE-Request: <id>`).

Layout: `text/tabwriter` + the existing `moePrintln`/`moePrintf`
cyan. Header line (ministry name + date/time). One section per
bucket with its count in the heading. Footer: "N projects
registered · M active".

## Out of scope (deferred with specific reasons)

- **Pending-turn detection** — needs `thread.jsonl` or equivalent
  per-turn metadata; neither exists. Revisit when either lands.
- **Settled-upstream stale** — needs a staleness notion beyond
  "prereq moved since last turn" (which is the inverse signal the
  readiness check already uses). The README's rule wants stale
  *with* all upstreams settled; no stale tracking is recorded
  anywhere yet.
- **Explicit flag / unflag** — no `moe flag` command, no
  `Flag` field on `Metadata`. Adding either here doubles scope
  for a feature whose storage isn't designed yet.
- **Failed-run tracking** — no exit-status field on `Metadata`;
  `moe work` doesn't record crash state. Same argument.
- **Dormant collapse with a per-request "last touched" string** —
  the footer already reports counts; the per-row "last touched
  Xd ago" in the README mock is advisory. Keep first cut
  terse.
- **`moe next`** — separate command, separate dispatcher logic,
  separate PR.
- **Cost aggregates in RECENT** — no cost tracking yet.
- **scrapped status** — no `moe scrap` command yet, so the
  status never takes that value.

All of the above are additive: each one is a new column or a new
bucket-predicate in the same file, not a restructuring.

## Implementation notes

- `request.Scan(root)` — new helper in `internal/request/` that
  globs `requests/*/runs/*/request.json` and returns a slice of
  `*Metadata` (or a small `{Metadata, Path}` struct). Centralizes
  the glob so `moe dash`, `moe status`, `moe history` can share
  it. If this is the only caller for a while, fine — it stays a
  clean seam.
- `request.LastActivity(root, requestID) (time.Time, error)` —
  most recent committer time of any commit carrying
  `MoE-Request: <id>`. Symmetric with
  `LatestWorkTurnSHA`; `git log -1 --grep=MoE-Request: <id>
  --format=%ct`. Used both to sort within buckets and to
  bucket into ACTIVE vs. dormant.
- Readiness reuses the existing upstream-change machinery: ask
  `stage.Active`, read the active stage's canonical doc's
  `content.md`, compare its last work-turn time to each
  prereq's latest sign time. No new state.
- Scanning a bureaucracy with no registered projects or no
  requests is success with an empty-but-non-silent output —
  the header and footer still print, the buckets just say
  `(none)`. Silent success on an empty bureaucracy would look
  broken.

## Tests

- Empty bureaucracy → header, three empty buckets, footer.
- One request at design stage with `content.md` present →
  NEEDS ATTENTION.
- Same, but design was re-signed after the last work turn →
  ACTIVE (readiness rule rejects it; the banner would fire).
- Same, but `content.md` empty → ACTIVE (no content to sign off
  on yet).
- One request with status `approved`, signed 2 days ago → RECENT.
- One request with no activity in 40 days and
  `in_progress` → omitted by default, shown under `--all`.
- Sorting: within a bucket, newer activity sorts above older.

## Tradeoffs

- **One readiness rule, not five.** The README lists five
  attention triggers; four need data that isn't recorded. Shipping
  one honestly-computable rule beats faking the other four off
  proxies that would bucket requests wrongly and teach the
  operator to ignore the dashboard.
- **Re-queries `git log` per request.** For the handful-of-active
  requests the README targets, a few `git log -1 --grep` calls per
  `moe dash` is fine (~ms each). If the number of active requests
  climbs into the hundreds, batch via a single `git log --all
  --format=...` pass and fan out client-side. Don't pre-optimize.
- **`tabwriter` alignment vs. ANSI color.** `tabwriter` measures
  raw bytes, so SGR codes break column widths. The existing
  cyan styling is applied per-line in `output.go`; we keep it at
  the section-heading level and emit rows unstyled so columns
  align. Same posture `moe push` takes with subprocess output.

## Open questions

- Should a request with no documents at all still appear in
  ACTIVE? Working assumption: yes — the operator just ran
  `moe request new` and hasn't started a turn. The dashboard's
  job is to surface "what you're in the middle of," and empty
  is a state. Revisit if empty rows clutter the view in
  practice.
