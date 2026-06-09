package cli

import (
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
//     (run `moe twin claim` first to record the decided edit), or
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
	agentOverride := fs.String("agent", "", "agent backend for this run (claude/codex). Explicit values persist to run.json; omitted values resolve at stage time via $MOE_AGENT, then claude")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s reflect [--agent <name>] <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Mints a fresh reflect-<timestamp> run for the project's twin and")
		moePrintln(stderr, "dispatches the first stage of the six-stage ladder. Each managed doc")
		moePrintln(stderr, "(vision, architecture, patterns, operations, glossary) gets its")
		moePrintln(stderr, "own per-stage canvas; finalize seals the pass — inline hygiene cleanup,")
		moePrintln(stderr, "history-summary fold, checkpoint bump. The engine refuses to seal with")
		moePrintln(stderr, "leftover findings; per-stage commits don't bump the checkpoint.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
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
	if canonical == nil {
		moePrintf(stderr, "wiki: builder returned nil config; reflect requires a registered wiki\n")
		return 1
	}
	if canonical.Mode != wiki.Closed {
		moePrintf(stderr, "wiki: reflect is closed-schema only (%s is %s)\n", workflow, canonical.Mode)
		return 1
	}

	// Decided-edit guardrail: refuse with a redirect to claim if the
	// operator has touched managed docs outside the changelog.
	if det, err := wiki.DetectUnrecordedEdits(*canonical); err == nil && len(det.UnrecordedDocs) > 0 {
		moePrintf(stderr, "%s\n", unrecordedEditsRedirect(workflow, det))
		return 1
	}

	// Refuse if an in-progress twin run already exists. Two concurrent
	// reflects would each see the same kickoff context (events,
	// findings, feedback) but write divergent stage commits, and the
	// `EventsSinceCheckpoint` filter has no way to distinguish them.
	// One pass at a time.
	if existing, err := findInProgressTwinRun(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	} else if existing != "" {
		moePrintf(stderr,
			"twin reflect: a pass is already in progress (%s/%s) — resume it with `moe twin <stage> %s/%s` or close it before starting another\n",
			projectID, existing, projectID, existing)
		return 1
	}

	// Mint the run. workflow="twin"; the id-base "reflect" routes the
	// slug through nextFreeDatedID, producing `reflect-YYYY-MM-DD` (or
	// `reflect-YYYY-MM-DD-2` on same-day collision). Slug is the
	// operator-facing handle.
	opts := run.Options{
		IDBase:   "reflect",
		Workflow: "twin",
		Agent:    *agentOverride,
	}
	var md *run.Metadata
	err = sync.WithJournalPush(root, repolock.Options{
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
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened twin reflect %s/%s\n", md.Project, md.ID)

	// Hand off to the chain prompt's fresh-run path. justFinished="" so
	// promptNextStage falls back to Workflow.Next, which returns the
	// first parked stage (vision). The chain prompt offers `Y` to run
	// it; `!!` to cascade headless through the ladder and ship this run,
	// `!!!` to also ride the chain; `!` for headless dispatch of just
	// the next stage.
	return promptNextStage(root, md, "", stdout, stderr)
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
	if findings.IsEmpty() {
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
		len(f.EmptyDocs) + len(f.MissingManagedDocs) + len(f.GlossaryOrphans)
}

// unrecordedEditsRedirect formats the one-line redirect printed when
// reflect refuses to run because managed docs have been edited
// outside a reflect pass. Names the docs and points the operator at
// claim.
func unrecordedEditsRedirect(workflow string, det wiki.DetectionResult) string {
	docs := strings.Join(det.UnrecordedDocs, ", ")
	since := "the last log entry"
	if !det.Since.IsZero() {
		since = det.Since.Format("2006-01-02")
	}
	return fmt.Sprintf("unrecorded edits to %s since %s — run `moe %s claim <project>` first",
		docs, since, workflow)
}

// twinFeedbackEntry is one note left under projects/<p>/runs/<slug>/
// feedback/twin.md by a non-twin workflow agent, surfaced into the
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
		if !threshold.IsZero() && !when.After(threshold) {
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
