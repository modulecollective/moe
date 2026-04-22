package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

// newRunCommand returns a Command suitable for registering under a
// workflow as its `new` entry point (e.g., `moe sdlc new`, `moe kb
// new`). The workflow name is baked into the closure so each facade is
// a thin wrapper — all the real work (slug derivation, collision
// suffixing, git commit, next-stage hint) lives in runNew.
func newRunCommand(workflowName string) *Command {
	return &Command{
		Name:    "new",
		Summary: "open a new run in this workflow",
		Run: func(args []string, stdout, stderr io.Writer) int {
			return runNew(workflowName, args, stdout, stderr)
		},
	}
}

// runNew is the shared creator behind every workflow's `new` facade.
// It parses --id and --from-idea, loads the bureaucracy root, writes
// the run, commits it, and prints the first stage's invocation so the
// operator can move straight into work. The workflow is baked in via
// the caller — there is no --workflow flag here, because the workflow
// is implicit in which top-level command the operator typed (`moe sdlc
// new` vs `moe kb new`).
//
// --from-idea=<slug> promotes a captured idea into a fresh run: the
// idea's H1 becomes the run title (unless an explicit title is also
// passed, which wins), the idea body seeds the workflow's first-stage
// content.md, and the idea file is `git rm`d in the same commit so
// the transition is atomic in git history. Lives here rather than on
// each workflow facade so kb, sdlc, and any future workflow inherit
// the behavior for free.
func runNew(workflowName string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflowName+" new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	fromIdea := fs.String("from-idea", "", "promote projects/<project>/ideas/<slug>.md into a new run, seeding the first-stage doc")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--id <slug>] [--from-idea <slug>] <project> [\"title\"]\n", workflowName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	// Title is required normally, optional when --from-idea is set
	// (the idea's H1 supplies it).
	if fs.NArg() < 1 || (fs.NArg() < 2 && *fromIdea == "") {
		fs.Usage()
		return 2
	}
	project := fs.Arg(0)
	title := strings.Join(fs.Args()[1:], " ")

	// Sanity check against the workflow registry. The facade's caller
	// supplies a compile-time constant, so this should never fail in
	// practice — but if a workflow's `new` slot gets wired up before
	// the workflow itself is registered (e.g., init-order bug), we
	// catch it before writing state to disk.
	wf, err := LookupWorkflow(workflowName)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

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

	opts := run.Options{
		ID:       *idOverride,
		Workflow: workflowName,
	}
	if *fromIdea != "" {
		ideaTitle, ideaBody, err := loadIdeaForPromote(root, project, *fromIdea)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if title == "" {
			title = ideaTitle
		}
		stages := wf.Stages()
		if len(stages) == 0 {
			moePrintf(stderr, "workflow %q has no stages to seed from --from-idea\n", workflowName)
			return 1
		}
		opts.SeedDocs = map[string]string{stages[0]: ideaBody}
		opts.RemovePaths = []string{ideaPath(project, *fromIdea)}
		opts.SubjectFrom = "idea " + *fromIdea
		opts.ExtraTrailers = []string{"MoE-Idea: " + *fromIdea}
	}

	md, err := run.New(root, project, title, opts)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)
	return promptNextStage(root, md, stdout, stderr)
}

// loadIdeaForPromote reads the idea file and returns its title (the
// first H1 line, or the slug as fallback) and the full body to seed
// the first stage's canvas with. The whole file is the body — H1
// included — so the agent that opens the first stage starts on a
// canvas that already names what it's about.
func loadIdeaForPromote(root, projectID, slug string) (title, body string, err error) {
	rel := ideaPath(projectID, slug)
	b, readErr := os.ReadFile(filepath.Join(root, rel))
	if readErr != nil {
		return "", "", fmt.Errorf("--from-idea: read %s: %w", rel, readErr)
	}
	body = string(b)
	title = firstH1(body)
	if title == "" {
		title = slug
	}
	return title, body, nil
}
