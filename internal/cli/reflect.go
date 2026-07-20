package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/wiki"
)

// reflectCommand is the user-facing `moe twin reflect <project>`
// entry. It mints a fresh `reflect-<timestamp>` run whose six
// stages (vision → architecture → patterns → operations →
// glossary → finalize) walk the closed-schema twin, then dispatches
// the first stage interactively. The chain prompt drives the
// remainder of the ladder; the cascade vocabulary (`!<stage>` /
// `!!` / `!!!`) is available at every stage gate.
//
// Per-stage commits don't bump the checkpoint; finalize does. That
// keeps `EventsSinceCheckpoint` stable across the pass — every stage
// reads the same events list — and folds log.md / checkpoint.json
// into the same per-turn commit as finalize's inline cleanups.
//
// Refuses with a redirect when:
//   - the operator has touched managed docs outside the changelog
//     (revert the edit, then land the change through a reflect
//     pass), or
//   - an in-progress twin run already exists for this project (resume
//     it via `moe twin <stage> <project> <run>` or close it before
//     starting a new pass).
func reflectCommand(workflow string, builder func(root, projectID string) (*wiki.Config, error)) *Command {
	return &Command{
		Name:    "reflect",
		Summary: "mint a twin reflect run and walk the six-stage ladder",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runReflectSession(workflow, builder, args, stdout, stderr)
		},
	}
}

func runReflectSession(workflow string, builder func(root, projectID string) (*wiki.Config, error), args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" reflect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agentOverride := fs.String("agent", "", "agent backend for this run (claude/codex). Explicit values persist to run.json; omitted values resolve at stage time via the model stylesheet, then $MOE_AGENT, then claude")
	park := fs.Bool("park", false, "open the run and stop: print the next-stage hint instead of prompting to run it")
	// The consent ladder, same trio the stage verbs and `new` carry
	// (`!!` / `!!!` / `!!!!`). `--ship` seals the pass headless; `--chain`
	// and `--dynamic` are meaningful even on a fresh unchained reflect —
	// `--chain`'s ride is usually a no-op there, but `--dynamic` licenses
	// the machine to act on what the pass surfaces (tail pulses grooming
	// onto the ridden tail). Mutually exclusive with each other and --park.
	ship := fs.Bool("ship", false, "open the run and cascade every stage headless to the seal (the flag twin of `!!` at the chain prompt; mutually exclusive with --park/--chain/--dynamic)")
	chain := fs.Bool("chain", false, "open the run, seal the pass, and ride the chain it heads (the flag twin of `!!!`; mutually exclusive with --park/--ship/--dynamic)")
	dynamic := fs.Bool("dynamic", false, "open the run, seal the pass, ride the chain, and license the machine to extend that ride (the flag twin of `!!!!`; mutually exclusive with --park/--ship/--chain)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s reflect [--agent <name>] [--park|--ship|--chain|--dynamic] <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Mints a fresh reflect-<timestamp> run for the project's twin and")
		moePrintln(stderr, "dispatches the first stage of the six-stage ladder. Each managed doc")
		moePrintln(stderr, "(vision, architecture, patterns, operations, glossary) gets its")
		moePrintln(stderr, "own per-stage canvas; finalize seals the pass — inline hygiene cleanup,")
		moePrintln(stderr, "history-summary fold, checkpoint bump. The engine refuses to seal with")
		moePrintln(stderr, "leftover findings; per-stage commits don't bump the checkpoint.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	// One bang answer out of the ladder, or "" for no cascade tail. ok is
	// false only when more than one rung was typed — they're a ladder,
	// not a set.
	cascade, ok := cascadeAnswerFromFlags(false /*once*/, "" /*to*/, *ship, *chain, *dynamic)
	if !ok {
		moePrintf(stderr, "%s reflect: --ship, --chain and --dynamic are one ladder — pick one\n", workflow)
		return 2
	}
	if code := preflightMintTail(workflow+" reflect", workflow, *park, cascade, stderr); code != 0 {
		return code
	}
	if *agentOverride != "" {
		if _, err := agent.Get(*agentOverride); err != nil {
			moePrintf(stderr, "%v\n", err)
			return 2
		}
	}
	projectID := fs.Arg(0)

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	canonical, err := builder(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: %v\n", err)
		return 1
	}

	// Guard-and-mint core, shared with pulse's auto-spawn. spawnedBy=""
	// — the operator ran the verb, so the run threads no parent edge.
	md, err := mintReflectRun(root, projectID, "" /*spawnedBy*/, *agentOverride, canonical, stdout, stderr)
	if err != nil {
		var refusal *reflectRefusal
		if errors.As(err, &refusal) {
			moePrintln(stderr, refusal.redirect(workflow, projectID))
			return 1
		}
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened twin reflect %s/%s\n", md.Project, md.ID)

	// Hand off through the shared mint tail. --park prints the next-stage
	// hint and stops; a cascade rung cascades headless through the ladder
	// and seals the pass (`!!` from the first parked stage, vision — a twin
	// run auto-closes after finalize instead of pushing — with `!!!`/`!!!!`
	// riding whatever the sealed pass chains onto); neither offers the
	// chain prompt's fresh-run path (justFinished="" → Workflow.Next
	// returns the first parked stage, and the operator picks `Y` / `!` /
	// `!!` / `!!!` / `!!!!` there).
	return mintTail(root, md, *park, cascade, stdout, stderr)
}

// reflectRefusalKind classifies the operator-prerequisite refusals the
// reflect mint core can raise, so the two callers format their own
// messaging off one typed value.
type reflectRefusalKind int

const (
	// reflectRefusalUnrecorded: managed docs were edited outside a
	// reflect pass — the operator lands them through a pass (or reverts)
	// first.
	reflectRefusalUnrecorded reflectRefusalKind = iota
	// reflectRefusalInProgress: a twin pass is already open for the
	// project (including a prior auto-spawned reflect still parked).
	reflectRefusalInProgress
)

// reflectRefusal is a guard refusal from mintReflectRun: an operator
// prerequisite, not an I/O failure. The verb prints redirect() and exits
// 1; pulse's auto-spawn warns (or stays silent) and skips. It carries
// the data each message needs — the detection result for the unrecorded
// case, the in-progress run's slug for the other.
type reflectRefusal struct {
	kind reflectRefusalKind
	det  wiki.DetectionResult // set for reflectRefusalUnrecorded
	slug string               // set for reflectRefusalInProgress
}

func (r *reflectRefusal) Error() string {
	switch r.kind {
	case reflectRefusalInProgress:
		return fmt.Sprintf("twin reflect: a pass is already in progress (%s)", r.slug)
	default:
		return fmt.Sprintf("twin reflect: unrecorded edits to %s", strings.Join(r.det.UnrecordedDocs, ", "))
	}
}

// redirect renders the verb's operator-facing one-liner for this
// refusal — the exact messages runReflectSession printed before the
// core was extracted.
func (r *reflectRefusal) redirect(workflow, projectID string) string {
	if r.kind == reflectRefusalInProgress {
		return fmt.Sprintf(
			"twin reflect: a pass is already in progress (%s/%s) — resume it with `moe twin <stage> %s/%s` or close it before starting another",
			projectID, r.slug, projectID, r.slug)
	}
	return unrecordedEditsRedirect(workflow, r.det)
}

// mintReflectRun runs the reflect preconditions and mints a parked twin
// reflect run for the project. It is the guard-and-mint core shared by
// the interactive `moe twin reflect` verb and pulse's auto-spawn. On a
// guard refusal (managed docs edited out of band, or a twin pass already
// in progress) it returns a *reflectRefusal the caller surfaces its own
// way; other non-nil errors are config or mint failures. spawnedBy
// stamps the MoE-Spawned-By edge and trailer ("" for the operator-run
// verb, which threads no parent); agentOverride persists the run's
// backend ("" on the pulse path, which resolves at stage time).
func mintReflectRun(root, projectID, spawnedBy, agentOverride string, canonical *wiki.Config, stdout, stderr io.Writer) (*run.Metadata, error) {
	if canonical == nil {
		return nil, fmt.Errorf("wiki: builder returned nil config; reflect requires a registered wiki")
	}
	if canonical.Mode != wiki.Closed {
		return nil, fmt.Errorf("wiki: reflect is closed-schema only (%s is %s)", canonical.Name, canonical.Mode)
	}

	// One pass at a time: two concurrent reflects would each see the same
	// kickoff context (events, findings, feedback) but write divergent
	// stage commits, and the `EventsSinceCheckpoint` filter has no way to
	// distinguish them. An already-parked auto-spawned reflect is
	// in_progress, so this is also what pulse maps its nominations onto.
	//
	// Checked *before* the unrecorded-edits guard on purpose: with a pass
	// already open, that pass is the answer whether or not the docs also
	// carry out-of-band edits — pulse maps onto it, and the verb's "resume
	// it" redirect is the more actionable of the two messages anyway (the
	// open pass is where those edits get landed).
	if existing, err := findInProgressTwinRun(root, projectID); err != nil {
		return nil, err
	} else if existing != "" {
		return nil, &reflectRefusal{kind: reflectRefusalInProgress, slug: existing}
	}

	// Guardrail: managed docs touched outside a reflect pass need the
	// operator to land them through a pass first (revert clears it). The
	// detection error is ignored — a failed scan never blocks the mint,
	// mirroring the pre-extraction verb.
	if det, err := wiki.DetectUnrecordedEdits(*canonical); err == nil && len(det.UnrecordedDocs) > 0 {
		return nil, &reflectRefusal{kind: reflectRefusalUnrecorded, det: det}
	}

	// Mint the run. workflow="twin"; the id-base "reflect" routes the
	// slug through nextFreeDatedID, producing `reflect-YYYY-MM-DD` (or
	// `reflect-YYYY-MM-DD-2` on same-day collision). Slug is the
	// operator-facing handle. SpawnedBy is paired across metadata and
	// trailer exactly as `moe sdlc reopen` pairs ReopenOf.
	opts := run.Options{
		IDBase:    "reflect",
		Workflow:  "twin",
		Agent:     agentOverride,
		SpawnedBy: spawnedBy,
		Trailers:  trailers.Block{SpawnedBy: spawnedBy, Consent: spawnConsent(spawnedBy)},
	}
	var md *run.Metadata
	err := sync.WithJournalPush(root, repolock.Options{
		Purpose: "run-new",
		Run:     projectID,
	}, stdout, stderr, func() error {
		m, err := run.New(root, projectID, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return md, nil
}

// findInProgressTwinRun returns the slug of an in-progress twin run
// for projectID, or "" if none. Scans the project's runs dir for
// run.json files keyed to workflow=twin / status=in_progress. Errors
// only on I/O — a project with no runs dir is "" with nil error.
func findInProgressTwinRun(root, projectID string) (string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return "", fmt.Errorf("scan runs: %w", err)
	}
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		if md.Workflow != "twin" {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		return md.ID, nil
	}
	return "", nil
}

// reflectPostFlightGate re-runs the structural scan against the
// worktree-rewritten wiki right before the engine seals the pass.
// Non-empty findings print to stderr and return an error so
// runWikiSession skips FinalizeIngest and the per-turn commit — no
// log entry, no checkpoint bump, no commit. The agent's content
// edits are left in the closed worktree and dropped; the operator
// re-runs `moe twin reflect` to try again. Strict by design: the
// gate is what makes the closing-discipline real (same shape as a
// pre-push hook).
func reflectPostFlightGate(worktreeWiki *wiki.Config, stderr io.Writer) error {
	if worktreeWiki == nil {
		return nil
	}
	findings, err := wiki.Scan(*worktreeWiki)
	if err != nil {
		return fmt.Errorf("reflect: post-flight scan: %w", err)
	}
	if !findings.HasBlocking() {
		return nil
	}
	moePrintln(stderr, "reflect: leftover hygiene findings — refusing to seal the pass.")
	moePrintln(stderr, "         The session is closed; re-run `moe twin reflect` and walk the agent through the remaining items inline.")
	moePrintln(stderr, "")
	moePrintln(stderr, wiki.RenderFindings(findings))
	return fmt.Errorf("reflect: post-flight scan found %d unresolved findings", findingsCount(findings))
}

// findingsCount sums the post-flight findings across all categories
// for the gate's exit message. The breakdown lives in the rendered
// block printed above; this is just the rolled-up number for the
// terminal "found N unresolved findings" line.
func findingsCount(f wiki.Findings) int {
	return len(f.Orphans) + len(f.MissingFromIndex) + len(f.BrokenLinks) +
		len(f.EmptyDocs) + len(f.MissingManagedDocs) + len(f.GlossaryOrphans) +
		len(f.DanglingXrefs)
}

// unrecordedEditsRedirect formats the one-line redirect printed when
// reflect refuses to run because managed docs have been edited
// outside a reflect pass. Names the docs and tells the operator to
// revert: a committed revert back to checkpoint state is a net no-op
// (docUnchangedSinceSHA diffs checkpoint..HEAD, so HEAD's tree must
// match — an uncommitted checkout doesn't clear it) and lifts the
// refusal; the change itself lands through a reflect pass.
func unrecordedEditsRedirect(workflow string, det wiki.DetectionResult) string {
	docs := strings.Join(det.UnrecordedDocs, ", ")
	since := "the last log entry"
	if !det.Since.IsZero() {
		since = det.Since.Format("2006-01-02")
	}
	return fmt.Sprintf("unrecorded edits to %s since %s — revert them, then land the change through `moe %s reflect`",
		docs, since, workflow)
}

// twinFeedbackEntry is one note left under projects/<p>/runs/<slug>/
// feedback/twin.md by a workflow agent, surfaced into the
// next reflect's kickoff. Provenance (runID) lets the agent trace a
// note back to where it came from; `when` is the git-time of the
// most recent commit touching the file, used to filter against the
// reflect checkpoint.
type twinFeedbackEntry struct {
	runID string
	body  string
	when  time.Time
}

// loadTwinFeedback walks projects/<projectID>/runs/*/feedback/twin.md
// and returns the entries whose latest touching commit post-dates the
// reflect checkpoint's LastIngestAt. Git-time (not filesystem mtime) is
// the signal, same as closedRunsSince — a freshly-edited but
// uncommitted feedback file is not yet a fact in the journal. Sorted
// freshest-first so the agent reads the most recent notes first.
//
// Closed-schema only; the caller hands in the canonical wiki cfg whose
// checkpoint anchors the "since when" boundary. Missing checkpoint /
// empty LastIngestAt means "first reflect" — every present feedback
// file lands.
//
// One exception to the threshold: the run named by the checkpoint's
// LastIngestRun. A reflect pass seals the checkpoint and writes its own
// feedback/twin.md in the same stage-exit commit, so that note's
// git-time never post-dates the threshold it created — yet it plainly
// wasn't ingested by the pass that filed it. It bypasses the filter so
// it reaches the next pass, then ages out once that pass seals and
// LastIngestRun moves on.
func loadTwinFeedback(root, projectID string, cfg wiki.Config) ([]twinFeedbackEntry, error) {
	cp, hasCheckpoint, err := wiki.ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return nil, fmt.Errorf("wiki: read checkpoint: %w", err)
	}
	var threshold time.Time
	if hasCheckpoint && cp.LastIngestAt != "" {
		if t, err := time.Parse(time.RFC3339, cp.LastIngestAt); err == nil {
			threshold = t
		}
	}
	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	var out []twinFeedbackEntry
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		rel := run.FeedbackPath(md.Project, md.ID, "twin")
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read feedback %s/%s: %w", md.Project, md.ID, err)
		}
		when, err := run.LastFileActivity(root, rel)
		if err != nil {
			return nil, fmt.Errorf("git time %s/%s: %w", md.Project, md.ID, err)
		}
		if when.IsZero() {
			// Present on disk but never committed — invisible to the
			// journal, so invisible to reflect. The next stage commit
			// will fold it in.
			continue
		}
		sealedOwn := hasCheckpoint && cp.LastIngestRun != "" && md.ID == cp.LastIngestRun
		if !threshold.IsZero() && !when.After(threshold) && !sealedOwn {
			continue
		}
		out = append(out, twinFeedbackEntry{
			runID: md.ID,
			body:  string(body),
			when:  when,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	return out, nil
}
