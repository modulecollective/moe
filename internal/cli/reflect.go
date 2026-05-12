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

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
	"github.com/modulecollective/moe/internal/wiki"
)

// reflectCommand builds the `reflect` facade for a workflow. Reflect
// is closed-schema only and out-of-band relative to the run ladder:
// no run.json, no stage progression, no merge gates. It does mint a
// `reflect-<timestamp>` run directory whose only artifact is a single
// end-of-pass summary at `documents/reflect/content.md` — the durable
// "what changed and why" the session-close gate refuses to seal
// without. Roadmap synthesis, doc-by-doc walk against recent events,
// and structural hygiene cleanup all happen in the same session, so
// `last_ingest_at` keeps its single meaning ("events ingested through
// here") with no partial-pass commands left to skew the checkpoint.
func reflectCommand(workflow string, builder func(root, projectID string) (*wiki.Config, error)) *Command {
	return &Command{
		Name:    "reflect",
		Summary: "open a Claude Code reflect session on the project's twin",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runReflectSession(workflow, builder, args, stdout, stderr)
		},
	}
}

func runReflectSession(workflow string, builder func(root, projectID string) (*wiki.Config, error), args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" reflect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s reflect <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code reflect session on the project's twin.")
		moePrintln(stderr, "Out-of-band relative to runs: no run.json, no stage, no merge gates. The")
		moePrintln(stderr, "session writes a one-shot end-of-pass summary to")
		moePrintln(stderr, "projects/<p>/runs/reflect-<timestamp>/documents/reflect/content.md, staged")
		moePrintln(stderr, "in the same `work: reflect pass …` commit as the twin edits. Surfaces")
		moePrintln(stderr, "under the dash's TWIN rail (`recent: …`), not ACTIVE/BACKLOG/COMPLETED.")
		moePrintln(stderr, "Walks each managed doc against project commits and closed runs since the")
		moePrintln(stderr, "last reflect, folds the open idea backlog into the roadmap, and clears")
		moePrintln(stderr, "structural hygiene findings. The engine re-scans at session-end and")
		moePrintln(stderr, "refuses to seal a reflect with leftover findings.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
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

	runSlug := "reflect-" + time.Now().Local().Format("2006-01-02-150405")
	docID := "reflect"

	sessionUUID, err := run.NewSessionID()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	events, err := wiki.EventsSinceCheckpoint(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: events: %v\n", err)
		return 1
	}
	historySummary, err := wiki.ReadHistorySummary(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: history summary: %v\n", err)
		return 1
	}
	ideas, err := loadIdeaBacklog(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: ideas: %v\n", err)
		return 1
	}
	feedback, err := loadTwinFeedback(root, projectID, *canonical)
	if err != nil {
		moePrintf(stderr, "wiki: feedback: %v\n", err)
		return 1
	}
	// Pre-flight hygiene scan against the canonical wiki so the
	// kickoff carries the same findings the post-flight gate will
	// later check against. EnsureManagedDocs hasn't run yet — fresh
	// twins surface as MissingManagedDocs here, which is the right
	// signal for the agent.
	findings, err := wiki.Scan(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki scan: %v\n", err)
		return 1
	}

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runSlug,
		DocID:       docID,
		LockPurpose: "reflect",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			return builder(canonicalRoot, projectID)
		},
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			return wikiTurnSpec{
				Metadata:         nil,
				DocID:            docID,
				ClonePath:        "",
				SessionUUID:      sessionUUID,
				NewSession:       true,
				InitialPrompt:    reflectKickoff(*canonical, historySummary, events, ideas, feedback, findings, run.ContentPath(projectID, runSlug, docID)),
				FinalizeRunID:    runSlug,
				FinalizeRunTitle: "Twin reflect pass",
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildReflectSystemPrompt(worktreeWiki)
				},
				PreFinalizeGate: func(workRoot string, worktreeWiki *wiki.Config) error {
					return reflectPostFlightGate(worktreeWiki, stderr)
				},
				CommitStager: func(workRoot, wikiRel string) error {
					return commitReflectTurn(workRoot, workflow, projectID, runSlug, wikiRel)
				},
			}, nil
		},
	}

	return runWikiSession(root, in, stdout, stderr)
}

func buildReflectSystemPrompt(worktreeWiki *wiki.Config) (string, error) {
	if worktreeWiki == nil {
		return "", fmt.Errorf("reflect: missing wiki config")
	}
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if ref := wiki.TwinReferenceSection(*worktreeWiki); ref != "" {
		sections = append(sections, ref)
	}
	body, err := wiki.ReflectPromptSection(*worktreeWiki)
	if err != nil {
		return "", err
	}
	sections = append(sections, body)
	return strings.Join(sections, "\n---\n\n"), nil
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
		len(f.EmptyDocs) + len(f.MissingManagedDocs)
}

// reflectKickoff is the auto-sent first user message. It frames the
// pass and lays out the session in the order the agent should walk
// it: hygiene findings (structural cleanup informs synthesis), the
// workflow feedback dropped by non-twin runs since the last reflect,
// the per-doc reflect prompts, the open idea backlog (drives roadmap
// re-prioritisation), the rolling history summary, and the verbatim
// "events since last reflect" tail. Empty inputs collapse to a
// one-line placeholder — a quiet section is fine.
func reflectKickoff(cfg wiki.Config, historySummary, events string, ideas []ideaSummary, feedback []twinFeedbackEntry, findings wiki.Findings, canvasRel string) string {
	var b strings.Builder
	b.WriteString("The operator just opened a twin reflect session. " +
		"Walk each managed doc against recent project activity, fold the open " +
		"idea backlog into the roadmap, and clear any structural hygiene " +
		"findings. Vision is drift-only — flag gaps, don't rewrite.\n\n")

	if !findings.IsEmpty() {
		b.WriteString("## Hygiene findings\n\n")
		b.WriteString("Walk these with the operator before the doc-by-doc pass — " +
			"structural issues inform the synthesis. The engine re-scans at " +
			"session-end and refuses to seal a reflect with leftover findings.\n\n")
		b.WriteString(wiki.RenderFindings(findings))
	}

	b.WriteString("## Workflow feedback\n\n")
	if len(feedback) == 0 {
		b.WriteString("(no workflow feedback since the last reflect)\n\n")
	} else {
		b.WriteString("Notes that workflow agents left for this reflect pass. Each " +
			"entry is from a non-twin run; treat as input, not direction — fold " +
			"what's real into the relevant managed doc, set aside what isn't.\n\n")
		for _, fb := range feedback {
			title := fb.runTitle
			if title == "" {
				title = fb.runID
			}
			fmt.Fprintf(&b, "### %s — %s (%s)\n\n", fb.runID, title, fb.when.Format("2006-01-02"))
			body := strings.TrimSpace(fb.body)
			if body == "" {
				b.WriteString("(empty feedback file)\n\n")
				continue
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("## Per-doc reflect prompts\n\n")
	for _, d := range cfg.ManagedDocs {
		fmt.Fprintf(&b, "### %s\n\n", d.Filename)
		body := strings.TrimSpace(d.ReflectPrompt)
		if body == "" {
			body = "Walk this doc against recent activity. Update where work has changed what the doc claims."
		}
		b.WriteString(body)
		b.WriteString("\n\n")
	}

	b.WriteString("## Idea backlog\n\n")
	if len(ideas) == 0 {
		b.WriteString("(no open ideas captured for this project)\n\n")
	} else {
		b.WriteString("Each entry below is an open idea run (`moe idea list`). " +
			"Decide which belong on the roadmap and at which horizon; the rest " +
			"stay on the idea shelf or move to Parked with a reason.\n\n")
		for _, idea := range ideas {
			fmt.Fprintf(&b, "### %s — %s\n\n", idea.slug, idea.title)
			body := strings.TrimSpace(idea.body)
			if body == "" {
				b.WriteString("(empty canvas)\n\n")
				continue
			}
			b.WriteString(body)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("## History summary\n\n")
	if historySummary != "" {
		b.WriteString(historySummary)
		b.WriteString("\n\n")
	} else {
		b.WriteString("(no rolling summary yet — seed `history-summary.md` from the events " +
			"below at the end of this pass)\n\n")
	}

	if events != "" {
		b.WriteString(events)
	} else {
		b.WriteString("## Events since last reflect\n\n")
		b.WriteString("(no project commits or closed runs since the last checkpoint)\n\n")
	}

	b.WriteString("Acknowledge in one or two sentences which docs look most likely to need " +
		"updates and how you'd walk through them with the operator — name the hygiene " +
		"findings you'd clear and the idea-backlog entries you'd promote. Then wait for " +
		"the operator's go-ahead.\n\n")
	b.WriteString("Before you finish the pass, propose an updated `history-summary.md` that " +
		"folds in the events you just walked. The summary is the twin's compressed memory " +
		"of everything before the next checkpoint — keep it prose, keep it slow-growing, " +
		"and don't drop signal that future reflects will need.\n\n")
	fmt.Fprintf(&b, "When the pass is sealed and you're ready to hand control back, write "+
		"your end-of-pass summary to `%s`. That summary is the durable per-pass artifact — "+
		"the same kind of \"what changed and why\" you'd write in a PR description. Keep it "+
		"terse; the twin diff itself is the detail. The session refuses to seal until that "+
		"file is non-empty.\n", canvasRel)
	return b.String()
}

func commitReflectTurn(workRoot, workflow, projectID, runSlug, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	paths := []string{wikiRel}
	// The agent is instructed (in reflectKickoff) to drop an end-of-pass
	// summary at canvasRel. Stage it alongside the twin edits so both
	// land in the same `work: reflect pass <runSlug>` commit and the
	// session-close gate sees a non-empty canvas at the branch tip.
	// `git add` errors on a missing path, so skip if the agent forgot —
	// the close-time gate is the strict check that will refuse to seal.
	canvasRel := run.ContentPath(projectID, runSlug, "reflect")
	if _, err := os.Stat(filepath.Join(workRoot, canvasRel)); err == nil {
		paths = append(paths, canvasRel)
	}
	if err := run.Stage(workRoot, paths...); err != nil {
		return err
	}
	if !run.HasStagedChanges(workRoot) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf("work: reflect pass %s\n\n", runSlug) +
		trailers.Block{
			Run:      runSlug,
			Project:  projectID,
			Workflow: workflow,
			Document: "reflect",
		}.String()
	return run.StageAndCommit(workRoot, msg, paths...)
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

// ideaSummary captures the minimum needed to render an open idea
// into the reflect kickoff: the slug, title, and verbatim canvas
// body. The agent uses this to decide which ideas belong on the
// roadmap (and at which horizon) versus which stay on the idea
// shelf.
type ideaSummary struct {
	slug  string
	title string
	body  string
}

// twinFeedbackEntry is one note left under projects/<p>/runs/<slug>/
// feedback/twin.md by a non-twin workflow agent, surfaced into the
// next reflect's kickoff. Provenance (runID, runTitle) lets the agent
// trace a note back to where it came from; `when` is the git-time of
// the most recent commit touching the file, used to filter against
// the reflect checkpoint.
type twinFeedbackEntry struct {
	runID    string
	runTitle string
	body     string
	when     time.Time
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
			runID:    md.ID,
			runTitle: md.Title,
			body:     string(body),
			when:     when,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.After(out[j].when) })
	return out, nil
}

// loadIdeaBacklog enumerates every open idea run for projectID and
// loads each canvas body. "Backlog" means StatusInProgress idea
// runs: closed/promoted ideas have already been spent. Sorted by
// slug for stable kickoff ordering across passes.
func loadIdeaBacklog(root, projectID string) ([]ideaSummary, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var out []ideaSummary
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		if md.Workflow != ideaWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, ideaDocID)))
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read idea %s/%s: %w", md.Project, md.ID, err)
		}
		out = append(out, ideaSummary{slug: md.ID, title: md.Title, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].slug < out[j].slug })
	return out, nil
}
