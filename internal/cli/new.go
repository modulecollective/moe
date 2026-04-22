package cli

import (
	"flag"
	"io"
	"os"
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
// It parses --id, loads the bureaucracy root, writes the run, commits
// it, and prints the first stage's invocation so the operator can move
// straight into work. The workflow is bakes in via the caller — there
// is no --workflow flag here, because the workflow is implicit in
// which top-level command the operator typed (`moe sdlc new` vs future
// `moe kb new`).
func runNew(workflowName string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(workflowName+" new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new [--id <slug>] <project> \"title\"\n", workflowName)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	project := fs.Arg(0)
	// Join remaining args so an unquoted multi-word title still works.
	title := strings.Join(fs.Args()[1:], " ")

	// Sanity check against the workflow registry. The facade's caller
	// supplies a compile-time constant, so this should never fail in
	// practice — but if a workflow's `new` slot gets wired up before
	// the workflow itself is registered (e.g., init-order bug), we
	// catch it before writing state to disk.
	if _, err := LookupWorkflow(workflowName); err != nil {
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
	md, err := run.New(root, project, title, run.Options{
		ID:       *idOverride,
		Workflow: workflowName,
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened run %s/%s\n", md.Project, md.ID)

	// Print the first stage's invocation as a copy-pasteable hint. We
	// deliberately don't auto-launch it: every stage is one explicit
	// command, typed by the operator.
	if wf, err := LookupWorkflow(md.Workflow); err == nil {
		if stages := wf.Stages(); len(stages) > 0 {
			moePrintf(stdout, "next: moe %s %s %s %s\n", wf.Name, stages[0], md.Project, md.ID)
		}
	}
	return 0
}
