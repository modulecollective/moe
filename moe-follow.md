# moe follow

Read-only "what should I be looking at?" companion command. Keeps a
design doc in front of the operator while it's in play; gets out of
the way when it isn't.

```
for {
    doc = pickDesignDoc()           // most recent run parked at design,
                                    // or with an open design session
    if doc == "" { idleScreen(); sleep; continue }
    spawnPager(doc) and wait        // user reads, presses q when done
    // on return: re-evaluate. either the same run advanced to code,
    // or another design surfaced, or idle.
}
```

## Why

Operators currently alt-tab to an editor to read the doc claude is
co-authoring on a design stage. A second terminal pane running `moe
follow` keeps the live doc visible without leaving the terminal and
without moe owning the screen — the pager does that.

This is deliberately not a TUI (see README §"At a glance" — no
dashboard that updates on its own). moe resolves *which file*; the
multiplexer handles layout; the pager handles redraws.

## Locked decisions

- **Designs only.** The operator deals with one design at a time, so
  the queue depth is one. Code, push, kb stages don't surface here.
- **Pager-driven, not redraw-driven.** moe spawns a pager and waits
  for it to exit. Live updates flow through the pager's own follow
  mode. Closing the pager is the user gesture that triggers
  re-evaluation.
- **Read-only by construction.** moe only opens the file for read and
  execs a viewer. No `$EDITOR` path; no write permissions.
- **Stdlib-only.** Polling, not fsnotify. No TUI library.

## Open decisions

1. **Which workflows count as "design"?** First cut: `sdlc/design`
   only. `kb/research` is the obvious candidate to add — same
   exploratory shape, same "read along while it drafts" use. Punt
   until requested.
2. **Default pager.** `${MOE_PAGER:-less +F -R}`. `less +F` is the only
   universally-available follow-mode pager; `-R` passes ANSI through
   so a pre-rendered file works. Operators who want rendered markdown
   set `MOE_PAGER='glow -p'` and accept manual reload.
3. **Tie-break when multiple designs are in play.** Reuse `dash`'s
   order: open session beats parked-only; within a tier, most-recent
   activity wins. `--run <id>` to lock.
4. **Idle screen.** One terse line — `(no design in play · 2 active ·
   last: <project>/<run> awaiting merge)` — then sleep `--interval`
   (default 5s) and re-check. Clear-and-print each tick; no
   tabwriter, no art. Ctrl-C exits.
5. **Name.** `moe follow`. Bikesheddable.

## Non-goals

- Multi-pane layout. That's tmux/zellij/iTerm, not moe.
- Rendering markdown ourselves. Operator picks the pager.
- Following code or push docs. Designs only — those are the only
  docs the operator reads passively.
- Notifications, sound, dashboards-that-update. Same reason the rest
  of moe doesn't have them: agents act only when invoked.
- Editing. `moe follow` never opens a writable view. To edit, exit
  and run the appropriate workflow command.

## Pieces to reuse

- `run.Scan` + `run.BuildJournalIndex` (used by `dash`) for the run
  set and last-activity index.
- `session.List` for open-session liveness — same source `dash` uses
  for the `[running]` marker.
- `LookupWorkflow(md.Workflow).Next(root, md)` to resolve the parked
  next stage; filter to `next.Name == "design"` (and whatever else
  decision 1 admits).
- `winningRunningDoc` (dash.go:362) for the open-session-vs-parked
  selection inside a single run. `pickDesignDoc` is the across-runs
  version of the same idea.

## Wart to call out

claude often **rewrites** the design doc rather than appending. `less
+F` handles this — it re-reads from the current offset and shows the
new tail — but the operator may see the file shrink or jump.
Acceptable; the alternative is moe owning the screen, which we
rejected up front.
