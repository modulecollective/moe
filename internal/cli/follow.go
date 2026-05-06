package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

// `moe follow` keeps the design doc in play in front of the operator.
// Auto-pick surfaces the most recent run with an *open session* on its
// design doc and execs a pager. Parked-at-design runs (work-to-do, not
// work-being-done) are deliberately invisible to auto-pick — `dash` is
// the surface for those. `--run <id>` is the explicit pin escape hatch
// and ignores liveness. When the pager exits, follow waits out a 3s
// countdown (Ctrl-C exits cleanly) so the operator can break the
// relaunch loop, then re-evaluates and either spawns a fresh pager or
// drops to a single-line idle screen polled every --interval. Read-
// only by construction: the canvas is opened for read, no `$EDITOR`
// path.
//
// Pieces reused from `dash`: same Scan, same journal index, same
// session.List for liveness. follow is the across-runs counterpart to
// dash's within-run liveness picker.

// followIdleInterval is the default poll cadence between idle ticks.
// 5s is the design's chosen cadence: long enough that the operator's
// terminal isn't busy, short enough that a freshly-opened design lands
// in the pager within one breath.
const followIdleInterval = 5 * time.Second

// followCountdownSeconds is the dwell after a pager exit before the
// next auto-pick fires — long enough for a Ctrl-C to land cleanly,
// short enough not to feel sluggish. Mirrors queueCountdownSeconds.
const followCountdownSeconds = 3

// defaultPager is the fallback when MOE_PAGER is unset. moe owns the
// follow loop now (watchCanvas + kill-and-respawn), so the pager runs
// in plain mode: `-R` passes ANSI through; `-M` shows the long prompt
// so the file path stays on the status line — no `Waiting for data…`
// banner stealing that real estate. Dropping `+F` also unlocks normal
// scroll keys (`j`/`k`/`g`/`G`/`/`) without operator-quit gymnastics.
const defaultPager = "less -R -M"

// followWatchPoll is the cadence at which watchCanvas stats the canvas
// for change. Cheap (one stat per tick) and well below the debounce
// window so a change can't slip through unseen. var rather than const
// so tests can dial it down to milliseconds; production stays at 250ms.
var followWatchPoll = 250 * time.Millisecond

// followWatchDebounce is the quiet window watchCanvas waits for after
// the last detected change before signalling a respawn. 3s collapses a
// burst of mid-turn rewrites (claude commonly writes → re-reads →
// revises → writes again over a few seconds) into one respawn, while
// staying short enough that the operator perceives the new content
// within one breath of the agent settling. Mirrors followCountdown-
// Seconds as a side benefit — one dwell number to reason about.
var followWatchDebounce = 3 * time.Second

// followKillGrace is how long we wait between SIGTERM and SIGKILL
// when respawning the pager. less catches SIGTERM and restores the
// terminal cleanly within milliseconds; 200ms is comfortably enough
// without making the respawn feel sluggish. The SIGKILL fallback
// covers the rare pager that ignores SIGTERM — terminal state may
// then be left in raw mode, but we don't deadlock on a wedged pager.
var followKillGrace = 200 * time.Millisecond

func init() {
	Register(&Command{
		Name:    "follow",
		Summary: "page the design doc currently in play; idle when none",
		Run:     runFollow,
	})
}

func runFollow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", followIdleInterval, "polling interval when no design is in play")
	runFilter := fs.String("run", "", "lock follow to a specific run id (matches across projects)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe follow [--interval <duration>] [--run <id>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Pages the design doc most worth watching: the most recent run with an")
		moePrintln(stderr, "open session on its design doc. When the pager exits, follow waits 3s")
		moePrintln(stderr, "(Ctrl-C to exit cleanly), then re-evaluates. With no live design, prints")
		moePrintln(stderr, "a single-line idle status and re-checks every --interval. --run <id>")
		moePrintln(stderr, "pins to a specific run regardless of session liveness.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Pager is ${MOE_PAGER:-less -R -M}; moe respawns it when the canvas changes.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	for {
		path, summary, err := pickFollowTarget(root, *runFilter)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if path != "" {
			// Inner loop: respawn on the same path while the watcher
			// keeps tripping on canvas rewrites. Re-evaluation (back
			// to pickFollowTarget) only runs on operator-quit, same
			// rule as today — the operator is reading *this* canvas;
			// a file change doesn't mean a different run is in play.
			if quit := followPath(path, sigCh, stdout, stderr); quit {
				return 0
			}
			continue
		}
		// Idle screen: clear-and-print on the same line each tick so
		// the status doesn't scroll. \r returns the cursor; \033[K
		// clears to end-of-line so a shorter line doesn't leave the
		// previous tail visible. No newline here — it lands when the
		// loop exits, in the sigCh branch below.
		fmt.Fprintf(stdout, "\r\033[K%s", idleLine(summary))
		select {
		case <-sigCh:
			fmt.Fprintln(stdout)
			return 0
		case <-time.After(*interval):
		}
	}
}

// followSummary captures the figures the idle screen reports: total
// active run count and the single most-recently-active run, when any.
type followSummary struct {
	activeCount int
	last        *followLast // nil when nothing's active.
}

type followLast struct {
	project, run, state string
}

// pickFollowTarget resolves the design doc moe should page, plus the
// idle-screen summary for when no doc is in play. Returns ("", summary, nil)
// when no design candidate exists; the caller renders the summary instead.
//
// A design candidate is an in-progress run with an open session on its
// design document — work-being-done. Parked-at-design runs (work-to-do
// but untouched) deliberately don't surface here; that's `dash`'s job.
// Most recent activity wins. runFilter, when non-empty, locks to that
// run id — the operator's pin overrides liveness, and a not-yet-
// existent design canvas falls through to the idle screen so follow
// keeps polling until the file appears.
func pickFollowTarget(root, runFilter string) (string, followSummary, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return "", followSummary{}, err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return "", followSummary{}, err
	}
	// session.List is read-only; a worktree-list error shouldn't
	// suppress the idle-screen summary, so swallow and proceed with no
	// liveness signal — auto-pick will simply find no candidates.
	// We index by run id, keeping only design sessions: the resolver
	// needs the session's WorktreePath (the live canvas lives on the
	// session branch in the worktree, not on main under root) and the
	// per-run "is design live?" check collapses to a map presence test.
	designSessionByRun := make(map[string]*session.Session)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			if s.Doc != "design" {
				continue
			}
			designSessionByRun[s.Run] = s
		}
	}
	summary := buildFollowSummary(root, mds, idx)

	if runFilter != "" {
		for _, md := range mds {
			if md.ID != runFilter {
				continue
			}
			// A pin overrides liveness, but when a design session *is*
			// open the live bytes are in the worktree, not on main —
			// resolve against WorktreePath so the operator sees what
			// the agent has actually written. Parked-at-design pins
			// (no session) fall back to root, which holds the seeded
			// stub or the most recently merged content.
			base := root
			if sess, ok := designSessionByRun[md.ID]; ok {
				base = sess.WorktreePath
			}
			path := filepath.Join(base, run.ContentPath(md.Project, md.ID, "design"))
			if _, err := os.Stat(path); err == nil {
				return path, summary, nil
			}
			return "", summary, nil
		}
		return "", summary, nil
	}

	type cand struct {
		path string
		when time.Time
	}
	var cands []cand
	for _, md := range mds {
		if md.Status != run.StatusInProgress {
			continue
		}
		sess, live := designSessionByRun[md.ID]
		if !live {
			continue
		}
		// Auto-pick liveness gate guarantees a session here, so the
		// canvas resolves against the session's worktree — main holds
		// the pre-session stub until the session closes and rebases.
		cands = append(cands, cand{
			path: filepath.Join(sess.WorktreePath, run.ContentPath(md.Project, md.ID, "design")),
			when: idx.LastActivity[md.ID],
		})
	}
	if len(cands) == 0 {
		return "", summary, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].when.After(cands[j].when)
	})
	return cands[0].path, summary, nil
}

// buildFollowSummary rolls scanned metadata into the figures the idle
// screen needs. "Active" mirrors dash's bucketActiveRuns: in_progress
// or pushed runs from non-idea workflows. The "last" entry is the
// single most-recently-active run by journal activity, with a state
// label suitable for inline display ("awaiting merge" or
// "<workflow>:<stage>"). Idea runs and terminal statuses don't count.
func buildFollowSummary(root string, mds []*run.Metadata, idx *run.JournalIndex) followSummary {
	type row struct {
		md    *run.Metadata
		when  time.Time
		state string
	}
	var rows []row
	for _, md := range mds {
		if md.Workflow == ideaWorkflow {
			continue
		}
		switch md.Status {
		case run.StatusInProgress, run.StatusPushed:
		default:
			continue
		}
		rows = append(rows, row{
			md:    md,
			when:  idx.LastActivity[md.ID],
			state: stateForActive(root, md),
		})
	}
	sum := followSummary{activeCount: len(rows)}
	if len(rows) == 0 {
		return sum
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].when.After(rows[j].when) })
	top := rows[0]
	sum.last = &followLast{project: top.md.Project, run: top.md.ID, state: top.state}
	return sum
}

// stateForActive renders the inline state cell for the idle screen's
// "last:" segment. Pushed runs are "awaiting merge"; in_progress runs
// carry their workflow's parked next-stage name. A workflow-lookup or
// Next failure degrades to the bare workflow name rather than dropping
// the row — the operator still sees *which* run was last touched.
func stateForActive(root string, md *run.Metadata) string {
	if md.Status == run.StatusPushed {
		return "awaiting merge"
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return md.Workflow
	}
	next, kind, err := wf.Next(root, md)
	if err != nil || kind != NextKindStage || next == nil {
		return md.Workflow
	}
	return md.Workflow + ":" + next.Name
}

// idleLine renders the single line moe prints when no design is in
// play. Mirrors the design doc's example shape:
//
//	(no design in play · 2 active · last: tele/fix-it awaiting merge)
//
// With no active runs, the trailing "last:" segment drops.
func idleLine(s followSummary) string {
	parts := []string{
		"no design in play",
		fmt.Sprintf("%d active", s.activeCount),
	}
	if s.last != nil {
		parts = append(parts, fmt.Sprintf("last: %s/%s %s",
			s.last.project, s.last.run, s.last.state))
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

func drainSignal(ch <-chan os.Signal) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// followPath drives one path's lifetime: spawn pager, watch the canvas
// for rewrites, and respawn the pager whenever the watcher fires.
// Returns true when the operator asked to exit (Ctrl-C through the
// post-quit countdown), false when the caller should re-evaluate via
// pickFollowTarget.
//
// The inner respawn loop exists because of the design's "Re-spawn keeps
// the same path" rule: a canvas rewrite isn't a signal that a different
// run is in play, just that *this* run's agent wrote bytes — so we
// reopen against the same path with no countdown. Operator-quit (`q`)
// or a pager crash falls through to the outer countdown + re-evaluate
// path, same as before the watcher existed.
func followPath(path string, sigCh <-chan os.Signal, stdout, stderr io.Writer) bool {
	for {
		// Drain anything queued before the pager takes the tty — a
		// Ctrl-C just before the spawn shouldn't race the pager for
		// the signal. While the pager runs, SIGINT is the pager's to
		// handle (less consumes it; the operator's `q` is the clean
		// exit) — we just don't read from sigCh, so the buffered
		// notify intercept keeps the default tear-down from firing on
		// moe.
		drainSignal(sigCh)
		cmd, err := startPager(path)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return true
		}

		watchStop := make(chan struct{})
		watchFired := watchCanvas(path, watchStop)
		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()

		var respawnRequested bool
		select {
		case <-waitErr:
			// Pager exited on its own — operator quit or crashed.
			// Tell the watcher to stop; it may have already returned
			// (close-fired race), in which case the send-to-stop
			// channel just closes a channel nobody reads. Safe.
			close(watchStop)
		case <-watchFired:
			// Canvas changed and quiesced; kill the pager so we can
			// respawn against the fresh inode. SIGTERM the process
			// *group* so we reach the inner pager, not just the `sh
			// -c` wrapper — Setpgid:true on startPager makes
			// cmd.Process.Pid the pgid leader.
			respawnRequested = true
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-waitErr:
			case <-time.After(followKillGrace):
				// Pager ignored SIGTERM (rare). Last-resort SIGKILL;
				// terminal may be left in raw mode but at least we
				// don't deadlock on a wedged pager.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-waitErr
			}
		}
		drainSignal(sigCh)

		// Distinguish kill-from-respawn vs. operator-quit using the
		// pager's exit code. less exits 0 on a clean `q`; SIGTERM-
		// killed less exits non-zero (signal-terminated). The exit-
		// code check is load-bearing for the q-and-watcher-fire race:
		// without it, every near-simultaneous `q` would leave a stray
		// respawn the operator has to dismiss.
		exitCode := 0
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		if respawnRequested && exitCode != 0 {
			// We killed it; respawn immediately on the same path —
			// the operator didn't ask to exit, they shouldn't have to
			// press Ctrl-C through a countdown to stay in the follow.
			continue
		}

		// exit 0 (clean q), or non-zero with no respawn flag (pager
		// crash / external signal). Both fall through to the dwell.
		// Countdown after the pager exits is the operator's escape
		// hatch from the relaunch loop: without it, a fresh pager
		// re-spawns immediately and the only way out is timing a
		// Ctrl-C through that brief window. The first pager open
		// isn't delayed — the operator just typed `moe follow` and
		// asked for it; the dwell only sits between exits. Scoped
		// sigint subscription mirrors queue's pattern so the
		// countdown's signal handling doesn't clash with the outer
		// sigCh used by the idle-screen branch.
		countdownSig, stopCountdownSig := installSigint()
		stopped := runCountdown(followCountdownSeconds, func(n int) string {
			return fmt.Sprintf("follow: re-checking in %d…  (Ctrl-C to exit)", n)
		}, stdout, countdownSig)
		stopCountdownSig()
		return stopped
	}
}

// startPager launches ${MOE_PAGER:-less -R -M} with path appended as
// the final argument and returns the running *exec.Cmd. Stdin/stdout/
// stderr are wired to the operator's terminal so the pager owns the
// screen for its lifetime. The command is run via `sh -c` so a pager
// string with embedded flags (`MOE_PAGER='less -R'`,
// `MOE_PAGER='glow -p'`) parses the way the operator wrote it without
// us reimplementing shell word-splitting.
//
// Setpgid:true puts sh and the inner pager in their own process group
// rooted at cmd.Process.Pid. The watcher-respawn path needs to signal
// the inner pager (not just the sh wrapper); `syscall.Kill(-pgid, …)`
// fans out to every member of the group.
func startPager(path string) (*exec.Cmd, error) {
	pager := os.Getenv("MOE_PAGER")
	if pager == "" {
		pager = defaultPager
	}
	cmd := exec.Command("sh", "-c", pager+` "$@"`, "moe-follow", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("follow: pager %q failed to start: %w", pager, err)
	}
	return cmd, nil
}

// watchCanvas polls path every followWatchPoll and signals via the
// returned channel once the canvas has been quiet for followWatchDebounce
// after a detected change. The returned channel is closed exactly once,
// then the goroutine exits; callers reading from it should treat the
// close as the respawn trigger.
//
// `stop` is the operator-quit teardown: closing it makes the watcher
// return without firing. The watcher polls (`os.Stat`) rather than
// reaching for fsnotify per moe-follow.md's locked stdlib-only stance;
// 250ms is cheap enough that no one notices.
//
// Edge cases the design calls out:
//
//   - **File mid-rewrite when watcher reads.** A truncate-then-write
//     can catch us reading at zero size between calls. The next stat
//     (one tick later) sees the post-write size, the debounce timer
//     resets, and the eventual respawn opens the finished file. Worst
//     case is one extra respawn cycle — the price of stat-polling.
//   - **File deleted briefly during rename.** os.Stat returns an error
//     during the gap. We treat stat-error as "no change yet" and don't
//     trip — the next successful stat sees the new inode's mtime/size
//     and trips normally.
func watchCanvas(path string, stop <-chan struct{}) <-chan struct{} {
	// Snapshot the tunables in the calling goroutine so a test that
	// flips them via t.Cleanup doesn't race the still-running watcher
	// (which would otherwise read the globals from the spawned
	// goroutine without synchronization).
	poll := followWatchPoll
	debounce := followWatchDebounce
	fired := make(chan struct{})
	go func() {
		ticker := time.NewTicker(poll)
		defer ticker.Stop()

		var lastMtime time.Time
		var lastSize int64
		var haveLast bool
		// pendingDeadline / hasPending implements the debounce as a
		// time-of-deadline check rather than a separate timer. Avoids
		// the time.Timer.Reset/drain dance and folds naturally into
		// the same per-tick wakeup.
		var pendingDeadline time.Time
		var hasPending bool

		for {
			select {
			case <-stop:
				// Caller asked us to tear down (operator quit). Exit
				// without closing fired — the channel-close contract
				// is "the watcher decided to fire", and on stop we
				// did not.
				return
			case <-ticker.C:
				if fi, err := os.Stat(path); err == nil {
					switch {
					case !haveLast:
						lastMtime = fi.ModTime()
						lastSize = fi.Size()
						haveLast = true
					case !fi.ModTime().Equal(lastMtime) || fi.Size() != lastSize:
						lastMtime = fi.ModTime()
						lastSize = fi.Size()
						pendingDeadline = time.Now().Add(debounce)
						hasPending = true
					}
				}
				// Stat errors are intentionally ignored — see the
				// "file deleted briefly during rename" edge case
				// above. The next successful stat resyncs.
				if hasPending && !time.Now().Before(pendingDeadline) {
					close(fired)
					return
				}
			}
		}
	}()
	return fired
}
