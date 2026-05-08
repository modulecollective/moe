package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sandbox"
	"github.com/modulecollective/moe/internal/session"
)

// `moe follow` keeps the run currently in play in front of the operator.
// Auto-pick surfaces the most recent run with an *open stage session* and
// drives `hunk diff --watch` against the workspace that session is
// mutating: a code session resolves to the run's sandbox clone diffed
// against the project's default branch; every other stage resolves to
// the session's bureaucracy worktree diffed against main. Parked runs
// (work-to-do, not work-being-done) are deliberately invisible to
// auto-pick — `dash` is the surface for those. `--run <id>` is the
// explicit pin escape hatch and ignores liveness, but a pinned run with
// no open session still falls through to idle.
//
// hunk owns its own tty and watcher: when it exits, follow waits out a
// 3s countdown (Ctrl-C exits cleanly) so the operator can break the
// relaunch loop, then re-evaluates and either spawns a fresh hunk or
// drops to a single-line idle screen polled every --interval. Read-
// only by construction: hunk is a viewer.
//
// While hunk is running, a watcher polls pickFollowTarget on the same
// idle cadence; if the followTarget the loop would now pick no longer
// matches what hunk is rendering, the watcher SIGTERMs hunk so the
// loop can re-spawn against the new target without waiting on the
// operator's `q`. State-change rotations skip the post-exit countdown
// because the operator didn't ask to bail.
//
// Pieces reused from `dash`: same Scan, same journal index, same
// session.List for liveness. follow is the across-runs counterpart to
// dash's within-run liveness picker.

// followIdleInterval is the default poll cadence between idle ticks.
// pickFollowTarget runs in roughly half a second on a populated tree;
// a 1s tick keeps the idle screen feeling live without the "is it
// hung?" smell of a longer dwell, and shrinks the operator-quit
// window between ticks where ^C felt unresponsive.
const followIdleInterval = 1 * time.Second

// followCountdownSeconds is the dwell after hunk exits before the next
// auto-pick fires — long enough for a Ctrl-C to land cleanly, short
// enough not to feel sluggish. Mirrors queueCountdownSeconds.
const followCountdownSeconds = 3

func init() {
	Register(&Command{
		Name:    "follow",
		Summary: "page diffs of the run currently in play; idle when none",
		Run:     runFollow,
	})
}

func runFollow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", followIdleInterval, "polling interval when no run is in play")
	runFilter := fs.String("run", "", "lock follow to a specific run id (matches across projects)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe follow [--interval <duration>] [--run <id>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Drives `hunk diff --watch` against the run most worth watching: the most")
		moePrintln(stderr, "recent in-progress run with an open stage session. Code sessions resolve")
		moePrintln(stderr, "to the run's sandbox clone diffed against the project's default branch;")
		moePrintln(stderr, "every other stage resolves to the session's bureaucracy worktree diffed")
		moePrintln(stderr, "against main. When hunk exits, follow waits 3s (Ctrl-C to exit cleanly),")
		moePrintln(stderr, "then re-evaluates. With no live session, prints a single-line idle status")
		moePrintln(stderr, "and re-checks every --interval. --run <id> pins to a specific run; with")
		moePrintln(stderr, "no live session for the pinned run, follow stays idle and keeps polling.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	// Fail fast with a clear install hint if hunk isn't installed —
	// otherwise the first auto-pick would tear down the idle screen,
	// fail to exec, and bounce back through the countdown on every
	// tick. One up-front check is cheaper to reason about.
	if _, err := exec.LookPath("hunk"); err != nil {
		moePrintln(stderr, "moe follow: hunk is not on PATH")
		moePrintln(stderr, "  install: https://github.com/modem-dev/hunk")
		return 1
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	for {
		target, summary, err := pickFollowTarget(root, *runFilter)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if target.Dir != "" {
			if quit := followTargetRun(root, *runFilter, target, sigCh, stdout, stderr); quit {
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

// followTarget is the (workspace dir, diff base) tuple hunk runs
// against. Empty Dir means no candidate — the caller renders idle.
type followTarget struct {
	Dir  string
	Base string
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

// pickFollowTarget resolves the workspace hunk should diff, plus the
// idle-screen summary for when no run is in play. Returns an empty
// followTarget when no candidate exists; the caller renders the
// summary instead.
//
// A candidate is an in-progress run with an open stage session — work-
// being-done. Parked runs (work-to-do but untouched) deliberately
// don't surface here; that's `dash`'s job. Most-recent journal
// activity wins. runFilter, when non-empty, locks to that run id —
// the operator's pin overrides liveness, but a pinned run with no
// open session still falls through to idle so follow keeps polling
// until the operator (or a queued chain) opens one.
func pickFollowTarget(root, runFilter string) (followTarget, followSummary, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return followTarget{}, followSummary{}, err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return followTarget{}, followSummary{}, err
	}
	// session.List is read-only; a worktree-list error shouldn't
	// suppress the idle-screen summary, so swallow and proceed with no
	// liveness signal — auto-pick will simply find no candidates.
	// We index by run id; resolveFollowTarget routes per-doc.
	sessionsByRun := make(map[string][]*session.Session)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionsByRun[s.Run] = append(sessionsByRun[s.Run], s)
		}
	}
	summary := buildFollowSummary(root, mds, idx)

	if runFilter != "" {
		for _, md := range mds {
			if md.ID != runFilter {
				continue
			}
			sess := pickSessionForRun(sessionsByRun[md.ID])
			if sess == nil {
				return followTarget{}, summary, nil
			}
			target, err := resolveFollowTarget(root, md, sess)
			if err != nil {
				return followTarget{}, summary, err
			}
			return target, summary, nil
		}
		return followTarget{}, summary, nil
	}

	type cand struct {
		target followTarget
		when   time.Time
	}
	var cands []cand
	for _, md := range mds {
		if md.Status != run.StatusInProgress {
			continue
		}
		sess := pickSessionForRun(sessionsByRun[md.ID])
		if sess == nil {
			continue
		}
		target, err := resolveFollowTarget(root, md, sess)
		if err != nil {
			return followTarget{}, summary, err
		}
		if target.Dir == "" {
			continue
		}
		cands = append(cands, cand{
			target: target,
			when:   idx.LastActivity[md.ID],
		})
	}
	if len(cands) == 0 {
		return followTarget{}, summary, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].when.After(cands[j].when)
	})
	return cands[0].target, summary, nil
}

// pickSessionForRun chooses a single session per run when more than
// one happens to be open. A run normally has at most one open session
// at a time, but a botched close could leave an orphan around;
// preferring code over design over alphabetical produces a
// deterministic answer that biases toward the workspace nearest the
// run's likely live stage.
func pickSessionForRun(sessions []*session.Session) *session.Session {
	if len(sessions) == 0 {
		return nil
	}
	if len(sessions) == 1 {
		return sessions[0]
	}
	sort.Slice(sessions, func(i, j int) bool {
		ri, rj := stageRank(sessions[i].Doc), stageRank(sessions[j].Doc)
		if ri != rj {
			return ri < rj
		}
		return sessions[i].Doc < sessions[j].Doc
	})
	return sessions[0]
}

// stageRank biases pickSessionForRun toward the doc most likely to be
// the live workspace. Lower wins.
func stageRank(doc string) int {
	switch doc {
	case "code":
		return 0
	case "design":
		return 1
	default:
		return 2
	}
}

// resolveFollowTarget routes a session to the workspace tuple hunk
// runs in. Code sessions diff the run's sandbox clone against the
// project's recorded default branch; every other stage diffs the
// session's bureaucracy worktree against the run's OpenedFrom (the
// commit the run branched from), falling back to "main" when the run
// pre-dates the field. The OpenedFrom anchor means hunk's pane shows
// the run's contribution since open — including any --from-idea seed
// that landed on main as part of the open commit. The dir is stat'd
// as defense-in-depth so an orphaned session record (worktree dir
// gone, or sandbox not yet cloned) idles instead of feeding hunk a
// non-existent cwd.
func resolveFollowTarget(root string, md *run.Metadata, sess *session.Session) (followTarget, error) {
	if sess.Doc == "code" {
		dir := sandbox.Path(root, md.Project, md.ID)
		if _, err := os.Stat(dir); err != nil {
			return followTarget{}, nil
		}
		proj, err := project.Load(root, md.Project)
		if err != nil {
			return followTarget{}, err
		}
		return followTarget{Dir: dir, Base: proj.DefaultBranch}, nil
	}
	if _, err := os.Stat(sess.WorktreePath); err != nil {
		return followTarget{}, nil
	}
	base := md.OpenedFrom
	if base == "" {
		base = "main"
	}
	return followTarget{Dir: sess.WorktreePath, Base: base}, nil
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

// idleLine renders the single line moe prints when no run is in play.
// Mirrors the design doc's example shape:
//
//	(no run in play · 2 active · last: tele/fix-it awaiting merge)
//
// With no active runs, the trailing "last:" segment drops.
func idleLine(s followSummary) string {
	parts := []string{
		"no run in play",
		fmt.Sprintf("%d active", s.activeCount),
	}
	if s.last != nil {
		parts = append(parts, fmt.Sprintf("last: %s/%s %s",
			s.last.project, s.last.run, s.last.state))
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

// drainSignal empties any queued ^C deliveries on ch without blocking.
// Used between hunk runs and the countdown so a stale signal from the
// idle screen doesn't pre-trip the next select.
func drainSignal(ch <-chan os.Signal) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// followTargetRun spawns `hunk diff --watch <base>` rooted at
// target.Dir and waits for it to exit. Returns true when the operator
// asked to exit (Ctrl-C in the post-exit countdown), false when the
// caller should re-evaluate via pickFollowTarget.
//
// hunk owns its own tty: it inherits stdin/stdout/stderr, stays in
// moe's process group, and handles its own raw-mode setup, watcher,
// and SIGINT. moe just waits for it to exit. A Ctrl-C from the
// operator while hunk is running reaches both processes (shared PG):
// hunk tears itself down, moe waits for it, then drops into the
// post-exit countdown — same as `q` or a hunk crash. ^C while hunk
// is up means "stop watching this target", not "exit follow"; the
// operator's escape hatch is a second ^C in the countdown, which
// runCountdown's scoped sigint handler catches.
//
// A watcher goroutine runs alongside cmd.Wait, polling pickFollowTarget
// at the same cadence as the idle screen. When the followTarget the
// loop would now pick no longer matches the one hunk was launched
// against — pinned run advanced a stage, ranking flipped, session
// closed — the watcher SIGTERMs hunk so the loop can re-spawn against
// the new target without waiting for the operator's `q`. State-change
// rotations skip the post-exit countdown; operator-driven exits keep
// it.
func followTargetRun(root, runFilter string, target followTarget, sigCh <-chan os.Signal, stdout, stderr io.Writer) bool {
	drainSignal(sigCh)
	cmd := exec.Command("hunk", "diff", "--watch", target.Base)
	cmd.Dir = target.Dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		moePrintf(stderr, "follow: hunk failed to start: %v\n", err)
		// exec.ErrNotFound here is recoverable in principle (operator
		// installs hunk and re-runs), but during the loop we'd just
		// bounce; the up-front LookPath check has already caught the
		// no-binary case, so a Start failure now is a real I/O error
		// — exit follow rather than spin.
		if errors.Is(err, exec.ErrNotFound) {
			moePrintln(stderr, "  install: https://github.com/modem-dev/hunk")
		}
		return true
	}

	// Install the countdown's signal subscriber up front, before the
	// watcher and the wait-select. Any ^C the operator hits between
	// here and runCountdown — including the ^C that closed hunk and
	// the race window between waitErr/sigCh in the select below —
	// lands in countdownSig. We drain the matching delivery for the
	// ^C-that-closed-hunk before runCountdown so it doesn't pre-trip
	// the first iteration; anything else stays queued and exits the
	// countdown on the first select.
	//
	// Buffer 4 (vs installSigint's 1): signal.Notify is non-blocking,
	// so a single-slot buffer would drop the operator's "exit follow"
	// ^C if it lands while the ^C-that-closed-hunk is still queued.
	// We need room for at least the close-^C + one exit-^C.
	countdownSig := make(chan os.Signal, 4)
	signal.Notify(countdownSig, os.Interrupt)
	defer signal.Stop(countdownSig)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	rotateCh := make(chan struct{}, 1)
	stopWatcher := make(chan struct{})
	go func() {
		// Two reasons to wake on this goroutine:
		// (1) rotation — pickFollowTarget has changed under us and we
		//     should signal hunk to exit so the loop re-picks. Polled
		//     every followIdleInterval to keep up with state changes.
		// (2) post-UI hang — hunk handles ^C by reading 0x03 from
		//     stdin (raw mode), runs its UI cleanup (alt-screen exit
		//     + termios restore), but doesn't always exit the
		//     process. When that happens cmd.Wait blocks forever and
		//     moe appears hung. We detect the cleanup having run by
		//     watching for ICANON to flip from cleared (raw) back to
		//     set (cooked); when it does, we send hunk a real SIGINT
		//     to push its process the rest of the way out.
		//
		// Termios poll runs more often than rotation (200ms vs 1s) so
		// the operator's ^C-on-hunk → "hunk reappears or follow shows
		// the countdown" cycle stays snappy.
		const tick = 200 * time.Millisecond
		const pickEvery = 5 // 5 * 200ms = 1s, matches followIdleInterval
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		ticks := 0
		rawSeen := false
		for {
			select {
			case <-stopWatcher:
				return
			case <-ticker.C:
			}

			// Termios poll — see (2) above. ICANON cleared = hunk has
			// switched the tty to raw mode (UI live); ICANON set after
			// we've seen it cleared = hunk's UI cleanup has run, so the
			// process is in the post-UI hang state and a SIGINT will
			// push it out via its signal-driven exit path. Skip the
			// poll silently if we're not on a tty (ENOTTY); rotation
			// still works.
			if attrs, err := getTermios(0); err == nil {
				isRaw := attrs.Lflag&syscall.ICANON == 0
				switch {
				case !rawSeen && isRaw:
					rawSeen = true
				case rawSeen && !isRaw:
					_ = cmd.Process.Signal(syscall.SIGINT)
					return
				}
			}

			ticks++
			if ticks < pickEvery {
				continue
			}
			ticks = 0

			fresh, _, err := pickFollowTarget(root, runFilter)
			if err != nil {
				// Same forgiveness pickFollowTarget extends to its
				// own session.List failures: a transient I/O blip
				// shouldn't yank the current hunk. Keep watching.
				continue
			}
			if fresh == target {
				continue
			}
			// SIGINT is best-effort — if hunk has already exited
			// (e.g. operator just hit `q`), the signal returns an
			// error we don't care about. Either way, signal the
			// caller that this exit is a state-change rotation so
			// the countdown is skipped.
			//
			// SIGINT (not SIGTERM): hunk's SIGINT handler runs the
			// alt-screen exit + termios restore that the operator-^C
			// path relies on; SIGTERM kills the process before that
			// teardown runs and leaves the tty wedged.
			_ = cmd.Process.Signal(syscall.SIGINT)
			select {
			case rotateCh <- struct{}{}:
			default:
			}
			return
		}
	}()

	rotated := false
	closedByOperator := false
	select {
	case <-waitErr:
		// hunk exited — `q`, internal Ctrl-C, a crash, our SIGINT
		// for state-change rotation, or operator ^C reaching hunk
		// via the shared PG (and racing waitErr against sigCh).
		select {
		case <-rotateCh:
			rotated = true
		default:
		}
		// If operator ^C raced waitErr to win this select, the same
		// SIGINT is sitting in sigCh; drain it so it doesn't mislead
		// the next iteration, and remember that we did so.
		select {
		case <-sigCh:
			closedByOperator = true
		default:
		}
	case <-sigCh:
		// Operator ^C; the kernel delivered the same SIGINT to hunk
		// via the shared PG. Wait for hunk to tear down so the
		// terminal's mode is restored before we touch stdout, then
		// fall through to the countdown — see followTargetRun's doc
		// comment.
		closedByOperator = true
		// Bound the wait. Hunk's SIGINT teardown should be fast (alt-
		// screen exit + termios restore + exit) but we've seen reports
		// where the process visibly tears down its UI yet keeps the
		// process alive long enough that moe appears hung. If hunk
		// hasn't reaped in two seconds, force the issue with a kill so
		// the loop can move on. Termios is already restored by hunk's
		// own teardown; SIGKILL after that point doesn't leave the tty
		// any worse off.
		select {
		case <-waitErr:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-waitErr
		}
	}
	// Don't <-watcherDone — close(stopWatcher) signals the watcher to
	// return on its next select iteration, but if it's currently mid-
	// pickFollowTarget that adds up to a poll's worth of latency
	// before the countdown can paint. Letting the watcher tail off in
	// the background is safe: its rotateCh is unread by anyone after
	// we return, its cmd.Process.Signal call on a dead hunk is a
	// harmless no-op, and the next followTargetRun's watcher is fully
	// independent (own ticker, own channels, own cmd).
	close(stopWatcher)
	drainSignal(sigCh)
	if rotated {
		// State changed under us; the operator didn't ask to bail.
		// Skip the countdown so the loop re-picks immediately against
		// whatever's live now.
		return false
	}

	// Drain countdownSig's matching delivery for the ^C that closed
	// hunk so it doesn't pre-trip the countdown's first iteration.
	// Anything that arrived during the race window or the cleanup
	// gap above stays queued — that's the operator's "exit follow"
	// intent, and runCountdown's first select will catch it.
	if closedByOperator {
		select {
		case <-countdownSig:
		default:
		}
	}


	// Countdown after hunk exits is the operator's escape hatch from
	// the relaunch loop: without it, a fresh hunk re-spawns
	// immediately and the only way out is timing a Ctrl-C through
	// that brief window. The first hunk open isn't delayed — the
	// operator just typed `moe follow` and asked for it; the dwell
	// only sits between exits.
	return runCountdown(followCountdownSeconds, func(n int) string {
		return fmt.Sprintf("follow: re-checking in %d…  (Ctrl-C to exit)", n)
	}, stdout, countdownSig)
}
