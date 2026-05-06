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

// defaultPager is the fallback when MOE_PAGER is unset. `+F` is the
// only universally-available follow mode; `-R` passes ANSI through;
// `-M` shows the long prompt so the file path lands on the status
// line — that's how the operator knows *which* design they're reading.
const defaultPager = "less +F -R -M"

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
		moePrintln(stderr, "Pager is ${MOE_PAGER:-less +F -R -M}.")
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
			// Drain anything queued before the pager takes the tty —
			// a Ctrl-C just before the spawn shouldn't race the pager
			// for the signal. While the pager runs, SIGINT is the
			// pager's to handle (less consumes it; +F drops to normal
			// less and re-exposes the path) — we just don't read from
			// sigCh, so the buffered notify intercept keeps the
			// default tear-down from firing on moe.
			drainSignal(sigCh)
			if err := spawnPager(path); err != nil {
				moePrintf(stderr, "%v\n", err)
				return 1
			}
			drainSignal(sigCh)
			// Countdown after the pager exits is the operator's escape
			// hatch from the relaunch loop: without it, a fresh pager
			// re-spawns immediately and the only way out is timing a
			// Ctrl-C through that brief window. The first pager open
			// isn't delayed — the operator just typed `moe follow`
			// and asked for it; the dwell only sits between exits.
			// Scoped sigint subscription mirrors queue's pattern so
			// the countdown's signal handling doesn't clash with the
			// outer sigCh used by the idle-screen branch.
			countdownSig, stopCountdownSig := installSigint()
			stopped := runCountdown(followCountdownSeconds, func(n int) string {
				return fmt.Sprintf("follow: re-checking in %d…  (Ctrl-C to exit)", n)
			}, stdout, countdownSig)
			stopCountdownSig()
			if stopped {
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
	sessionDocsByRun := make(map[string][]string)
	if ss, err := session.List(root); err == nil {
		for _, s := range ss {
			sessionDocsByRun[s.Run] = append(sessionDocsByRun[s.Run], s.Doc)
		}
	}
	summary := buildFollowSummary(root, mds, idx)

	if runFilter != "" {
		for _, md := range mds {
			if md.ID != runFilter {
				continue
			}
			path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "design"))
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
		live := false
		for _, doc := range sessionDocsByRun[md.ID] {
			if doc == "design" {
				live = true
				break
			}
		}
		if !live {
			continue
		}
		cands = append(cands, cand{
			path: filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")),
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

// spawnPager runs ${MOE_PAGER:-less +F -R -M} with path appended as
// the final argument. Stdin/stdout/stderr are wired to the operator's
// terminal so the pager owns the screen for its lifetime. The command
// is run via `sh -c` so a pager string with embedded flags
// (`MOE_PAGER='less +F -R'`, `MOE_PAGER='glow -p'`) parses the way the
// operator wrote it without us reimplementing shell word-splitting.
func spawnPager(path string) error {
	pager := os.Getenv("MOE_PAGER")
	if pager == "" {
		pager = defaultPager
	}
	cmd := exec.Command("sh", "-c", pager+` "$@"`, "moe-follow", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("follow: pager %q exited %w", pager, err)
	}
	return nil
}
