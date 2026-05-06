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
// is closed-schema only and out-of-band relative to the run ladder
// (no per-run canvas, no stage). It is the only twin-mutating pass
// over closed-schema content: roadmap synthesis, doc-by-doc walk
// against recent events, and structural hygiene cleanup all happen
// in the same session — `last_ingest_at` recovers its single meaning
// ("events ingested through here") with no partial-pass commands
// left to skew the checkpoint.
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
		moePrintln(stderr, "Out-of-band relative to runs: no stage, no canvas, no run.json — the")
		moePrintln(stderr, "session surfaces under the dash's TWIN rail (`recent: …`), not in")
		moePrintln(stderr, "ACTIVE/BACKLOG/COMPLETED.")
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
				InitialPrompt:    reflectKickoff(*canonical, historySummary, events, ideas, findings),
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
// per-doc reflect prompts, the open idea backlog (drives roadmap
// re-prioritisation), the rolling history summary, and the verbatim
// "events since last reflect" tail. Empty inputs collapse to a
// one-line placeholder — a quiet section is fine.
func reflectKickoff(cfg wiki.Config, historySummary, events string, ideas []ideaSummary, findings wiki.Findings) string {
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
		"and don't drop signal that future reflects will need.\n")
	return b.String()
}

func commitReflectTurn(workRoot, workflow, projectID, runSlug, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	if err := run.Stage(workRoot, wikiRel); err != nil {
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
	return run.StageAndCommit(workRoot, msg, wikiRel)
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
