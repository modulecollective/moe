package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// A **chain run** is a placeholder head: a run whose only job is to be
// the stable handle for the batch chained behind it. `moe chain new`
// mints one by hand (a topic — `moe chain new moe/perf-cleanups`); the
// pulse mints one when a groom group names a `head`. Either way the
// head holds still while its children ship, which is what a bare chain
// of ordinary runs cannot do — the handle would move every time the
// head shipped.
//
// A head is a convenience, not the primitive. Grooming's primitive is
// "chain after an existing item" (pulse_groom.go), and a bare chain of
// ordinary runs is a perfectly good thread — it just has a moving
// handle. Mint a head when naming the group helps the dash tell the
// story; skip it everywhere else.
//
// The workflow registers **no stages**. That is the whole trick: a run
// with no ladder is trivially done the moment it exists, so `moe chain
// kick` closes it and rides on without a special case, and nothing can
// ever open an agent session on it. Its one document (`chain`) is
// registered via RegisterDoc so the canvas still resolves for serve and
// cat — it is the operator's **purpose note**: why this batch exists,
// what ties it together. Membership is not written there. The edges are
// the truth, so the head's run page renders the batch live from them;
// the one thing the journal cannot derive is the why, and that is the
// note's whole job.
//
// The invariant the whole surface preserves: **chaining under a parked
// chain is inert.** Grooming reshapes membership; it never starts
// anything. Motion roots in an operator kick — static (`!!!`, the
// machine can't grow it) or dynamic (`!!!!`, it can) — or in a
// confident pulse downstream of a fourth bang the operator typed
// (pulse_kick.go).
const (
	// chainWorkflow is the workflow name written to run.json. Aliased
	// from dash so the string lives in one place — dash also needs it
	// to spell a parked head's hint (see dash.ChainWorkflow).
	chainWorkflow = dash.ChainWorkflow
	// chainDoc is the stageless document id. The chain canvas lives at
	// documents/chain/content.md.
	chainDoc = "chain"
)

// chainCanvasSkeleton is what a freshly minted chain run opens with: a
// heading and a prompt, and nothing that pretends to be content. The
// prompt is an HTML comment so an untouched note renders as a bare
// heading rather than placeholder boilerplate the operator has to
// recognise as unwritten.
//
// Nothing here lists members. `moe chain edit` moves runs under a head
// without touching this file, and the pulse no longer appends either —
// a membership list in the canvas is a second copy of journal state
// that every edge writer would have to maintain, and it went stale the
// first time anything was reordered, shipped, or pruned.
const chainCanvasSkeleton = `# Chain

<!-- Why does this batch exist — what ties these runs together?

     Write it here (` + "`moe chain note <project>/<slug>`" + `, or just edit this
     file). Membership and status render live on the head's run page
     from the chain edges; don't list runs here. -->
`

// chainSeed builds the purpose-note seed for a fresh head. A
// pulse-minted head gets one provenance line naming the survey that
// spawned it — the only fact about a machine batch that isn't already
// on the edges or on each child's own seeded design canvas.
func chainSeed(spawnedBy string) string {
	if spawnedBy == "" {
		return chainCanvasSkeleton
	}
	return chainCanvasSkeleton + "\nSpawned by `" + spawnedBy + "`.\n"
}

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
	seed := fs.Bool("seed", false, "pop $EDITOR on the purpose note and mint the head with the edited body")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain new [--seed] <project>/<slug>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Mints a chain run: a placeholder head that holds still while the")
		moePrintln(stderr, "runs chained behind it ship. Name it for the topic it collects")
		moePrintln(stderr, "(moe chain new moe/perf-cleanups); the slug is dated on collision.")
		moePrintln(stderr, "A project can hold several live chains at once.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Returns immediately with an empty purpose note. --seed pops $EDITOR")
		moePrintln(stderr, "on that note first; `moe chain note` writes it later either way.")
		fs.PrintDefaults()
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

	note := ""
	if *seed {
		if code := seedChainNote(&note, stdout, stderr); code != 0 {
			return code
		}
	}

	md, err := mintChainRun(root, projectID, slug, "" /*spawnedBy*/, note, stdout, stderr)
	if err != nil {
		moePrintf(stderr, "chain new: %v\n", err)
		return 1
	}
	key := projectID + "/" + md.ID
	moePrintf(stdout, "opened chain %s\n", key)
	moePrintln(stdout, "next:")
	moePrintf(stdout, "  moe chain note %s    # why this batch exists\n", key)
	moePrintln(stdout, "  moe chain edit    # move runs under it")
	moePrintf(stdout, "  moe chain kick %s    # ride the chain headlessly\n", key)
	return 0
}

// seedChainNote is `moe chain new --seed`: the editor pop at mint. The
// workflow-generic --seed (new.go) seeds a workflow's *first stage*; a
// chain head has no stages, so chain's version seeds the purpose note
// instead — the only doc it has.
//
// Same shape as seedFirstStage minus the slug pre-flight: chain new
// mints with IDBase, so a name that's been used before is dated rather
// than refused, and there is no collision to fail fast on. The
// unchanged-stub abort carries over — an operator who asked for the
// editor and typed nothing meant to back out, and plain `moe chain new`
// is right there for a head with no note.
func seedChainNote(note *string, stdout, stderr io.Writer) int {
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "chain new: set $EDITOR or $VISUAL — --seed needs an editor")
		return 1
	}
	body, tmpPath, code := captureEditorBody("moe-chain-seed-", chainCanvasSkeleton, stdout, stderr)
	if code != 0 {
		if tmpPath != "" {
			moePrintf(stderr, "chain new: your edited note is preserved at %s\n", tmpPath)
		}
		return code
	}
	defer os.RemoveAll(filepath.Dir(tmpPath))
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == strings.TrimSpace(chainCanvasSkeleton) {
		moePrintln(stderr, "chain new: aborting: note unchanged")
		return 1
	}
	*note = body
	return 0
}

func runChainKick(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain kick", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dynamic := fs.Bool("dynamic", false, "ride dynamically: tail pulses may groom onto this chain's tail and kick threads they root (= !!!!)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain kick [--dynamic] <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Rides the named chain from its head, headlessly: the head cascades")
		moePrintln(stderr, "to its ship, then each chained run is walked design -> ... -> push")
		moePrintln(stderr, "in the order the chain records. A chain run head has no stages, so")
		moePrintln(stderr, "it just closes and the ride carries on into its children. Reorder or")
		moePrintln(stderr, "prune with `moe chain edit` first.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Default is the static ride: the machine cannot grow it. What is")
		moePrintln(stderr, "chained now is what runs. --dynamic lifts that — tail pulses may")
		moePrintln(stderr, "append work onto the tail this ride has yet to reach, so the ride")
		moePrintln(stderr, "can outlive the batch you can see.")
		fs.PrintDefaults()
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
	mode := rideStatic
	if *dynamic {
		mode = rideDynamic
	}
	return chainKickRun(root, projectID, runID, mode, stdout, stderr)
}

// chainKickRun is the kick body, split from the verb's flag parsing so
// the pulse's own self-kick (item 6 of the grooming design) roots a ride
// through exactly this path rather than a sibling implementation. mode
// is the consent level the ride carries — static for a bare `moe chain
// kick`, dynamic for `--dynamic` and for every kick the pulse roots
// itself (a confident pulse rooting a bounded-only ride would defeat the
// point; an operator who wants bounded keeps `!!!`).
func chainKickRun(root, projectID, runID string, mode rideMode, stdout, stderr io.Writer) int {
	defer withRideMode(mode)()
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
// as its own head.
//
// The index error is returned rather than swallowed: this is a refusal
// guard, and failing open would ride a chain from the middle.
func liveChainParent(root string, md *run.Metadata) (string, bool, error) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return "", false, err
	}
	mds, err := run.Scan(root)
	if err != nil {
		return "", false, err
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, m := range mds {
		byKey[m.Project+"/"+m.ID] = m
	}
	parent := run.NewChainGraph(idx, byKey).LiveParentOf(md.Project + "/" + md.ID)
	return parent, parent != "", nil
}

// runChainNote edits a chain head's purpose note — the one thing the
// canvas carries, and the one thing the journal can't derive from the
// edges. Same shape as `moe idea edit`: $EDITOR on the file in place,
// then one trailered commit under the repolock with a journal push.
//
// A separate verb rather than a mode of `chain edit`: edit is the
// batch's *shape* (reorder, prune, unchain, merge chains) across every
// project at once, and there is no head in scope to attach prose to.
func runChainNote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chain note", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chain note <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens $EDITOR on the chain head's purpose note: why this batch")
		moePrintln(stderr, "exists, what ties it together. Membership is not written here —")
		moePrintln(stderr, "it renders live from the chain edges on the head's run page.")
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
		moePrintf(stderr, "chain note: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "chain note: %v\n", err)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		moePrintf(stderr, "chain note: %v\n", err)
		return 1
	}
	if md.Workflow != chainWorkflow {
		moePrintf(stderr, "chain note: %s/%s is a %s run — only chain heads carry a note\n", projectID, runID, md.Workflow)
		return 1
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "chain note: set $EDITOR or $VISUAL — chain note needs an editor")
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, runID, chainDoc))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "chain note: canvas missing: %v\n", err)
		return 1
	}
	if code := launchEditor(abs, stdout, stderr); code != 0 {
		return code
	}

	msg := fmt.Sprintf("work: update %s\n\n", chainDoc) +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: chainWorkflow,
			Document: chainDoc,
		}.String()
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "chain-note",
		Run:     projectID + "/" + runID,
	}, stdout, stderr, func() error {
		return run.StageAndCommit(root, msg, run.DocDir(projectID, runID, chainDoc))
	})
	switch {
	case errors.Is(err, run.ErrNothingToCommit):
		moePrintf(stdout, "chain note %s/%s unchanged\n", projectID, runID)
	case err != nil:
		moePrintf(stderr, "chain note: commit: %v\n", err)
		return 1
	default:
		moePrintf(stdout, "wrote chain note %s/%s\n", projectID, runID)
	}
	return 0
}

// mintChainRun opens a chain placeholder. IDBase rather than ID so a
// slug that has been used before (an operator re-using a topic name, the
// pulse minting its second batch of the day) gets a fresh dated slug
// rather than colliding with history. spawnedBy is the qualified spawner
// for a machine mint and empty for `moe chain new`, so a pulse's whole
// batch — head included — reads as the survey's lineage.
//
// note is the purpose-note body; empty takes the default skeleton (plus
// a provenance line when spawnedBy is set). Only `--seed` passes one.
func mintChainRun(root, projectID, base, spawnedBy, note string, stdout, stderr io.Writer) (*run.Metadata, error) {
	if note == "" {
		note = chainSeed(spawnedBy)
	}
	opts := run.Options{
		IDBase:   base,
		Workflow: chainWorkflow,
		SeedDocs: map[string]string{chainDoc: note},
	}
	if spawnedBy != "" {
		opts.SpawnedBy = spawnedBy
		opts.Trailers = trailers.Block{SpawnedBy: spawnedBy, Consent: spawnConsent(spawnedBy)}
	}
	return runopen.Open(root, projectID, opts, stdout, stderr)
}
