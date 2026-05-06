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
// It resolves which file deserves the screen — most recent run with an
// open design session, else most recent run parked at the design stage
// — and execs a pager. When the pager exits, follow re-evaluates and
// either spawns a fresh pager (different design surfaced, or the same
// run still wants reading) or drops to a single-line idle screen and
// re-checks every --interval. It is read-only by construction: the
// canvas is opened for read, no `$EDITOR` path.
//
// Pieces are deliberately reused from `dash`: same Scan, same journal
// index, same session.List for liveness, same workflow.Next for the
// parked stage. follow is the across-runs counterpart to dash's
// within-run liveness picker.

// followIdleInterval is the default poll cadence between idle ticks.
// 5s is the design's chosen cadence: long enough that the operator's
// terminal isn't busy, short enough that a freshly-opened design lands
// in the pager within one breath.
const followIdleInterval = 5 * time.Second

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
		moePrintln(stderr, "Pages the design doc most worth watching: most recent run with an open")
		moePrintln(stderr, "design session, else most recent run parked at the design stage. When")
		moePrintln(stderr, "the pager exits, follow re-evaluates. With no design in play, prints a")
		moePrintln(stderr, "single-line idle status and re-checks every --interval. Ctrl-C exits.")
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
// A design candidate is a run that is either (a) parked at the design
// stage under its workflow's parking rule, or (b) has an open session
// on its design document. Tier (b) beats (a); within a tier the most
// recent activity wins. runFilter, when non-empty, locks to that run id
// — the operator's pin overrides the usual selection, and a not-yet-
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
	// session.List is read-only; a worktree-list error shouldn't suppress
	// the dash's parking-rule pick, so swallow and proceed without
	// liveness signal — same shape dash uses for the [running] marker.
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
		live bool
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
		parked := false
		if wf, lerr := LookupWorkflow(md.Workflow); lerr == nil {
			next, kind, nerr := wf.Next(root, md)
			if nerr != nil {
				return "", summary, nerr
			}
			if kind == NextKindStage && next != nil && next.Name == "design" {
				parked = true
			}
		}
		if !live && !parked {
			continue
		}
		cands = append(cands, cand{
			path: filepath.Join(root, run.ContentPath(md.Project, md.ID, "design")),
			when: idx.LastActivity[md.ID],
			live: live,
		})
	}
	if len(cands) == 0 {
		return "", summary, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].live != cands[j].live {
			return cands[i].live
		}
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
