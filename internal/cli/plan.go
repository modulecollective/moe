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

// planCommand builds the `plan` facade for a workflow. Plan is
// twin-only in practice — kb has no plan-shaped doc, and the wiki
// engine doesn't grow a plan primitive for open-schema. Plan opens
// an interactive session on roadmap.md, pre-loading the four other
// managed-doc bodies, recent project activity, and the open idea
// backlog into the kickoff so the agent can synthesise an ordering
// without shelling out for context.
func planCommand(workflow string, builder func(root, projectID string) (*wiki.Config, error)) *Command {
	return &Command{
		Name:    "plan",
		Summary: "open a Claude Code session on roadmap.md to propose / re-propose ordering",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runPlanSession(workflow, builder, args, stdout, stderr)
		},
	}
}

func runPlanSession(workflow string, builder func(root, projectID string) (*wiki.Config, error), args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflow+" plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s plan <project>\n", workflow)
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the project's roadmap.md.")
		moePrintln(stderr, "Out-of-band relative to runs: no stage, no canvas, no run.json — the")
		moePrintln(stderr, "session surfaces under the dash's TWIN rail, not in")
		moePrintln(stderr, "ACTIVE/BACKLOG/COMPLETED.")
		moePrintln(stderr, "Synthesises near / mid / long / parked from the other four twin docs,")
		moePrintln(stderr, "recent project activity, and the open idea backlog.")
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
		moePrintf(stderr, "wiki: builder returned nil config; plan requires a registered wiki\n")
		return 1
	}
	if canonical.Mode != wiki.Closed {
		moePrintf(stderr, "wiki: plan is closed-schema only (%s is %s)\n", workflow, canonical.Mode)
		return 1
	}

	// Decided-edit guardrail: plan, like reflect, refuses if the operator
	// has hand-edited managed docs since the last log entry. Claim those
	// first so the synthesis sees a consistent state.
	if det, err := wiki.DetectUnrecordedEdits(*canonical); err == nil && len(det.UnrecordedDocs) > 0 {
		moePrintf(stderr, "%s\n", unrecordedEditsRedirect(workflow, det))
		return 1
	}

	twinDocs, err := loadTwinDocBodies(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: %v\n", err)
		return 1
	}
	events, err := wiki.EventsSinceCheckpoint(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: events: %v\n", err)
		return 1
	}
	ideas, err := loadIdeaBacklog(root, projectID)
	if err != nil {
		moePrintf(stderr, "wiki: ideas: %v\n", err)
		return 1
	}
	roadmapBody, err := readRoadmap(*canonical)
	if err != nil {
		moePrintf(stderr, "wiki: roadmap: %v\n", err)
		return 1
	}

	runSlug := "plan-" + time.Now().UTC().Format("2006-01-02-150405")
	docID := "plan"

	sessionUUID, err := run.NewSessionID()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	in := wikiSessionInputs{
		Project:     projectID,
		RunSlug:     runSlug,
		DocID:       docID,
		LockPurpose: "plan",
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
				InitialPrompt:    planKickoff(twinDocs, events, ideas, roadmapBody),
				FinalizeRunID:    runSlug,
				FinalizeRunTitle: "Twin plan pass",
				BuildPrompt: func(workRoot string, worktreeWiki *wiki.Config) (string, error) {
					return buildPlanSystemPrompt(worktreeWiki)
				},
				CommitStager: func(workRoot, wikiRel string) error {
					return commitPlanTurn(workRoot, workflow, projectID, runSlug, wikiRel)
				},
			}, nil
		},
	}

	return runWikiSession(root, in, stdout, stderr)
}

func buildPlanSystemPrompt(worktreeWiki *wiki.Config) (string, error) {
	if worktreeWiki == nil {
		return "", fmt.Errorf("plan: missing wiki config")
	}
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if ref := wiki.TwinReferenceSection(*worktreeWiki); ref != "" {
		sections = append(sections, ref)
	}
	body, err := wiki.PlanPromptSection(*worktreeWiki)
	if err != nil {
		return "", err
	}
	sections = append(sections, body)
	return strings.Join(sections, "\n---\n\n"), nil
}

// loadTwinDocBodies returns a map of doc filename to its on-disk body
// for every managed doc except roadmap.md. Roadmap is excluded because
// it ships in its own kickoff section as the live target. Missing
// files are silently skipped — EnsureManagedDocs stubs them on session
// open, but the kickoff is built before the session opens, so a fresh
// twin's empty/missing inputs degrade to an absent section rather than
// a hard error.
func loadTwinDocBodies(cfg wiki.Config) (map[string]string, error) {
	out := map[string]string{}
	for _, d := range cfg.ManagedDocs {
		if d.Filename == "roadmap.md" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(cfg.ContentDir, d.Filename))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", d.Filename, err)
		}
		out[d.Filename] = string(body)
	}
	return out, nil
}

func readRoadmap(cfg wiki.Config) (string, error) {
	body, err := os.ReadFile(filepath.Join(cfg.ContentDir, "roadmap.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read roadmap.md: %w", err)
	}
	return string(body), nil
}

// ideaSummary captures the minimum needed to render an open idea into
// the plan kickoff: the slug, title, and verbatim canvas body.
type ideaSummary struct {
	slug  string
	title string
	body  string
}

// loadIdeaBacklog enumerates every open idea run for projectID and
// loads each canvas body. "Backlog" means StatusInProgress idea runs:
// closed/promoted ideas have already been spent. Sorted by slug for
// stable kickoff ordering across passes.
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

// planKickoff builds the auto-sent first user message. It frames the
// pass and inlines four context blocks: the current roadmap.md
// (target), the four other twin docs (synthesis context), the open
// idea backlog, and the events-since-checkpoint block reflect uses.
// Empty inputs degrade to a one-line "(none)" — the agent should
// still acknowledge and propose how to walk through with the
// operator.
func planKickoff(twinDocs map[string]string, events string, ideas []ideaSummary, roadmapBody string) string {
	var b strings.Builder
	b.WriteString("The operator just opened a twin plan session. " +
		"Synthesise an ordering for the project's roadmap.md against the four other " +
		"twin docs, the captured idea backlog, and recent project activity. Propose " +
		"near / mid / long term assignments and a parked list. Iterate with the " +
		"operator and edit roadmap.md directly.\n\n")

	b.WriteString("## Roadmap conventions\n\n")
	b.WriteString("roadmap.md uses four `##` sections by convention:\n")
	b.WriteString("- **Near term** — what's actively being lined up next.\n")
	b.WriteString("- **Mid term** — directionally agreed, not yet in flight.\n")
	b.WriteString("- **Long term** — over the horizon; intent without commitment.\n")
	b.WriteString("- **Parked** — considered and set aside, with a reason.\n\n")
	b.WriteString("On a fresh roadmap.md (just `# Roadmap` and nothing else), establish " +
		"the four sections at this pass. On subsequent passes, walk the prior content " +
		"with the operator and promote / demote / retire entries as agreed.\n\n")

	b.WriteString("## Current roadmap.md\n\n")
	if strings.TrimSpace(roadmapBody) == "" {
		b.WriteString("(empty stub — first plan session for this project)\n\n")
	} else {
		b.WriteString("```markdown\n")
		b.WriteString(strings.TrimRight(roadmapBody, "\n"))
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Other twin docs (synthesis context)\n\n")
	any := false
	for _, name := range []string{"vision.md", "architecture.md", "patterns.md", "operations.md"} {
		body, ok := twinDocs[name]
		if !ok {
			continue
		}
		any = true
		fmt.Fprintf(&b, "### %s\n\n", name)
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n\n")
	}
	if !any {
		b.WriteString("(no other twin docs on disk yet — fresh twin)\n\n")
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

	if events != "" {
		b.WriteString(events)
	} else {
		b.WriteString("## Events since last reflect\n\n")
		b.WriteString("(no project commits or closed runs since the last checkpoint)\n\n")
	}

	b.WriteString("Acknowledge in one or two sentences which way the synthesis points — " +
		"what stands out as obvious near-term, what looks parked, what's missing from " +
		"the inputs. Then wait for the operator's go-ahead before editing roadmap.md.\n")
	return b.String()
}

func commitPlanTurn(workRoot, workflow, projectID, runSlug, wikiRel string) error {
	if wikiRel == "" {
		return run.ErrNothingToCommit
	}
	if err := run.Stage(workRoot, wikiRel); err != nil {
		return err
	}
	if !run.HasStagedChanges(workRoot) {
		return run.ErrNothingToCommit
	}
	msg := fmt.Sprintf(`work: plan pass %s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: plan
`, runSlug, runSlug, projectID, workflow)
	return run.StageAndCommit(workRoot, msg, wikiRel)
}
