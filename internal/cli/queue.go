package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/queue"
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

// queueWorkflowSDLC and queueWorkflowIdea name the workflow values
// the queue accepts on add/remove. sdlc items are walked directly via
// runResume; idea items are promoted-and-walked lazily at dispatch
// time (see dispatchQueueItem). New workflow values reshape the verb
// only by extending these constants — the JSON shape stays the same.
const (
	queueWorkflowSDLC = "sdlc"
	queueWorkflowIdea = "idea"
)

// queueWorkflows enumerates the workflow values queue add/remove will
// accept. Kept here (rather than computed from the workflow registry)
// so the operator-facing error message lists exactly what the queue
// supports, not every workflow with a `new` verb.
var queueWorkflows = []string{queueWorkflowSDLC, queueWorkflowIdea}

func isQueueableWorkflow(workflow string) bool {
	for _, w := range queueWorkflows {
		if w == workflow {
			return true
		}
	}
	return false
}

func init() {
	g := NewCommandGroup("queue", "queue verbs: add, remove, list, edit, run — walk a curated playlist of opened runs")
	g.Register(&Command{
		Name:    "add",
		Summary: "queue an opened sdlc run, or an idea to promote-then-walk lazily",
		Run:     runQueueAdd,
	})
	g.Register(&Command{
		Name:    "remove",
		Summary: "remove a queued item by identity",
		Run:     runQueueRemove,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "show the queue with each item's next stage (or drop reason)",
		Run:     runQueueList,
	})
	g.Register(&Command{
		Name:    "edit",
		Summary: "open the queue in $EDITOR to reorder or drop items (git rebase -i flavour)",
		Run:     runQueueEdit,
	})
	g.Register(&Command{
		Name:    "run",
		Summary: "walk the queue and exit when empty",
		Run:     runQueueRun,
	})
	RegisterGroup(g)
}

func runQueueAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	front := fs.Bool("front", false, "prepend to the head of the queue instead of appending to the back")
	agentOverride := fs.String("agent", "", "(idea only) agent backend to stamp on the run when the walker lazy-promotes it (claude/codex)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue add [--front] sdlc <project> <run>")
		moePrintln(stderr, "       moe queue add [--front] [--agent <name>] idea <project> <slug>")
		fs.PrintDefaults()
	}
	// `--from-idea` was the eager-promote flavor. The new lazy form —
	// `queue add idea <project> <slug>` — supersedes it: the queue holds
	// the idea pointer until the walker dispatches it. Scan args before
	// fs.Parse so a typed-by-muscle-memory invocation lands on the
	// migration hint, not the stdlib's "flag provided but not defined."
	for _, a := range args {
		if a == "--from-idea" || a == "-from-idea" ||
			strings.HasPrefix(a, "--from-idea=") || strings.HasPrefix(a, "-from-idea=") {
			moePrintln(stderr, "queue add: --from-idea was removed; use `moe queue add idea <project> <slug>` to lazily queue an idea (the walker promotes it on dispatch)")
			return 2
		}
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	workflow, projectID, runID := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	if !isQueueableWorkflow(workflow) {
		moePrintf(stderr, "queue add: workflow %q not supported (use one of: %s)\n", workflow, strings.Join(queueWorkflows, ", "))
		return 2
	}
	// --agent applies only to idea entries — the lazy-promote shape is
	// where the agent name actually needs to flow somewhere. sdlc
	// entries reference an already-open run that carries its own
	// run.json.Agent, so a flag here would mean two sources of truth.
	if *agentOverride != "" {
		if workflow != queueWorkflowIdea {
			moePrintf(stderr, "queue add: --agent only applies to `idea` entries; sdlc entries inherit their run's persisted agent\n")
			return 2
		}
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "queue add: %v\n", err)
		return 1
	}
	if md.Workflow != workflow {
		moePrintf(stderr, "queue add: %s %s %s is a %s run, not %s\n", workflow, projectID, runID, md.Workflow, workflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted, run.StatusPushed:
		moePrintf(stderr, "queue add: %s %s %s is %s; nothing to queue\n", workflow, projectID, runID, md.Status)
		return 1
	}

	item := queue.Item{Workflow: workflow, Project: projectID, Run: runID, Agent: *agentOverride}
	err = withRepoLock(root, repolock.Options{
		Purpose: "queue-add",
		Run:     projectID + "/" + runID,
	}, func() error {
		items, err := queue.Load(root)
		if err != nil {
			return err
		}
		if pos := queue.IndexOf(items, item); pos > 0 {
			return fmt.Errorf("already queued at position %d", pos)
		}
		items = queue.AddItem(items, item, *front)
		return queue.Save(root, items)
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
		moePrintln(stderr, "usage: moe queue remove sdlc|idea <project> <run>")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		return 2
	}
	workflow := fs.Arg(0)
	if !isQueueableWorkflow(workflow) {
		moePrintf(stderr, "queue remove: workflow %q not supported (use one of: %s)\n", workflow, strings.Join(queueWorkflows, ", "))
		return 2
	}
	projectID, runID := fs.Arg(1), fs.Arg(2)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	target := queue.Item{Workflow: workflow, Project: projectID, Run: runID}
	var removed bool
	err = withRepoLock(root, repolock.Options{
		Purpose: "queue-remove",
		Run:     projectID + "/" + runID,
	}, func() error {
		items, err := queue.Load(root)
		if err != nil {
			return err
		}
		out, ok := queue.RemoveFirst(items, target)
		if !ok {
			return nil
		}
		removed = true
		return queue.Save(root, out)
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
	items, err := queue.Load(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if len(items) == 0 {
		moePrintln(stdout, "(queue is empty)")
		return 0
	}
	for i, it := range items {
		moePrintf(stdout, "%d. %s    %s\n", i+1, it, queueItemPreview(root, it))
	}
	return 0
}

// queueItemPreview returns the right-hand column of `queue list`:
// either `next: <stage>` for a live item or `(will drop: <reason>)`
// for one the walker would skip. Idea items render as `promote → sdlc,
// next: design` (or `… next: code` when the source idea was itself
// promoted from an upstream idea, so the seeded canvas would let the
// walker skip design) — they're queued pointers, not opened runs, so
// the column has to predict the post-promote state rather than read it.
// Stays in cli because it consults the per-workflow registry;
// queue.Classify covers the dropped-state half of the same shape.
func queueItemPreview(root string, it queue.Item) string {
	live, reason := queue.Classify(root, it)
	if live != queue.LivenessReady {
		return "(will drop: " + reason + ")"
	}
	if it.Workflow == queueWorkflowIdea {
		// At dispatch the walker promotes the idea into a fresh sdlc
		// run with the idea's canvas seeded into the first stage. The
		// next sdlc stage is always design — promoteIdeaToSdlcRun
		// seeds design from the idea's canvas and the run is
		// in_progress with no stage turns yet.
		return "promote → sdlc, next: design"
	}
	md, err := run.Load(root, it.Project, it.Run)
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		return fmt.Sprintf("(will drop: %v)", err)
	}
	if kind != NextKindStage || next == "" {
		return "next: (none — at merge gate)"
	}
	return "next: " + next
}

// dispatchQueueItem invokes the per-item entry for one queue item.
// Hot-pluggable so tests can swap in a deterministic stub that
// records invocation order and exit codes without spawning Claude.
//
// Important: dispatch runs *outside* the queue's repolock. The
// walker peeks under lock, releases, dispatches here, then
// re-acquires the lock to identity-pop. That contract is what keeps
// a concurrent `queue add` from another terminal unblocked while a
// stage session is grinding away.
var dispatchQueueItem = defaultDispatchQueueItem

func defaultDispatchQueueItem(it queue.Item, stdout, stderr io.Writer) int {
	switch it.Workflow {
	case queueWorkflowSDLC:
		return cascadeQueuedSdlcResume(it.Project, it.Run, stdout, stderr)
	case queueWorkflowIdea:
		// Lazy promote at dispatch. promoteIdeaToSdlcRun takes its own
		// repolock for the run-new and idea-promote commits; that's
		// outside the queue's lock window (peek released before dispatch).
		root, err := findRoot(stderr)
		if err != nil {
			return 1
		}
		md, err := promoteIdeaToSdlcRun(root, it.Project, it.Run, it.Agent)
		switch {
		case md == nil && err != nil:
			moePrintf(stderr, "queue: %v\n", err)
			return 1
		case err != nil:
			// New run is open; idea wasn't flipped. Surface the warning
			// and keep going — same shape as queue add --from-idea took.
			moePrintf(stderr, "queue: %v\n", err)
		}
		moePrintf(stdout, "queue: promoted idea %s %s → sdlc %s %s\n", it.Project, it.Run, md.Project, md.ID)
		// Resume against the freshly-opened sdlc run. Failure here
		// leaves the queue's idea-pop ungrabbed by the walker, the
		// idea status=promoted, and the new sdlc run not in the
		// queue. Recovery is the operator pair:
		//   moe queue remove idea <project> <slug>
		//   moe queue add sdlc <project> <new-slug>
		// — the new slug is in the stdout line above, hence the loud
		// print before cascade.
		return cascadeQueuedSdlcResume(md.Project, md.ID, stdout, stderr)
	default:
		moePrintf(stderr, "queue: workflow %q not supported by walker\n", it.Workflow)
		return 1
	}
}

// cascadeQueuedSdlcResume is the per-item walker the queue dispatches.
// Mirrors runResume's pre-flight (refuses missing/terminal/pushed
// runs) then either parks at the push gate (when stages are still
// pending) or hands straight to it (when the only thing left is the
// merge gate). The cascade itself is the same driver the chain
// prompt's `!push` answer uses; items in the queue are work the
// operator already triaged, so the walker grinds them headless to
// the ship decision rather than re-opening every stage interactively.
// Operators who want full interactivity on one item pull it off the
// queue (`moe queue remove`) and run `moe sdlc resume` on it.
func cascadeQueuedSdlcResume(projectID, runID string, stdout, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "sdlc resume: %v\n", err)
		return 1
	}
	if md.Workflow != "sdlc" {
		moePrintf(stderr, "sdlc resume: %s %s is a %s run, not sdlc\n", projectID, runID, md.Workflow)
		return 1
	}
	switch md.Status {
	case run.StatusMerged, run.StatusClosed, run.StatusPromoted:
		moePrintf(stderr, "sdlc resume: %s %s is %s; nothing to resume\n", projectID, runID, md.Status)
		return 1
	case run.StatusPushed:
		moePrintf(stderr, "sdlc resume: %s %s already pushed; resume cannot drive a pushed run\n", projectID, runID)
		return 1
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	nextStage, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if kind != NextKindStage || nextStage == "" || nextStage == "push" {
		// Nothing to cascade — hand straight to the chain prompt so the
		// push gate fires (or so a terminal-shaped run falls through).
		return promptNextStage(root, md, "", stdout, stderr)
	}
	res, code := cascadeFromGate(nextStage, "push", md, stdout, stderr)
	if summary := renderCascadeSummary(res); summary != "" {
		moePrintln(stdout, summary)
	}
	if code != 0 {
		return code
	}
	var lastStage string
	if len(res.ran) > 0 {
		lastStage = res.ran[len(res.ran)-1].stage
	}
	return promptNextStage(root, md, lastStage, stdout, stderr)
}

func runQueueRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue run")
		moePrintln(stderr, "")
		moePrintln(stderr, "Walks the queue, cascading each item headlessly to its push gate")
		moePrintln(stderr, "and prompting [N/m/p] before chaining to the next item. Items in")
		moePrintln(stderr, "the queue are work already triaged; the walker grinds them rather")
		moePrintln(stderr, "than re-opening every stage interactively. Pull an item out with")
		moePrintln(stderr, "`moe queue remove` if you want full interactivity on it.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Exits when the queue is empty; relaunch after `queue add` to drain more.")
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

	// Walker-scoped SIGINT subscription. Catches Ctrl-C delivered at
	// the stage prompt (where the prompt's own helper has already
	// returned "decline" — but the walker still needs to know to stop
	// the loop). The countdown registers its own scoped channel below;
	// Go's signal.Notify multiplexes a single SIGINT to every subscribed
	// channel, so both fire on one Ctrl-C without conflict. Buffered
	// size 1 — one observed delivery is enough to stop, extra ones
	// during dispatch drop and we exit on the first.
	walkerSig, stopWalkerSig := installSigint()
	defer stopWalkerSig()

	for {
		var head queue.Item
		var depth int
		var empty bool
		err = withRepoLock(root, repolock.Options{Purpose: "queue-peek"}, func() error {
			items, err := queue.Load(root)
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
		live, reason := queue.Classify(root, head)
		if live != queue.LivenessReady {
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
		// down. The walker-scoped channel also receives the same
		// signal — redundant when the countdown caught it,
		// load-bearing for the post-dispatch drain when SIGINT lands
		// at the stage prompt instead.
		countdownSig, stopCountdownSig := installSigint()
		stopped := runCountdown(queueCountdownSeconds, func(n int) string {
			return fmt.Sprintf("queue: starting %s in %d…  (Ctrl-C to stop)", head, n)
		}, stdout, countdownSig)
		stopCountdownSig()
		if stopped {
			moePrintf(stdout, "queue: stopped — %s still at head (%d remaining)\n", head, depth)
			return 0
		}

		code := dispatchQueueItem(head, stdout, stderr)
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
		// dispatch returned 0 here — but signal.Notify also fanned
		// the SIGINT into walkerSig and it is sitting in the buffer.
		// Non-blocking drain so we honour it before grabbing the
		// next head.
		select {
		case <-walkerSig:
			remaining := depth - 1
			if remaining < 0 {
				remaining = 0
			}
			moePrintf(stdout, "queue: stopped (%d remaining)\n", remaining)
			return 0
		default:
		}
	}
}

// queuePopIdentity removes target from .moe/queue.json by identity
// (workflow+project+run), under the repo lock. No-op when target is
// no longer in the queue (e.g. the operator `queue remove`'d it
// from another terminal while dispatch was in flight). The identity
// match — rather than a positional pop — is the contract that makes
// concurrent add/remove safe against an in-flight walker.
func queuePopIdentity(root string, target queue.Item) error {
	return withRepoLock(root, repolock.Options{
		Purpose: "queue-pop",
		Run:     target.Project + "/" + target.Run,
	}, func() error {
		items, err := queue.Load(root)
		if err != nil {
			return err
		}
		out, removed := queue.RemoveFirst(items, target)
		if !removed {
			return nil
		}
		return queue.Save(root, out)
	})
}

// queueEditHeader is the comments block at the top of the tempfile
// `queue edit` pops into $EDITOR. Lines starting with `#` are stripped
// at parse time; the operator never has to copy this past the editor.
const queueEditHeader = `# moe queue edit — reorder or drop items. Save & exit to apply.
# Lines starting with # are ignored. Remove a line to remove an item.
# Adds use ` + "`moe queue add`" + ` — not allowed here.
#
`

// queueEditNow is overridable so tests can pin a stable timestamp in
// the backup-file name. Production passes time.Now().Unix().
var queueEditNow = func() int64 { return time.Now().Unix() }

func runQueueEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue edit")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens the queue in $EDITOR. Move lines to reorder; delete lines")
		moePrintln(stderr, "to drop items. Adds use `moe queue add`.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "queue edit: set $EDITOR or $VISUAL — queue edit needs an editor")
		return 1
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}

	// Step 1: snapshot under the lock, render to tempfile, release.
	var snapshot []queue.Item
	err = withRepoLock(root, repolock.Options{Purpose: "queue-edit-snapshot"}, func() error {
		items, err := queue.Load(root)
		if err != nil {
			return err
		}
		snapshot = items
		return nil
	})
	if err != nil {
		moePrintf(stderr, "queue edit: %v\n", err)
		return 1
	}
	if len(snapshot) == 0 {
		// Matches `queue list`'s empty-queue surface — no editor, no
		// no-op write. The design's open question came down on the side
		// of read-verb parity here.
		moePrintln(stdout, "(queue is empty)")
		return 0
	}

	tmpDir, err := os.MkdirTemp("", "moe-queue-edit-")
	if err != nil {
		moePrintf(stderr, "queue edit: tempdir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, "queue.txt")
	if err := os.WriteFile(tmpPath, renderQueueForEdit(root, snapshot), 0o644); err != nil {
		moePrintf(stderr, "queue edit: write tempfile: %v\n", err)
		return 1
	}

	// Step 2: editor with no lock held. launchEditor wires stdio at the
	// terminal level — same helper `idea edit` uses.
	if code := launchEditor(tmpPath, stdout, stderr); code != 0 {
		return code
	}

	// Step 3 + 4: re-acquire the lock, compare against snapshot, parse,
	// save. Concurrent change refusal fires here, after the editor exit.
	err = withRepoLock(root, repolock.Options{Purpose: "queue-edit-apply"}, func() error {
		current, err := queue.Load(root)
		if err != nil {
			return err
		}
		if !itemsEqual(current, snapshot) {
			path, saveErr := backupQueueEdit(root, tmpPath)
			if saveErr != nil {
				return fmt.Errorf("queue changed while editing; also failed to back up the buffer: %w", saveErr)
			}
			return fmt.Errorf("queue changed while editing; re-run `moe queue edit` (your edits saved to %s)", path)
		}
		edited, perr := parseQueueEdit(tmpPath, snapshot)
		if perr != nil {
			path, saveErr := backupQueueEdit(root, tmpPath)
			if saveErr != nil {
				return fmt.Errorf("%w; also failed to back up the buffer: %v", perr, saveErr)
			}
			return fmt.Errorf("%w (your edits saved to %s)", perr, path)
		}
		return queue.Save(root, edited)
	})
	if err != nil {
		moePrintf(stderr, "queue edit: %v\n", err)
		return 1
	}
	moePrintln(stdout, "queue updated")
	return 0
}

// renderQueueForEdit produces the tempfile body the operator edits.
// Header + one line per item, with the right-hand preview column
// rendered as a `#` comment so it survives parse-strip but signals
// next-stage / drop state to the operator.
func renderQueueForEdit(root string, items []queue.Item) []byte {
	var b strings.Builder
	b.WriteString(queueEditHeader)
	// Pre-compute the longest identity so previews line up vertically;
	// `queue list` does the same dance implicitly via tabular printf
	// but we render to a static buffer here.
	maxLen := 0
	identities := make([]string, len(items))
	for i, it := range items {
		identities[i] = it.String()
		if n := len(identities[i]); n > maxLen {
			maxLen = n
		}
	}
	for i, it := range items {
		preview := queueItemPreview(root, it)
		pad := strings.Repeat(" ", maxLen-len(identities[i]))
		fmt.Fprintf(&b, "%s%s    # %s\n", identities[i], pad, preview)
	}
	return []byte(b.String())
}

// parseQueueEdit reads the tempfile, strips comments, and validates
// that every remaining line is `<workflow> <project> <run>` with a
// known workflow keyword and an identity present in snapshot. Returns
// the new slice in the order the operator wrote — duplicates and
// previously-absent identities are refused as design calls for.
func parseQueueEdit(path string, snapshot []queue.Item) ([]queue.Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tempfile: %w", err)
	}
	allowed := make(map[queue.Item]struct{}, len(snapshot))
	for _, it := range snapshot {
		allowed[it] = struct{}{}
	}
	seen := make(map[queue.Item]struct{}, len(snapshot))
	var out []queue.Item
	for ln, raw := range strings.Split(string(b), "\n") {
		line := stripQueueEditComment(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: expected `<workflow> <project> <run>`, got %q", ln+1, line)
		}
		it := queue.Item{Workflow: fields[0], Project: fields[1], Run: fields[2]}
		if !isQueueableWorkflow(it.Workflow) {
			return nil, fmt.Errorf("line %d: workflow %q not supported (use one of: %s)", ln+1, it.Workflow, strings.Join(queueWorkflows, ", "))
		}
		if _, ok := allowed[it]; !ok {
			return nil, fmt.Errorf("line %d: %s was not in the queue when editing began; use `moe queue add` to add new items", ln+1, it)
		}
		if _, dup := seen[it]; dup {
			return nil, fmt.Errorf("line %d: %s appears twice", ln+1, it)
		}
		seen[it] = struct{}{}
		out = append(out, it)
	}
	return out, nil
}

// stripQueueEditComment removes a trailing `# …` from a line and trims
// whitespace. A line that starts with `#` (after leading whitespace) is
// returned as empty. Mirrors git rebase -i's comment behaviour.
func stripQueueEditComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// itemsEqual is identity-equality on two queue snapshots. Slice
// comparison via reflect.DeepEqual would also do, but a length check
// plus value loop is shorter and avoids importing reflect for one call.
func itemsEqual(a, b []queue.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// backupQueueEdit copies the edited tempfile into the bureaucracy
// alongside .moe/queue.json so the operator can recover their work
// after a refusal. The path shape `.moe/queue.json.edit-<unix>.bak`
// matches the design's `<unix>` suffix; the timestamp comes from
// queueEditNow() so tests can pin it.
func backupQueueEdit(root, tmpPath string) (string, error) {
	src, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(queue.Path(root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("queue.json.edit-%s.bak", strconv.FormatInt(queueEditNow(), 10))
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}
