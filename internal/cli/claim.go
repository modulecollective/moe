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
	"github.com/modulecollective/moe/internal/wiki"
)

// claimCommand builds the `claim` facade for a workflow. Claim is
// closed-schema only and records context for managed-doc edits the
// operator made outside a reflect pass. It does NOT edit managed
// docs — the entry lands in log.md and the checkpoint advances so
// the next reflect sees a clean state.
func claimCommand(workflow string, builder func(root, projectID string) (*wiki.Config, error)) *Command {
	return &Command{
		Name:    "claim",
		Summary: "record context for decided edits made outside a reflect pass",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runClaimSession(workflow, builder, args, stdout, stderr)
		},
	}
}

func runClaimSession(workflow string, builder func(root, projectID string) (*wiki.Config, error), args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe workflow %s claim <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Record context for decided edits the operator made directly to managed docs.")
		moePrintln(stderr, "Out-of-band relative to runs. Bookkeeping only — does not edit managed docs.")
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
		moePrintf(stderr, "wiki: builder returned nil config; claim requires a registered wiki\n")
		return 1
	}
	if canonical.Mode != wiki.Closed {
		moePrintf(stderr, "wiki: claim is closed-schema only (%s is %s)\n", workflow, canonical.Mode)
		return 1
	}

	det, err := wiki.DetectUnrecordedEdits(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: detect: %v\n", err)
		return 1
	}
	if len(det.UnrecordedDocs) == 0 {
		moePrintf(stderr, "no unrecorded edits to managed docs — nothing to claim\n")
		return 0
	}

	diff, err := wiki.UnrecordedDiff(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: diff: %v\n", err)
		return 1
	}
	recentRun, recentCtx, err := loadRecentRunContext(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: recent run: %v\n", err)
		return 1
	}

	runSlug := "claim-" + time.Now().UTC().Format("2006-01-02-150405")
	docID := "claim"

	sessionUUID, err := run.NewSessionID()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runSlug,
		DocID:       docID,
		LockPurpose: "claim",
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
				InitialPrompt:    claimKickoff(det, diff, recentRun, recentCtx),
				FinalizeRunID:    runSlug,
				FinalizeRunTitle: claimRunTitle(recentRun),
				FinalizeClaim:    true,
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildClaimSystemPrompt(worktreeWiki)
				},
				CommitStager: func(workRoot, wikiRel string) error {
					return commitClaimTurn(workRoot, workflow, projectID, runSlug, wikiRel)
				},
			}, nil
		},
	}

	return runWikiSession(root, in, stdout, stderr)
}

func buildClaimSystemPrompt(worktreeWiki *wiki.Config) (string, error) {
	if worktreeWiki == nil {
		return "", fmt.Errorf("claim: missing wiki config")
	}
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if ref := wiki.TwinReferenceSection(*worktreeWiki); ref != "" {
		sections = append(sections, ref)
	}
	body, err := wiki.ClaimPromptSection(*worktreeWiki)
	if err != nil {
		return "", err
	}
	sections = append(sections, body)
	return strings.Join(sections, "\n---\n\n"), nil
}

func claimKickoff(det wiki.DetectionResult, diff string, recentRun *run.Metadata, recentCtx string) string {
	var b strings.Builder
	b.WriteString("The operator just opened a twin claim session. " +
		"Managed docs have been edited outside a reflect pass. Walk through what " +
		"changed with the operator and synthesise a log entry recording what changed, " +
		"why, and which run drove it. You do not edit managed docs in this session.\n\n")

	b.WriteString("## Unrecorded edits\n\n")
	docs := strings.Join(det.UnrecordedDocs, ", ")
	since := "the last log entry"
	if !det.Since.IsZero() {
		since = det.Since.Format("2006-01-02")
	}
	fmt.Fprintf(&b, "The following managed doc(s) have changes since %s: %s.\n\n", since, docs)
	if strings.TrimSpace(diff) != "" {
		b.WriteString("```diff\n")
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	if recentRun != nil && recentCtx != "" {
		fmt.Fprintf(&b, "## Recent run context — %s (%s)\n\n", recentRun.ID, recentRun.Title)
		b.WriteString("Verbatim workflow document content from the most recently closed run for this " +
			"project. Use it to pull motivation directly rather than asking the operator to retype it. " +
			"If the operator names a different run as the driver, ignore this block.\n\n")
		b.WriteString(recentCtx)
		if !strings.HasSuffix(recentCtx, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Acknowledge the diff in one or two sentences. Ask the operator for the *why* (and " +
		"the run id if any), then synthesise the log entry together. Once you and the operator agree, " +
		"append a short paragraph or bullet list to log.md under the heading the engine writes at " +
		"finalize. Don't touch managed docs.\n")
	return b.String()
}

func claimRunTitle(recentRun *run.Metadata) string {
	if recentRun == nil {
		return "Twin claim"
	}
	return fmt.Sprintf("Twin claim (driven by %s)", recentRun.ID)
}

func commitClaimTurn(workRoot, workflow, projectID, runSlug, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	if err := run.Stage(workRoot, wikiRel); err != nil {
		return err
	}
	if !run.HasStagedChanges(workRoot) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf(`work: claim pass %s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: claim
`, runSlug, runSlug, projectID, workflow)
	return run.StageAndCommit(workRoot, msg, wikiRel)
}

// loadRecentRunContext returns the most recently closed (or merged /
// promoted) bureaucracy run for the named project, and a verbatim
// concatenation of its workflow documents' content.md (design, plan,
// code…) lifted from disk. Returns (nil, "", nil) when there is no
// such run on disk; the kickoff degrades to "no recent context."
func loadRecentRunContext(root, projectID string) (*run.Metadata, string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, "", err
	}
	type candidate struct {
		md   *run.Metadata
		when time.Time
	}
	var cands []candidate
	for _, md := range mds {
		if md.Project != projectID {
			continue
		}
		switch md.Status {
		case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		default:
			continue
		}
		when, _ := run.LastActivity(root, md.ID)
		cands = append(cands, candidate{md: md, when: when})
	}
	if len(cands) == 0 {
		return nil, "", nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].when.After(cands[j].when) })
	picked := cands[0].md

	docDir := filepath.Join(root, "projects", projectID, "runs", picked.ID, "documents")
	entries, err := os.ReadDir(docDir)
	if err != nil {
		if os.IsNotExist(err) {
			return picked, "", nil
		}
		return picked, "", fmt.Errorf("read %s: %w", docDir, err)
	}
	var sections []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(docDir, e.Name(), "content.md"))
		if err != nil {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "### documents/%s/content.md\n\n", e.Name())
		b.WriteString(strings.TrimRight(string(body), "\n"))
		b.WriteString("\n")
		sections = append(sections, b.String())
	}
	return picked, strings.Join(sections, "\n"), nil
}
