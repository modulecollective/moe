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

// A **chain run** is a placeholder head: a run whose only job is to be
// the stable handle for the batch chained behind it. `moe chain new`
// mints one by hand (a topic — `moe chain new moe/perf-cleanups`); the
// pulse mints one per spawn batch. Either way the head holds still while
// its children ship, which is what a bare chain of ordinary runs cannot
// do — the handle would move every time the head shipped.
//
// The workflow registers **no stages**. That is the whole trick: a run
// with no ladder is trivially done the moment it exists, so `moe chain
// kick` closes it and rides on without a special case, and nothing can
// ever open an agent session on it. Its one document (`chain`) is
// registered via RegisterDoc so the canvas still resolves for serve and
// cat — it is moe-owned, one line appended per run the pulse chains,
// same posture as idea and intent.
//
// The invariant the whole surface preserves: **chaining under a parked
// head is inert; execution is operator-rooted.** The pulse proposes and
// the chain holds; only an operator kick (or their own `!!!`) executes.
const (
	chainWorkflow = "chain"
	// chainDoc is the stageless document id. The chain canvas lives at
	// documents/chain/content.md.
	chainDoc = "chain"
)

// chainCanvasSkeleton is what a freshly minted chain run opens with.
// The harness appends one line per run the pulse chains under it; runs
// the operator moves in with `moe chain edit` appear in the chain but
// get no canvas line. The chain edges are the truth kick rides; the
// canvas is commentary.
const chainCanvasSkeleton = `# Chain

Runs chained behind this head ride as one batch on kick. Reorder or
prune with ` + "`moe chain edit`" + `; kick the head to walk them headlessly.

## Chained
`

func init() {
	// The chain-run workflow. No RegisterStage call — see the file
	// comment; the empty ladder is load-bearing, not an omission.
	w := NewWorkflow(chainWorkflow)
	w.RegisterDoc(chainDoc)
	RegisterWorkflow(w)
}

func runChainNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain new <project>/<slug>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Mints a chain run: a placeholder head that holds still while the")
		moePrintln(stderr, "runs chained behind it ship. Name it for the topic it collects")
		moePrintln(stderr, "(moe chain new moe/perf-cleanups); the slug is dated on collision.")
		moePrintln(stderr, "A project can hold several live chains at once.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chain new: %v\n", err)
		return 2
	}
	if run.Slugify(slug) != slug {
		moePrintf(stderr, "chain new: %q is not a canonical slug (try %q)\n", slug, run.Slugify(slug))
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "chain new: %v\n", err)
		return 1
	}

	md, err := mintChainRun(root, projectID, slug, "" /*spawnedBy*/, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "chain new: %v\n", err)
		return 1
	}
	key := projectID + "/" + md.ID
	moePrintf(stdout, "opened chain %s\n", key)
	moePrintln(stdout, "next:")
	moePrintln(stdout, "  moe chain edit    # move runs under it")
	moePrintf(stdout, "  moe chain kick %s    # ride the chain headlessly\n", key)
	return 0
}

func runChainKick(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain kick", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain kick <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Rides the named chain from its head, headlessly: the head cascades")
		moePrintln(stderr, "to its ship, then each chained run is walked design -> ... -> push")
		moePrintln(stderr, "in the order the chain records. A chain run head has no stages, so")
		moePrintln(stderr, "it just closes and the ride carries on into its children. Reorder or")
		moePrintln(stderr, "prune with `moe chain edit` first.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chain kick: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "chain kick: %v\n", err)
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "chain kick: %v\n", err)
		return 1
	}
	// No status refusal: a head that has already shipped still heads a
	// chain of parked runs, and riding them is the point. Next() reports
	// every terminal status as done, so such a head falls into the
	// nothing-pending branch below and is left exactly as it is.
	//
	// Same admissible set the chain editor offers: every operator-paced
	// workflow, plus chain heads. A run moe never lets the operator
	// chain is a run kick has no business driving.
	if !chainableWorkflow(md.Workflow) {
		moePrintf(stderr, "chain kick: %s/%s is a %s run — not chainable\n", projectID, runID, md.Workflow)
		return 1
	}
	parent, chained, err := liveChainParent(root, md)
	if err != nil {
		moePrintf(stderr, "chain kick: build index: %v\n", err)
		return 1
	}
	if chained {
		moePrintf(stderr, "chain kick: %s/%s is chained under %s — kick the head\n", projectID, runID, parent)
		return 1
	}

	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "chain kick: %v\n", err)
		return 1
	}
	next, kind, err := wf.Next(root, md)
	if err != nil {
		moePrintf(stderr, "chain kick: next stage: %v\n", err)
		return 1
	}
	if kind == NextKindStage && next != "" {
		// A regular head with work left: a programmatic `!!!`. The
		// cascade ships this run and then rides its children through the
		// same seam the operator's own `!!!` uses — including the
		// childless case, which just ships and rides nothing.
		res, code := cascadeFromGate(next, "" /*destination*/, false /*oneStep*/, true /*rideChain*/, md, stdout, stderr)
		if summary := renderCascadeSummary(projectID+"/"+runID, res); summary != "" {
			moePrintln(stdout, summary)
		}
		return code
	}

	// Nothing pending. For a chain run that is the whole lifecycle —
	// reaching done trivially is what it exists to do — so close it. A
	// regular run with nothing pending is left open: chain ride's
	// standing decision is never to auto-close someone's parked run.
	//
	// Close before riding, not after (cascadeFromGate's non-sdlc branch
	// rides first): leaving a placeholder open while its chain executes
	// puts a run on the dash's ACTIVE list that nobody is ever going to
	// work. skipEdit because a chain head carries no followups worth an
	// editor pop, and tailPulse=false because closing a placeholder is
	// not run traffic — the runs it rides each tail their own pulse at
	// push.
	if md.Workflow == chainWorkflow && md.Status == run.StatusInProgress {
		reg, ok := lookupCloseRegistration(chainWorkflow)
		if !ok {
			moePrintf(stderr, "chain kick: no close registered for %s\n", chainWorkflow)
			return 1
		}
		if err := closeRunInProcess(root, chainWorkflow, reg.subject, reg.cleanup,
			projectID, md.ID, true /*skipEdit*/, false /*tailPulse*/, stdout, stderr); err != nil {
			moePrintf(stderr, "chain kick: close %s/%s: %v\n", projectID, md.ID, err)
			return 1
		}
		moePrintf(stdout, "closed chain %s/%s — riding the chain\n", projectID, md.ID)
	}

	// rideChain=true is the `!!!` vocabulary: walk this run's chained
	// child to its ship, then its child, and so on. maybeRideChain is the
	// same seam the cascade's terminal stages use, so the summary lines,
	// interrupt handling, and per-run exit codes are shared.
	return maybeRideChain(md, true /*rideChain*/, stdout, stderr)
}

// liveChainParent reports the in-progress run md is chained under, if
// any. Only a live parent refuses a kick: it is the one that still has a
// head to kick instead. A terminal parent's edge is history — every
// other edge reader already filters those out — so it leaves md kickable
// as its own head. Fan-in is allowed, so the lowest key wins for a
// deterministic message; the operator sees a head either way.
//
// The index error is returned rather than swallowed: this is a refusal
// guard, and failing open would ride a chain from the middle.
func liveChainParent(root string, md *run.Metadata) (string, bool, error) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return "", false, err
	}
	key := md.Project + "/" + md.ID
	var best string
	for parent, child := range idx.ChainedChild {
		if child != key {
			continue
		}
		pp, pr, err := splitProjectRun(parent)
		if err != nil {
			continue
		}
		pmd, err := run.Load(root, pp, pr)
		if err != nil || pmd.Status != run.StatusInProgress {
			continue
		}
		if best == "" || parent < best {
			best = parent
		}
	}
	return best, best != "", nil
}

// appendChainEntries writes one line per newly chained run under the
// chain canvas's `## Chained` heading and commits it together with the
// chain edges the spawn established. One commit, not one per run: the
// batch is one event, and BuildJournalIndex's grep picks up
// MoE-Chained-To alongside the MoE-Run trailer on the same commit.
//
// Caller holds the repolock.
func appendChainEntries(root, projectID, chainID string, lines, edges []string) error {
	canvasRel := run.ContentPath(projectID, chainID, chainDoc)
	body, err := os.ReadFile(filepath.Join(root, canvasRel))
	if err != nil {
		return fmt.Errorf("read chain canvas: %w", err)
	}
	updated := strings.TrimRight(string(body), "\n") + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, canvasRel), []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write chain canvas: %w", err)
	}

	msg := fmt.Sprintf("chain: append %d run(s) to %s/%s\n\n", len(lines), projectID, chainID) +
		trailers.Block{
			Run:       chainID,
			Project:   projectID,
			Workflow:  chainWorkflow,
			Document:  chainDoc,
			ChainedTo: edges,
		}.String()
	return run.StageAndCommit(root, msg, run.DocDir(projectID, chainID, chainDoc))
}

// stampChainBatch is the durable half of a spawn batch: it mints a
// *fresh* chain run, appends the batch's lines to its canvas, and stamps
// the edges that string the new runs under it. Runs under its own
// repolock acquisition and pushes the journal, since it is called after
// the survey's own lock windows have closed.
//
// The pulse never appends to an existing chain — not its own, not the
// operator's. A live-pen lookup plus a tail walk would let a batch land
// under a chain that is mid-ride (maybeRideChain rebuilds the journal
// index at every hop, so an edge stamped behind a still-live run would
// execute headlessly with no review) or dump machine proposals into a
// curated topic chain. Minting fresh costs the accumulating pen — one
// thing to review across pulses — and buys back both. Batches that pile
// up are visible chain units on the dash, and `moe chain edit` merges
// them by hand when the operator wants one.
//
// A batch of one gets no head at all: kick handles a chain of one, so a
// single parked fix needs no placeholder. Its "why" already rides on the
// run's seeded design canvas.
func stampChainBatch(root, projectID, pulseSlug string, spawned []spawnedRun, stdout, stderr io.Writer) error {
	if len(spawned) < 2 {
		return nil
	}
	chainMD, err := mintChainRun(root, projectID, chainWorkflow, projectID+"/"+pulseSlug, stdout, stderr)
	if err != nil {
		return fmt.Errorf("mint chain run: %w", err)
	}
	moePrintf(stderr, "pulse: opened chain %s/%s\n", projectID, chainMD.ID)

	chainKey := projectID + "/" + chainMD.ID
	tail := chainKey
	var lines, edges []string
	for _, s := range spawned {
		childKey := projectID + "/" + s.runID
		edges = append(edges, tail+" "+childKey)
		tail = childKey
		lines = append(lines, fmt.Sprintf("- `%s` — %s (proposed by %s): %s", s.runID, s.title, s.pulseSlug, s.why))
	}

	return sync.WithJournalPush(root, repolock.Options{
		Purpose: "chain-append",
		Run:     chainKey,
	}, stdout, stderr, func() error {
		return appendChainEntries(root, projectID, chainMD.ID, lines, edges)
	})
}

// mintChainRun opens a chain placeholder. IDBase rather than ID so a
// slug that has been used before (an operator re-using a topic name, the
// pulse minting its second batch of the day) gets a fresh dated slug
// rather than colliding with history. spawnedBy is the qualified spawner
// for a machine mint and empty for `moe chain new`, so a pulse's whole
// batch — head included — reads as the survey's lineage.
func mintChainRun(root, projectID, base, spawnedBy string, stdout, stderr io.Writer) (*run.Metadata, error) {
	opts := run.Options{
		IDBase:   base,
		Workflow: chainWorkflow,
		SeedDocs: map[string]string{chainDoc: chainCanvasSkeleton},
	}
	if spawnedBy != "" {
		opts.SpawnedBy = spawnedBy
		opts.Trailers = trailers.Block{SpawnedBy: spawnedBy}
	}
	return runopen.Open(root, projectID, opts, stdout, stderr)
}
