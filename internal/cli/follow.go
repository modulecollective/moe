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
			if quit := followTargetRun(target, sigCh, stdout, stderr); quit {
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
// asked to exit (Ctrl-C in countdown, or Ctrl-C while hunk was
// running), false when the caller should re-evaluate via
// pickFollowTarget.
//
// hunk owns its own tty: it inherits stdin/stdout/stderr, stays in
// moe's process group, and handles its own raw-mode setup, watcher,
// and SIGINT. moe just waits for it to exit. A Ctrl-C from the
// operator while hunk is running reaches both processes (shared PG):
// hunk tears itself down, moe sees sigCh fire, and we return true so
// follow exits without pulling the operator through a countdown they
// didn't ask for.
func followTargetRun(target followTarget, sigCh <-chan os.Signal, stdout, stderr io.Writer) bool {
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
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	operatorQuit := false
	select {
	case <-waitErr:
		// hunk exited on its own — `q`, internal Ctrl-C, or a crash.
		// Drop to the post-exit countdown.
	case <-sigCh:
		// Operator hit ^C; the kernel delivered the same SIGINT to
		// hunk. Wait for it to tear down so the terminal's mode is
		// restored before we touch stdout.
		operatorQuit = true
		<-waitErr
	}
	drainSignal(sigCh)
	if operatorQuit {
		return true
	}

	// Countdown after hunk exits is the operator's escape hatch from
	// the relaunch loop: without it, a fresh hunk re-spawns
	// immediately and the only way out is timing a Ctrl-C through
	// that brief window. The first hunk open isn't delayed — the
	// operator just typed `moe follow` and asked for it; the dwell
	// only sits between exits. Scoped sigint subscription mirrors
	// queue's pattern so the countdown's signal handling doesn't
	// clash with the outer sigCh used by the idle-screen branch.
	countdownSig, stopCountdownSig := installSigint()
	stopped := runCountdown(followCountdownSeconds, func(n int) string {
		return fmt.Sprintf("follow: re-checking in %d…  (Ctrl-C to exit)", n)
	}, stdout, countdownSig)
	stopCountdownSig()
	return stopped
}
