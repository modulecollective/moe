package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// lintCommand builds the `lint` facade for a workflow. Lint is
// out-of-band relative to the run ladder — there is no per-run
// canvas, no stage, no prerequisite chain — so it lives alongside
// `new` / `close` as a workflow facade rather than a stage. The
// builder is the wiki-config factory the workflow uses for its
// ingest stage; lint reuses it so the lint and ingest sessions agree
// on the wiki's identity and on-disk shape.
func lintCommand(workflow string, builder func(root, projectID string) (*wiki.Config, error)) *Command {
	return &Command{
		Name:    "lint",
		Summary: "open a Claude Code lint session on the project's wiki",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runLintSession(workflow, builder, args, stdout, stderr)
		},
	}
}

func runLintSession(workflow string, builder func(root, projectID string) (*wiki.Config, error), args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe workflow %s lint <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code lint session on the project's wiki.")
		moePrintln(stderr, "Out-of-band relative to runs: no stage, no canvas, no run.json.")
		moePrintln(stderr, "Findings (orphaned docs, broken cross-links, empty docs) are pre-scanned")
		moePrintln(stderr, "and seeded into the kickoff prompt; the agent walks them with the operator")
		moePrintln(stderr, "and applies fixes inline. The wiki diff is the artifact, recorded in")
		moePrintln(stderr, "log.md under a synthetic lint-YYYY-MM-DD-HHMMSS run id.")
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

	// Synthetic run id keys the session worktree branch and lands in
	// the log.md entry header so lint passes are distinguishable from
	// ingests at a glance. HHMMSS guarantees uniqueness even when the
	// operator runs lint twice on the same day.
	runSlug := "lint-" + time.Now().UTC().Format("2006-01-02-150405")
	docID := "lint"

	sessionUUID, err := run.NewSessionID()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	// Findings are produced under the canonical bureaucracy root
	// before the session worktree exists. They get spliced into the
	// kickoff prompt verbatim — the worktree path differs but the
	// findings (filenames under the wiki dir) remain valid.
	canonical, err := builder(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: %v\n", err)
		return 1
	}
	if canonical == nil {
		moePrintf(stderr, "wiki: builder returned nil config; lint requires a registered wiki\n")
		return 1
	}
	// Closed-schema guardrail: unrecorded managed-doc edits redirect
	// to claim. Open-schema returns an empty result.
	if det, err := wiki.DetectUnrecordedEdits(*canonical); err == nil && len(det.UnrecordedDocs) > 0 {
		moePrintf(stderr, "%s\n", unrecordedEditsRedirect(workflow, det))
		return 1
	}
	findings, err := wiki.Scan(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki scan: %v\n", err)
		return 1
	}

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runSlug,
		DocID:       docID,
		LockPurpose: "lint",
		WikiBuilder: func(canonicalRoot string) (*wiki.Config, error) {
			return builder(canonicalRoot, projectID)
		},
		BuildSpec: func(workRoot string) (wikiTurnSpec, error) {
			return wikiTurnSpec{
				// No run metadata — lint sessions don't have a
				// per-document thread.jsonl to mirror into. Executor
				// skips the transcript copy when Metadata is nil.
				Metadata:         nil,
				DocID:            docID,
				ClonePath:        "",
				SessionUUID:      sessionUUID,
				NewSession:       true,
				InitialPrompt:    lintKickoff(findings),
				FinalizeRunID:    runSlug,
				FinalizeRunTitle: "Wiki lint pass",
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildLintSystemPrompt(worktreeWiki)
				},
				CommitStager: func(workRoot, wikiRel string) error {
					return commitLintTurn(workRoot, workflow, projectID, runSlug, wikiRel)
				},
			}, nil
		},
	}

	return runWikiSession(root, in, stdout, stderr)
}

// buildLintSystemPrompt assembles the lint session's
// --append-system-prompt: soul + LintPromptSection. Run-scoped
// extras (canvas, stage fragment, upstream banner) don't apply —
// lint has no run, no stage, no prerequisites.
func buildLintSystemPrompt(worktreeWiki *wiki.Config) (string, error) {
	if worktreeWiki == nil {
		return "", fmt.Errorf("lint: missing wiki config")
	}
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if ref := wiki.TwinReferenceSection(*worktreeWiki); ref != "" {
		sections = append(sections, ref)
	}
	sections = append(sections, wiki.LintPromptSection(*worktreeWiki))
	return strings.Join(sections, "\n---\n\n"), nil
}

// lintKickoff is the auto-sent first user message for a lint
// session. It frames the pass and inlines the structural pre-scan
// (orphans, broken links, empty docs) so the agent doesn't have to
// rediscover them. Semantic findings (overlap, breadth, taxonomy
// drift) come from the agent walking the corpus during the session.
func lintKickoff(findings wiki.Findings) string {
	var b strings.Builder
	b.WriteString("The operator just opened a wiki lint session. " +
		"Read the project's wiki under the content directory named in " +
		"your system prompt before replying. ")
	if findings.IsEmpty() {
		b.WriteString("The engine's structural pre-scan found nothing — no orphans, " +
			"broken links, or empty docs. Acknowledge in one sentence and propose " +
			"how you'd walk the corpus for semantic findings (overlap, breadth, " +
			"taxonomy drift), then wait for the operator's go-ahead.\n")
		return b.String()
	}
	b.WriteString("In one or two sentences, acknowledge the structural findings below " +
		"and propose how you'd walk through them with the operator — what to fix " +
		"inline, what to defer, what semantic checks (overlap, breadth, taxonomy " +
		"drift) you'd add. Then wait for the operator's go-ahead.\n\n")
	b.WriteString(wiki.RenderFindings(findings))
	return b.String()
}

// commitLintTurn stages the wiki dir and commits the per-turn
// changes with a lint-trailered message. No run.json, no doc dir —
// lint sessions are out-of-band relative to runs.
//
// ErrNothingToCommit propagates so the caller can report "no changes"
// without treating an untouched wiki as a failure.
func commitLintTurn(workRoot, workflow, projectID, runSlug, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	if err := run.Stage(workRoot, wikiRel); err != nil {
		return err
	}
	if !run.HasStagedChanges(workRoot) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf(`work: lint pass %s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: lint
`, runSlug, runSlug, projectID, workflow)
	return run.StageAndCommit(workRoot, msg, wikiRel)
}
