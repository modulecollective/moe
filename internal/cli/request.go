package cli

import (
	"flag"
	"io"
	"os"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/request"
)

func init() {
	Register(&Command{
		Name:    "request",
		Summary: "manage requests (subcommands: new)",
		Run:     runRequest,
	})
}

func runRequest(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		moePrintln(stdout, "usage: moe request <subcommand> [args...]")
		moePrintln(stdout, "subcommands: new")
		return 0
	}
	switch args[0] {
	case "new":
		return runRequestNew(args[1:], stdout, stderr)
	default:
		moePrintf(stderr, "unknown request subcommand %q\n", args[0])
		return 2
	}
}

func runRequestNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("request new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	workflow := fs.String("workflow", "sdlc", "workflow this request belongs to (sdlc is the only one today)")
	fs.Usage = func() {
		moePrintln(stderr, `usage: moe request new [--workflow <name>] [--id <slug>] <project> "title"`)
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

	// Validate against the workflow registry up front so a typo fails
	// before we write request.json — no orphaned state to clean up.
	if _, err := LookupWorkflow(*workflow); err != nil {
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
	md, err := request.New(root, project, title, request.Options{
		ID:       *idOverride,
		Workflow: *workflow,
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "opened request %s/%s\n", md.Project, md.ID)
	return promptNextStage(root, md, stdout, stderr)
}
