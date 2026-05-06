package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// queueCountdownSeconds is the dwell at the top of every dispatch — the
// operator's predictable Ctrl-C window between items. Fixed at 3s per
// design: long enough to land a keystroke, short enough not to feel
// sluggish, no flag (per project no-config policy). Applies uniformly to
// the first item too, so an unexpected queue head can be aborted before
// any agent starts.
const queueCountdownSeconds = 3

// `moe queue` is the operator's playlist of opened runs to grind
// through in one sitting. Items are structured (workflow, project, run)
// triples — not raw command strings — so the walker can re-check
// liveness on each peek and drop dead items (merged, closed, missing)
// instead of trying to drive them. Storage at .moe/queue.json is
// operator-local working state, like .moe/clones/ and .moe/worktrees/;
// not committed.
//
// The walker holds the repo-wide lock only around the brief
// load-modify-save windows of peek and pop — never during the in-flight
// per-item dispatch. That keeps `queue add` from another terminal from
// blocking while a stage session is running, and lets identity-matched
// pop survive a concurrent add/remove of any item.

// queueWorkflowSDLC names the only workflow queueable in v1. The CLI
// takes it as a positional on add/remove so adding a second workflow later
// (when one earns --one-shot) doesn't reshape the verb.
const queueWorkflowSDLC = "sdlc"

// queueItem is one entry in .moe/queue.json. workflow + project + run
// is the identity used for duplicate refusal and identity-matched pop.
type queueItem struct {
	Workflow string `json:"workflow"`
	Project  string `json:"project"`
	Run      string `json:"run"`
}

func (q queueItem) String() string {
	return fmt.Sprintf("%s %s/%s", q.Workflow, q.Project, q.Run)
}

func init() {
	Register(&Command{
		Name:    "queue",
		Summary: "queue workflow: add, remove, list, run — walk a curated playlist of opened runs",
		Run:     runQueue,
	})
}

func runQueue(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printQueueUsage(stdout)
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		printQueueUsage(stdout)
		return 0
	case "add":
		return runQueueAdd(args[1:], stdout, stderr)
	case "remove":
		return runQueueRemove(args[1:], stdout, stderr)
	case "list":
		return runQueueList(args[1:], stdout, stderr)
	case "run":
		return runQueueRun(args[1:], stdout, stderr)
	default:
		moePrintf(stderr, "unknown queue subcommand %q\n", args[0])
		printQueueUsage(stderr)
		return 1
	}
}

func printQueueUsage(w io.Writer) {
	moePrintln(w, "usage: moe queue <subcommand> [args...]")
	moePrintln(w, "")
	moePrintln(w, "subcommands:")
	moePrintf(w, "  %-14s  %s\n", "add", "queue an opened run, or promote-and-queue an idea")
	moePrintf(w, "  %-14s  %s\n", "remove", "remove a queued run by identity")
	moePrintf(w, "  %-14s  %s\n", "list", "show the queue with each item's next stage (or drop reason)")
	moePrintf(w, "  %-14s  %s\n", "run", "walk the queue, pausing at each merge gate")
}

// queuePath is the on-disk JSON file. Lives under .moe/ alongside
// clones/ and worktrees/ — operator-local, never committed.
func queuePath(root string) string {
	return filepath.Join(root, ".moe", "queue.json")
}

// loadQueue reads .moe/queue.json. A missing or empty file is a normal
// state (no runs queued yet) and returns (nil, nil).
func loadQueue(root string) ([]queueItem, error) {
	p := queuePath(root)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: read %s: %w", p, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var items []queueItem
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("queue: parse %s: %w", p, err)
	}
	return items, nil
}

// saveQueue writes items to .moe/queue.json with a deterministic
// indent. Always writes a JSON array — empty queue persists as `[]`
// rather than a missing file so the caller can tell "explicitly empty"
// from "never used."
func saveQueue(root string, items []queueItem) error {
	p := queuePath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("queue: mkdir %s: %w", filepath.Dir(p), err)
	}
	if items == nil {
		items = []queueItem{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("queue: marshal: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return fmt.Errorf("queue: write %s: %w", p, err)
	}
	return nil
}

// queueIndexOf returns the 1-based position of an identity-matching
// item, or 0 if not present.
func queueIndexOf(items []queueItem, target queueItem) int {
	for i, it := range items {
		if it == target {
			return i + 1
		}
	}
	return 0
}

func runQueueAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	front := fs.Bool("front", false, "prepend to the head of the queue instead of appending to the back")
	fromIdea := fs.String("from-idea", "", "promote an open idea (by slug) to a sdlc run, then queue the resulting run")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue add [--front] sdlc <project> <run>")
		moePrintln(stderr, "       moe queue add [--front] sdlc --from-idea=<slug> <project>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	workflow := fs.Arg(0)
	if workflow != queueWorkflowSDLC {
		moePrintf(stderr, "queue add: workflow %q not supported (only %q today)\n", workflow, queueWorkflowSDLC)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	var projectID, runID string
	switch {
	case *fromIdea != "":
		if fs.NArg() != 2 {
			moePrintln(stderr, "queue add --from-idea: expected sdlc <project>")
			fs.Usage()
			return 2
		}
		projectID = fs.Arg(1)
		md, err := promoteIdeaToSdlcRun(root, projectID, *fromIdea)
		switch {
		case md == nil && err != nil:
			// Hard failure: the new run never opened. Nothing to queue.
			moePrintf(stderr, "queue add: %v\n", err)
			return 1
		case err != nil:
			// Soft failure: new run opened but the idea wasn't flipped
			// to promoted. Mirrors runNew's behaviour — surface the
			// warning but keep going; the run is queueable on its own.
			moePrintf(stderr, "queue add: %v\n", err)
		}
		runID = md.ID
		moePrintf(stdout, "promoted idea %s/%s to sdlc run %s/%s\n", projectID, *fromIdea, projectID, runID)
	default:
		if fs.NArg() != 3 {
			fs.Usage()
			return 2
		}
		projectID = fs.Arg(1)
		runID = fs.Arg(2)
		md, err := run.Load(root, projectID, runID)
		if err != nil {
			moePrintf(stderr, "queue add: %v\n", err)
			return 1
		}
		if md.Workflow != queueWorkflowSDLC {
			moePrintf(stderr, "queue add: %s/%s is a %s run, not sdlc\n", projectID, runID, md.Workflow)
			return 1
		}
		switch md.Status {
		case run.StatusMerged, run.StatusClosed, run.StatusPromoted, run.StatusPushed:
			moePrintf(stderr, "queue add: %s/%s is %s; nothing to queue\n", projectID, runID, md.Status)
			return 1
		}
	}

	item := queueItem{Workflow: workflow, Project: projectID, Run: runID}
	err = withRepoLock(root, repolock.Options{
		Purpose: "queue-add",
		Run:     projectID + "/" + runID,
	}, func() error {
		items, err := loadQueue(root)
		if err != nil {
			return err
		}
		if pos := queueIndexOf(items, item); pos > 0 {
			return fmt.Errorf("already queued at position %d", pos)
		}
		if *front {
			items = append([]queueItem{item}, items...)
		} else {
			items = append(items, item)
		}
		return saveQueue(root, items)
	})
	if err != nil {
		moePrintf(stderr, "queue add: %v\n", err)
		return 1
	}
	moePrintf(stdout, "queued %s\n", item)
	return 0
}

func runQueueRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue remove sdlc <project> <run>")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	workflow := fs.Arg(0)
	if workflow != queueWorkflowSDLC {
		moePrintf(stderr, "queue remove: workflow %q not supported (only %q today)\n", workflow, queueWorkflowSDLC)
		return 2
	}
	projectID, runID := fs.Arg(1), fs.Arg(2)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	target := queueItem{Workflow: workflow, Project: projectID, Run: runID}
	var removed bool
	err = withRepoLock(root, repolock.Options{
		Purpose: "queue-remove",
		Run:     projectID + "/" + runID,
	}, func() error {
		items, err := loadQueue(root)
		if err != nil {
			return err
		}
		out := items[:0]
		for _, it := range items {
			if !removed && it == target {
				removed = true
				continue
			}
			out = append(out, it)
		}
		if !removed {
			return nil
		}
		return saveQueue(root, out)
	})
	if err != nil {
		moePrintf(stderr, "queue remove: %v\n", err)
		return 1
	}
	if !removed {
		moePrintf(stderr, "queue remove: %s not in queue\n", target)
		return 1
	}
	moePrintf(stdout, "unqueued %s\n", target)
	return 0
}

func runQueueList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue list")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
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
	items, err := loadQueue(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if len(items) == 0 {
		moePrintln(stdout, "(queue is empty)")
		return 0
	}
	for i, it := range items {
		moePrintf(stdout, "%d. %s %s/%s    %s\n", i+1, it.Workflow, it.Project, it.Run, queueItemPreview(root, it))
	}
	return 0
}

// queueItemPreview returns the right-hand column of `queue list`: either
// `next: <stage>` for a live item or `(will drop: <reason>)` for one
// the walker would skip. Drives off the same liveness signals the
// walker uses, so what list shows is what run will do.
func queueItemPreview(root string, it queueItem) string {
	md, err := run.Load(root, it.Project, it.Run)
	if errors.Is(err, os.ErrNotExist) {
		return "(will drop: run not found)"
	}
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	if md.Workflow != it.Workflow {
		return fmt.Sprintf("(will drop: workflow=%s)", md.Workflow)
	}
	switch md.Status {
	case run.StatusMerged:
		return "(will drop: status=merged)"
	case run.StatusClosed:
		return "(will drop: status=closed)"
	case run.StatusPromoted:
		return "(will drop: status=promoted)"
	case run.StatusPushed:
		return "(will drop: status=pushed)"
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	if kind != NextKindStage || next == nil {
		return "next: (none — at merge gate)"
	}
	return "next: " + next.Name
}

// queueDispatchResult is what dispatchQueueItem reports back to the
// walker: the per-item entry's exit code plus whether the item was
// dropped before dispatch (so the walker doesn't emit a misleading
// "starting" line for a dead item).
type queueLiveness int

const (
	queueLivenessReady queueLiveness = iota
	queueLivenessDropMissing
	queueLivenessDropTerminal
	queueLivenessDropOther
)

// classifyQueueItem decides whether the walker should dispatch the
// item or drop it. Returns the liveness verdict and a short reason
// suitable for the walker's drop log line. Mirrors queueItemPreview's
// classification but returns a typed verdict rather than a string.
func classifyQueueItem(root string, it queueItem) (queueLiveness, string) {
	md, err := run.Load(root, it.Project, it.Run)
	if errors.Is(err, os.ErrNotExist) {
		return queueLivenessDropMissing, "run not found"
	}
	if err != nil {
		return queueLivenessDropOther, err.Error()
	}
	if md.Workflow != it.Workflow {
		return queueLivenessDropOther, "workflow=" + md.Workflow
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted, run.StatusPushed:
		return queueLivenessDropTerminal, "already " + md.Status
	}
	return queueLivenessReady, ""
}

// queueDispatchOpts controls how the walker drives each item. OneShot
// flips per-item dispatch from interactive to headless — same shape as
// `moe sdlc resume --one-shot` vs `moe sdlc resume` typed by hand.
type queueDispatchOpts struct {
	OneShot bool
}

// dispatchQueueItem invokes the per-item entry for one queue item.
// Hot-pluggable so tests can swap in a deterministic stub that records
// invocation order and exit codes without spawning Claude. The default
// dispatches in-process — same address space as the walker — because
// the walker is already running in `moe queue run`'s process and an
// extra fork-exec adds latency without changing semantics.
//
// Important: dispatch runs *outside* the queue's repolock. The walker
// peeks under lock, releases, dispatches here, then re-acquires the
// lock to identity-pop. That contract is what keeps a concurrent
// `queue add` from another terminal unblocked while a stage session
// is grinding away.
var dispatchQueueItem = defaultDispatchQueueItem

func defaultDispatchQueueItem(it queueItem, opts queueDispatchOpts, stdout, stderr io.Writer) int {
	switch it.Workflow {
	case queueWorkflowSDLC:
		var args []string
		if opts.OneShot {
			args = append(args, "--one-shot")
		}
		args = append(args, it.Project, it.Run)
		return runResume(args, stdout, stderr)
	default:
		moePrintf(stderr, "queue: workflow %q not supported by walker\n", it.Workflow)
		return 1
	}
}

func runQueueRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	oneShot := fs.Bool("one-shot", false, "drive each item headlessly via `claude -p` instead of opening interactive sessions")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue run [--one-shot]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Walks the queue, pausing at each merge gate. Default opens an")
		moePrintln(stderr, "interactive session per pending stage; --one-shot drives stages")
		moePrintln(stderr, "headlessly so the operator only stops at [N/m/p].")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
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
	dispatchOpts := queueDispatchOpts{OneShot: *oneShot}

	// Walker-scoped SIGINT handler. Catches Ctrl-C delivered at the
	// stage prompt (where the prompt's own helper has already returned
	// "decline" — but the walker still needs to know to stop the loop)
	// and any other cooked-mode point during the loop's body. The
	// countdown registers its own scoped channel below; Go's
	// signal.Notify multiplexes a single SIGINT to every subscribed
	// channel, so both fire on one Ctrl-C without conflict.
	walkerSig, stopWalkerSig := installSigint()
	defer stopWalkerSig()
	var stopRequested atomic.Bool
	walkerDone := make(chan struct{})
	defer close(walkerDone)
	go func() {
		for {
			select {
			case <-walkerSig:
				stopRequested.Store(true)
			case <-walkerDone:
				return
			}
		}
	}()

	for {
		var head queueItem
		var depth int
		var empty bool
		err = withRepoLock(root, repolock.Options{Purpose: "queue-peek"}, func() error {
			items, err := loadQueue(root)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				empty = true
				return nil
			}
			head = items[0]
			depth = len(items)
			return nil
		})
		if err != nil {
			moePrintf(stderr, "queue run: %v\n", err)
			return 1
		}
		if empty {
			moePrintln(stdout, "queue: empty")
			return 0
		}

		// Liveness check is outside the lock — run.Load is read-only
		// and the on-disk run.json doesn't need our protection. Drop
		// dead items without invoking dispatch, identity-popping so
		// concurrent edits don't shift the wrong item out. Dropped
		// items skip the countdown — the operator never sees a
		// "starting" frame for an item we're about to discard.
		liveness, reason := classifyQueueItem(root, head)
		if liveness != queueLivenessReady {
			moePrintf(stdout, "queue: dropping %s (%s)\n", head, reason)
			if err := queuePopIdentity(root, head); err != nil {
				moePrintf(stderr, "queue: pop %s: %v\n", head, err)
				return 1
			}
			continue
		}

		// Countdown gates every dispatch, including the first. Scoped
		// SIGINT channel lets `runCountdown` return cleanly on Ctrl-C
		// instead of letting Go's default handler tear the process
		// down. The walker-scoped handler also receives the same
		// signal and flips stopRequested — redundant when the
		// countdown caught it, load-bearing when SIGINT lands at the
		// stage prompt later.
		countdownSig, stopCountdownSig := installSigint()
		stopped := runCountdown(queueCountdownSeconds, func(n int) string {
			return fmt.Sprintf("queue: starting %s in %d…  (Ctrl-C to stop)", head, n)
		}, stdout, countdownSig)
		stopCountdownSig()
		if stopped {
			moePrintf(stdout, "queue: stopped — %s still at head (%d remaining)\n", head, depth)
			return 0
		}

		code := dispatchQueueItem(head, dispatchOpts, stdout, stderr)
		if code != 0 {
			moePrintf(stderr, "queue: %s exited %d; leaving at head of queue\n", head, code)
			return code
		}
		if err := queuePopIdentity(root, head); err != nil {
			moePrintf(stderr, "queue: pop %s: %v\n", head, err)
			return 1
		}
		// Catches Ctrl-C delivered at the stage prompt: the prompt's
		// helper returned decline, the chained stage returned 0,
		// dispatch returned 0 here — but the walker-scoped handler
		// has flipped stopRequested. Honour it before grabbing the
		// next head.
		if stopRequested.Load() {
			remaining := depth - 1
			if remaining < 0 {
				remaining = 0
			}
			moePrintf(stdout, "queue: stopped (%d remaining)\n", remaining)
			return 0
		}
	}
}

// queuePopIdentity removes target from .moe/queue.json by identity
// (workflow+project+run), under the repo lock. No-op when target is no
// longer in the queue (e.g. the operator `queue remove`'d it from another
// terminal while dispatch was in flight). The identity match — rather
// than a positional pop — is the contract that makes concurrent
// add/remove safe against an in-flight walker.
func queuePopIdentity(root string, target queueItem) error {
	return withRepoLock(root, repolock.Options{
		Purpose: "queue-pop",
		Run:     target.Project + "/" + target.Run,
	}, func() error {
		items, err := loadQueue(root)
		if err != nil {
			return err
		}
		out := items[:0]
		removed := false
		for _, it := range items {
			if !removed && it == target {
				removed = true
				continue
			}
			out = append(out, it)
		}
		if !removed {
			return nil
		}
		return saveQueue(root, out)
	})
}
