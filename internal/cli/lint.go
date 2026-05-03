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

// lintCommand builds the `lint` facade for an open-schema workflow.
// Lint is a standalone session — out-of-band relative to runs (no
// canvas, no stage), keyed off the wiki's content directory. The
// twin's hygiene sweep used to live here too; it now folds into
// `moe twin reflect`, so this surface is open-schema only (kb).
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
		moePrintf(stderr, "usage: moe %s lint <project>\n", workflow)
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

	canonical, err := builder(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: %v\n", err)
		return 1
	}
	if canonical == nil {
		moePrintf(stderr, "wiki: builder returned nil config; lint requires a registered wiki\n")
		return 1
	}
	if canonical.Mode != wiki.Open {
		moePrintf(stderr, "wiki: lint is open-schema only (%s is %s); closed-schema hygiene runs inside `moe twin reflect`\n",
			workflow, canonical.Mode)
		return 1
	}

	runSlug := "lint-" + time.Now().UTC().Format("2006-01-02-150405")
	docID := "lint"

	sessionUUID, err := run.NewSessionID()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
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
