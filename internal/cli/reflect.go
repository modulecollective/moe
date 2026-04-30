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

// reflectCommand builds the `reflect` facade for a workflow. Reflect
// is closed-schema only and out-of-band relative to the run ladder
// (no per-run canvas, no stage). Mirrors the lint facade: it lives
// alongside `new` / `close` as a workflow facade and reuses the
// workflow's wiki builder so reflect and ingest sessions agree on the
// wiki's identity and on-disk shape.
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
		moePrintf(stderr, "usage: moe workflow %s reflect <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code reflect session on the project's twin.")
		moePrintln(stderr, "Out-of-band relative to runs: no stage, no canvas, no run.json.")
		moePrintln(stderr, "Walks each managed doc against project commits and closed runs since the")
		moePrintln(stderr, "last reflect, and applies updates the operator agrees to.")
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

	runSlug := "reflect-" + time.Now().UTC().Format("2006-01-02-150405")
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
				InitialPrompt:    reflectKickoff(*canonical, events),
				FinalizeRunID:    runSlug,
				FinalizeRunTitle: "Twin reflect pass",
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildReflectSystemPrompt(worktreeWiki)
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

// reflectKickoff is the auto-sent first user message. It frames the
// pass, lays out the per-doc reflect prompts under a doc-named
// subhead, and inlines the "events since last reflect" block (commits
// + closed runs). Empty events → the agent walks the docs against the
// last-known state without a prompted set of changes, which is fine.
func reflectKickoff(cfg wiki.Config, events string) string {
	var b strings.Builder
	b.WriteString("The operator just opened a twin reflect session. " +
		"Walk each managed doc against recent project activity and propose updates " +
		"the operator agrees to. Vision is drift-only — flag gaps, don't rewrite.\n\n")

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

	if events != "" {
		b.WriteString(events)
	} else {
		b.WriteString("## Events since last reflect\n\n")
		b.WriteString("(no project commits or closed runs since the last checkpoint)\n\n")
	}

	b.WriteString("Acknowledge in one or two sentences which docs look most likely to need " +
		"updates and propose how you'd walk through them with the operator. Then wait for " +
		"the operator's go-ahead.\n")
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
	msg := fmt.Sprintf(`work: reflect pass %s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: reflect
`, runSlug, runSlug, projectID, workflow)
	return run.StageAndCommit(workRoot, msg, wikiRel)
}

// unrecordedEditsRedirect formats the one-line redirect printed when
// reflect/lint refuse to run because managed docs have been edited
// outside a reflect pass. Names the docs and points the operator at
// claim.
func unrecordedEditsRedirect(workflow string, det wiki.DetectionResult) string {
	docs := strings.Join(det.UnrecordedDocs, ", ")
	since := "the last log entry"
	if !det.Since.IsZero() {
		since = det.Since.Format("2006-01-02")
	}
	return fmt.Sprintf("unrecorded edits to %s since %s — run `moe workflow %s claim <project>` first",
		docs, since, workflow)
}
