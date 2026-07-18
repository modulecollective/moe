package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// `moe queue` is the per-project holding pen for fix runs the pulse
// proposed. It exists to give those runs one stable thing to review and
// one verb to kick.
//
// The runs themselves are ordinary parked sdlc runs; the queue run is a
// placeholder chained ahead of them (queue → fix1 → fix2), so the whole
// batch renders as one chain unit on the dash and `moe queue kick
// <project>` closes the head and rides the chain headlessly through the
// existing `!!!` machinery. Without the placeholder the handle would
// move every time the head ships, appending across pulses would mean
// hunting the current tail, and there would be no stable name to review.
//
// Like idea and intent, no `moe queue` verb launches an agent on the
// queue canvas — the canvas is moe-owned, one line appended per queued
// run. Unlike them, the queue is machine-authored: the pulse writes it,
// the operator prunes it (via `moe chain edit`) and kicks it.
const (
	queueWorkflow = "queue"
	// queueDoc is the nominal stage's document id. The queue canvas
	// lives at documents/queue/content.md.
	queueDoc = "queue"
)

// queueCanvasSkeleton is what a freshly minted queue run opens with.
// The harness appends one line per queued run under `## Queued`.
const queueCanvasSkeleton = `# Queue

Fix runs the pulse proposed as high-confidence, bounded, and verifiable.
Each is a parked sdlc run chained behind this one. Prune or reorder with
` + "`moe chain edit`" + `; kick the batch to walk them headlessly.

## Queued
`

func init() {
	g := NewCommandGroup(queueWorkflow, "queue workflow — the pulse's parked fix runs, kicked as one batch")
	g.Register(&Command{
		Name:    "kick",
		Summary: "close the project's queue run and ride its chain headlessly",
		Run:     runQueueKick,
	})
	g.Register(closeCommand(queueWorkflow, "Close queue run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a queue run's canvas to stdout",
		Run:     runCat(queueWorkflow, queueDoc),
		argKind: argProjectRun,
	})
	RegisterGroup(g)

	// Register the workflow so run.Load, dash bucketing, chain edit, and
	// the cat resolver's wf.Stages() all resolve it. The single stage
	// `queue` lives in the DAG without a matching `moe queue queue` verb
	// — same shape as idea and intent. Nothing ever opens an agent
	// session on it.
	w := NewWorkflow(queueWorkflow)
	w.RegisterStage(queueDoc)
	RegisterWorkflow(w)
}

func runQueueKick(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("queue kick", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe queue kick <project>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Closes the project's live queue run and rides its chain: each")
		moePrintln(stderr, "queued fix run is walked design -> ... -> push headlessly, in the")
		moePrintln(stderr, "order the chain records. Reorder or prune with `moe chain edit`")
		moePrintln(stderr, "first. The next pulse that proposes a fix mints a fresh queue.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "queue kick: %v\n", err)
		return 1
	}

	md, err := liveQueueRun(root, projectID)
	if err != nil {
		moePrintf(stderr, "queue kick: %v\n", err)
		return 1
	}
	if md == nil {
		moePrintf(stderr, "queue kick: no live queue run for %s — nothing to kick\n", projectID)
		return 1
	}

	// Close the head first, then ride: the queue is a placeholder, and
	// leaving it open while its chain executes would put a run on the
	// dash's ACTIVE list that nobody is ever going to work. skipEdit
	// because the queue carries no followups worth an editor pop, and
	// tailPulse=false because a queue close is not run traffic — the fix
	// runs it rides each tail their own pulse at push.
	if err := closeRunInProcess(root, queueWorkflow, "Close queue run %s/%s", nil,
		projectID, md.ID, true /*skipEdit*/, false /*tailPulse*/, stdout, stderr); err != nil {
		moePrintf(stderr, "queue kick: close %s/%s: %v\n", projectID, md.ID, err)
		return 1
	}
	moePrintf(stdout, "closed queue %s/%s — riding the chain\n", projectID, md.ID)

	// rideChain=true is the `!!!` vocabulary: walk this run's chained
	// child to its ship, then its child, and so on. maybeRideChain is
	// the same seam the cascade's terminal stages use, so the summary
	// lines, interrupt handling, and per-run exit codes are shared.
	return maybeRideChain(md, true /*rideChain*/, stdout, stderr)
}

// liveQueueRun returns the project's in-progress queue run, or nil when
// there is none. At most one is live at a time by construction — the
// spawn path mints one only when this returns nil — but if a second ever
// appears (a hand-opened run, a crash mid-mint) the newest by slug wins
// and the older one is left for the operator, rather than guessing.
func liveQueueRun(root, projectID string) (*run.Metadata, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var newest *run.Metadata
	for _, md := range mds {
		if md.Workflow != queueWorkflow || md.Project != projectID || md.Status != run.StatusInProgress {
			continue
		}
		if newest == nil || md.ID > newest.ID {
			newest = md
		}
	}
	return newest, nil
}

// chainTail walks the live chain from headKey to its last live link.
// Returns headKey itself when the head has no live child. byKey is the
// caller's existing scan, keyed qualified; the seen set guards against a
// corrupt index spinning the walk.
func chainTail(idx *run.JournalIndex, byKey map[string]*run.Metadata, headKey string) string {
	seen := map[string]bool{headKey: true}
	cur := headKey
	for {
		child, ok := idx.ChainedChild[cur]
		if !ok || child == "" || seen[child] || !run.ChainChildLive(child, byKey) {
			return cur
		}
		seen[child] = true
		cur = child
	}
}

// appendQueueEntries writes one line per newly queued run under the
// queue canvas's `## Queued` heading and commits it together with the
// chain edges the spawn established. One commit, not one per run: the
// batch is one event, and BuildJournalIndex's grep picks up
// MoE-Chained-To alongside the MoE-Run trailer on the same commit.
//
// Caller holds the repolock.
func appendQueueEntries(root, projectID, queueID string, lines, edges []string) error {
	canvasRel := run.ContentPath(projectID, queueID, queueDoc)
	body, err := os.ReadFile(filepath.Join(root, canvasRel))
	if err != nil {
		return fmt.Errorf("read queue canvas: %w", err)
	}
	updated := strings.TrimRight(string(body), "\n") + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write queue canvas: %w", err)
	}

	msg := fmt.Sprintf("queue: append %d run(s) to %s/%s\n\n", len(lines), projectID, queueID) +
		trailers.Block{
			Run:       queueID,
			Project:   projectID,
			Workflow:  queueWorkflow,
			Document:  queueDoc,
			ChainedTo: edges,
		}.String()
	return run.StageAndCommit(root, msg, run.DocDir(projectID, queueID, queueDoc))
}

// stampQueueBatch is the durable half of a spawn batch: it mints a queue
// run if the project has none live, appends the batch's lines to the
// queue canvas, and stamps the chain edges that string the new runs onto
// the queue's tail. Runs under its own repolock acquisition and pushes
// the journal, since it is called after the survey's own lock windows
// have closed.
func stampQueueBatch(root, projectID string, spawned []spawnedRun, stdout, stderr io.Writer) error {
	if len(spawned) == 0 {
		return nil
	}
	queueMD, err := liveQueueRun(root, projectID)
	if err != nil {
		return err
	}
	if queueMD == nil {
		queueMD, err = mintQueueRun(root, projectID, stdout, stderr)
		if err != nil {
			return fmt.Errorf("mint queue run: %w", err)
		}
		moePrintf(stderr, "pulse: opened queue %s/%s\n", projectID, queueMD.ID)
	}

	mds, err := run.Scan(root)
	if err != nil {
		return err
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return err
	}

	queueKey := projectID + "/" + queueMD.ID
	tail := chainTail(idx, byKey, queueKey)

	var lines, edges []string
	for _, s := range spawned {
		childKey := projectID + "/" + s.runID
		edges = append(edges, tail+" "+childKey)
		tail = childKey
		lines = append(lines, fmt.Sprintf("- `%s` — %s (proposed by %s): %s", s.runID, s.title, s.pulseSlug, s.why))
	}

	return sync.WithJournalPush(root, repolock.Options{
		Purpose: "queue-append",
		Run:     queueKey,
	}, stdout, stderr, func() error {
		return appendQueueEntries(root, projectID, queueMD.ID, lines, edges)
	})
}

// mintQueueRun opens the project's queue placeholder. IDBase rather
// than a fixed ID so a project that has queued before (and closed that
// queue) gets a fresh dated slug rather than colliding with history.
func mintQueueRun(root, projectID string, stdout, stderr io.Writer) (*run.Metadata, error) {
	return runopen.Open(root, projectID, run.Options{
		IDBase:   queueWorkflow,
		Workflow: queueWorkflow,
		SeedDocs: map[string]string{queueDoc: queueCanvasSkeleton},
	}, stdout, stderr)
}
